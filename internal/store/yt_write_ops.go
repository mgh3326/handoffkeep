package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// YTWriteLedger is the Postgres implementation of ytwrite.Ledger: the op log
// behind hk's YouTrack write protocol (yt_write_ops, yt_entity_map). Every
// method is one transaction, and every state move is a compare-and-set on the
// current state, so concurrent callers and a second hk process cannot both win
// a step.
//
// Lock order: it holds at most one advisory lock per transaction, the entity
// lane lock (Prepare, Claim, and Apply of an issue_create, issue_move or
// decision_create op), and then rows of yt_write_ops and yt_entity_map (a
// scoped Claim also reads yt_comment_canary, yt_task_bindings or
// yt_decision_bindings; an issue_create Apply inserts a yt_task_bindings row
// and, in fields mode on, the task's yt_snapshot_state row and its first
// issue_fields op, an issue_move Apply updates the binding's project, and a
// decision_create Apply does as issue_create for the decision with
// yt_decision_bindings and decision_fields, and an article_create or
// article_update Apply writes the document's yt_article_bindings row and may
// prepare its article_update, all under the same lane lock). It takes no task
// row, no level-2 lock and no job lock, and no writer of another table waits on
// one of its rows, so it cannot close a cycle with them. It reads no clock:
// time is passed in (lockorder.go, "Time").
type YTWriteLedger struct {
	s *Store
	// scoped is the worker's ledger for one tenant (YTTenantLedger): Claim
	// and Due admit a prepared op only while the tenant is this one and its
	// kind's gate admits it (ytAdmitSQL); remote_unknown ops are listed for
	// this tenant only. The unscoped ledger admits every op (the protocol
	// core's contract suite, the operator's tools).
	scoped bool
	tenant string
}

// YTWriteLedger returns the Ledger over this store.
func (s *Store) YTWriteLedger() *YTWriteLedger { return &YTWriteLedger{s: s} }

// YTTenantLedger is the ledger the write worker of one YouTrack tenant
// dispatches from (slice b1a, docs/youtrack-write-protocol.md): every
// registered kind, each admitted by its own gate (ytAdmitSQL).
func (s *Store) YTTenantLedger(tenant string) *YTWriteLedger {
	return &YTWriteLedger{s: s, scoped: true, tenant: tenant}
}

// ytKindGates is the admission of a prepared op o of one kind, by kind: a SQL
// condition over o (the tenant is already checked), read with the store's
// current mode for that kind. A kind with no gate here admits nothing. Each
// kind adds its own entry from its own file's init (registerYTKindGate), so
// kinds added in parallel edit no shared list.
var ytKindGates = map[string]func(s *Store) string{}

// registerYTKindGate adds kind's gate. The kind is a ytwrite kind constant
// and is embedded in SQL as a literal, so it must be plain [a-z_].
func registerYTKindGate(kind string, gate func(s *Store) string) {
	if _, dup := ytKindGates[kind]; dup || !ytKindNameRE.MatchString(kind) {
		panic(fmt.Sprintf("store: yt kind gate %q registered twice or malformed", kind))
	}
	ytKindGates[kind] = gate
}

var ytKindNameRE = regexp.MustCompile(`^[a-z][a-z_]{0,63}$`)

// ytApplyHook is what applying an op of a create kind (issue_create,
// decision_create, article_create) or of article_update (which records the
// content its binding now holds) also does, in Apply's transaction under the entity's lane
// lock, which Apply takes first for such a kind: conflict names a mapping hk
// already keeps that publishing ytID would contradict ("" when none; then the
// op applies with reason binding_conflict and publishes nothing), and bind
// publishes the entity's binding after the map rows (and may prepare the
// entity's first snapshot behind the op).
type ytApplyHook struct {
	conflict func(ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string) (string, error)
	bind     func(s *Store, ctx context.Context, tx pgx.Tx, op ytwrite.Op, ytID string, now time.Time, adopted bool) error
}

// ytApplyHooks are the create kinds' apply hooks, by kind. Each kind adds its
// own from its own file's init (registerYTApplyHook).
var ytApplyHooks = map[string]ytApplyHook{}

func registerYTApplyHook(kind string, h ytApplyHook) {
	if _, dup := ytApplyHooks[kind]; dup || h.conflict == nil || h.bind == nil {
		panic(fmt.Sprintf("store: yt apply hook %q registered twice or incomplete", kind))
	}
	ytApplyHooks[kind] = h
}

// ytAdmitSQL is the admission of a prepared op o by the worker of the tenant
// named by the placeholder: o is bound to that tenant and the gate of o's kind
// admits it. Due and Claim share it, so what a pass lists is what a claim
// admits.
func (s *Store) ytAdmitSQL(tenant string) string {
	kinds := make([]string, 0, len(ytKindGates))
	for k := range ytKindGates {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	gates := make([]string, 0, len(kinds))
	for _, k := range kinds {
		gates = append(gates, `(o.kind = '`+k+`' AND (`+ytKindGates[k](s)+`))`)
	}
	if len(gates) == 0 {
		gates = append(gates, `FALSE`)
	}
	return `(o.tenant = ` + tenant + ` AND (` + strings.Join(gates, ` OR `) + `))`
}

var _ ytwrite.Ledger = (*YTWriteLedger)(nil)

const ytOpColumns = `id, principal, kind, idem_key, payload_sha, payload, entity_kind, entity_id, yt_target_id, actor,
	coalesce(parent_id,0), child_seq, state, yt_result_id, reason, last_error, attempts, reconciles,
	created_at, dispatched_at, next_attempt_at, resolved_at, tenant`

type ytOpScanner interface{ Scan(dest ...any) error }

func scanYTOp(r ytOpScanner) (ytwrite.Op, error) {
	var op ytwrite.Op
	var state string
	var dispatched, resolved *time.Time
	err := r.Scan(&op.ID, &op.Principal, &op.Kind, &op.Key, &op.PayloadSHA, &op.Payload, &op.EntityKind, &op.EntityID,
		&op.YTTargetID, &op.Actor, &op.ParentID, &op.ChildSeq, &state, &op.YTResultID, &op.Reason, &op.LastError,
		&op.Attempts, &op.Reconciles, &op.CreatedAt, &dispatched, &op.NextAttemptAt, &resolved, &op.Tenant)
	if err != nil {
		return ytwrite.Op{}, err
	}
	op.State = ytwrite.State(state)
	op.CreatedAt, op.NextAttemptAt = op.CreatedAt.UTC(), op.NextAttemptAt.UTC()
	if dispatched != nil {
		op.DispatchedAt = dispatched.UTC()
	}
	if resolved != nil {
		op.ResolvedAt = resolved.UTC()
	}
	return op, nil
}

// lockYTEntity serialises the writers of one entity's lane: the insert of a new
// op and the check that decides whether an op may be dispatched. Without it an
// op committed after a later op's check could be dispatched behind it.
func lockYTEntity(ctx context.Context, tx pgx.Tx, kind, id string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('yt_write_entity:' || $1 || ':' || $2, 0))`, kind, id)
	return err
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary so the column keeps valid UTF-8.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func (l *YTWriteLedger) Prepare(ctx context.Context, now time.Time, n ytwrite.NewOp) (ytwrite.Op, bool, error) {
	tx, err := l.s.pool.Begin(ctx)
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	defer tx.Rollback(ctx)
	op, created, err := prepareYTOpTx(ctx, tx, now, n)
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	return op, created, tx.Commit(ctx)
}

// prepareYTOpTx is Prepare inside the caller's transaction: the comment path
// stores its op in the comment's own transaction (yt_comments.go), so the
// comment and the op commit or roll back together.
func prepareYTOpTx(ctx context.Context, tx pgx.Tx, now time.Time, n ytwrite.NewOp) (ytwrite.Op, bool, error) {
	if err := lockYTEntity(ctx, tx, n.EntityKind, n.EntityID); err != nil {
		return ytwrite.Op{}, false, err
	}
	// Orders this op against an authority switch (hk 4122).
	if err := lockYTAuthorityForOpTx(ctx, tx, n.Principal); err != nil {
		return ytwrite.Op{}, false, err
	}
	var parent any
	if n.ParentID > 0 {
		parent = n.ParentID
	}
	row := tx.QueryRow(ctx, `INSERT INTO yt_write_ops(principal, kind, idem_key, payload_sha, payload, entity_kind, entity_id,
			yt_target_id, actor, parent_id, child_seq, state, created_at, next_attempt_at, tenant)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9,$10,$11,'prepared',$12,$12,$13)
		ON CONFLICT (principal, kind, idem_key) DO NOTHING
		RETURNING `+ytOpColumns,
		n.Principal, n.Kind, n.Key, n.PayloadSHA, string(n.Payload), n.EntityKind, n.EntityID, n.YTTargetID, n.Actor, parent, n.ChildSeq, now.UTC(), n.Tenant)
	op, err := scanYTOp(row)
	if err == nil {
		return op, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.Op{}, false, err
	}
	op, err = scanYTOp(tx.QueryRow(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND idem_key=$3`,
		n.Principal, n.Kind, n.Key))
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	if op.PayloadSHA != n.PayloadSHA {
		return ytwrite.Op{}, false, ytwrite.ErrKeyConflict
	}
	return op, false, nil
}

// rejectYTOpTx is Reject inside the caller's transaction, for an op refused
// before it was ever sent.
func rejectYTOpTx(ctx context.Context, tx pgx.Tx, now time.Time, id int64, reason, detail string) (ytwrite.Op, error) {
	return scanYTOp(tx.QueryRow(ctx, `UPDATE yt_write_ops SET state='rejected', reason=$2, last_error=$3, resolved_at=$4
		WHERE id=$1 AND state='prepared' RETURNING `+ytOpColumns, id, clipText(reason, 200), clipText(detail, 2000), now.UTC()))
}

func (l *YTWriteLedger) Get(ctx context.Context, principal string, id int64) (ytwrite.Op, error) {
	op, err := scanYTOp(l.s.pool.QueryRow(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=$1 AND principal=$2`, id, principal))
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.Op{}, ytwrite.ErrNotFound
	}
	return op, err
}

func (l *YTWriteLedger) ByID(ctx context.Context, id int64) (ytwrite.Op, error) {
	op, err := scanYTOp(l.s.pool.QueryRow(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.Op{}, ytwrite.ErrNotFound
	}
	return op, err
}

func (l *YTWriteLedger) Claim(ctx context.Context, now time.Time, id int64) (ytwrite.Op, ytwrite.ClaimOutcome, error) {
	tx, err := l.s.pool.Begin(ctx)
	if err != nil {
		return ytwrite.Op{}, "", err
	}
	defer tx.Rollback(ctx)
	// The lane is named by an immutable pair of columns, so it can be read
	// before it is locked.
	var kind, entity string
	if err := tx.QueryRow(ctx, `SELECT entity_kind, entity_id FROM yt_write_ops WHERE id=$1`, id).Scan(&kind, &entity); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ytwrite.Op{}, "", ytwrite.ErrNotFound
		}
		return ytwrite.Op{}, "", err
	}
	if err := lockYTEntity(ctx, tx, kind, entity); err != nil {
		return ytwrite.Op{}, "", err
	}
	op, err := scanYTOp(tx.QueryRow(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return ytwrite.Op{}, "", err
	}
	if op.State != ytwrite.StatePrepared {
		return op, ytwrite.ClaimNotPrepared, tx.Commit(ctx)
	}
	if op.NextAttemptAt.After(now) {
		return op, ytwrite.ClaimNotDue, tx.Commit(ctx)
	}
	if l.scoped {
		// Admission is read here, under the lane lock that the canary's
		// writers (AddYTCommentCanary, RemoveYTCommentCanary) and the
		// binding's writer (AddYTTaskBinding) also take: a removal either
		// commits before this read and holds the op, or waits for this claim
		// to commit.
		var admitted bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.id=$1 AND `+l.s.ytAdmitSQL("$2")+`)`, id, l.tenant).Scan(&admitted); err != nil {
			return ytwrite.Op{}, "", err
		}
		if !admitted {
			return op, ytwrite.ClaimHeld, tx.Commit(ctx)
		}
	}
	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM yt_write_ops
		WHERE entity_kind=$1 AND entity_id=$2 AND id < $3 AND state NOT IN ('applied','rejected'))`, kind, entity, id).Scan(&blocked); err != nil {
		return ytwrite.Op{}, "", err
	}
	if blocked {
		return op, ytwrite.ClaimBlocked, tx.Commit(ctx)
	}
	// A comment queued behind its task's issue_create gets its target here,
	// from the binding that create published, once the lane is clear
	// (hk 4263); with no such binding, or one a local source disagrees
	// with, it is rejected here, never dispatched. Every other op keeps its
	// own.
	target, sha, reject, detail, err := claimYTCommentTargetTx(ctx, tx, op)
	if err != nil {
		return ytwrite.Op{}, "", err
	}
	if reject != "" {
		op, err = rejectYTOpTx(ctx, tx, now, id, reject, detail)
		if err != nil {
			return ytwrite.Op{}, "", err
		}
		return op, ytwrite.ClaimNotPrepared, tx.Commit(ctx)
	}
	op, err = scanYTOp(tx.QueryRow(ctx, `UPDATE yt_write_ops SET state='dispatched', attempts=attempts+1, dispatched_at=$2,
		yt_target_id=$3, payload_sha=$4
		WHERE id=$1 RETURNING `+ytOpColumns, id, now.UTC(), target, sha))
	if err != nil {
		return ytwrite.Op{}, "", err
	}
	return op, ytwrite.ClaimClaimed, tx.Commit(ctx)
}

// moveYTOp runs one compare-and-set UPDATE (it must end with RETURNING
// ytOpColumns and constrain the current state). When it matches no row, the op
// is either missing or in a state the move does not allow; same, if set,
// accepts the op as it is when it already is the move's target.
func (l *YTWriteLedger) moveYTOp(ctx context.Context, what string, same ytwrite.State, query string, args ...any) (ytwrite.Op, error) {
	op, err := scanYTOp(l.s.pool.QueryRow(ctx, query, args...))
	if err == nil {
		return op, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.Op{}, err
	}
	cur, gerr := l.ByID(ctx, args[0].(int64))
	if gerr != nil {
		return ytwrite.Op{}, gerr
	}
	if same != "" && cur.State == same {
		return cur, nil
	}
	return ytwrite.Op{}, fmt.Errorf("%w: %s from %s", ytwrite.ErrState, what, cur.State)
}

func (l *YTWriteLedger) Apply(ctx context.Context, now time.Time, id int64, ytID string, maps []ytwrite.MapRow) (ytwrite.Op, error) {
	tx, err := l.s.pool.Begin(ctx)
	if err != nil {
		return ytwrite.Op{}, err
	}
	defer tx.Rollback(ctx)
	// A create kind's op (issue_create, decision_create) publishes its
	// entity's map row and binding, and an issue_move op its binding's
	// project, which the other paths and the feed read under the entity's
	// lane lock: take that lock first, as Claim does, before the op row. Kind
	// and lane are immutable, so they are read before the lock.
	var kind, entityKind, entityID string
	err = tx.QueryRow(ctx, `SELECT kind, entity_kind, entity_id FROM yt_write_ops WHERE id=$1`, id).Scan(&kind, &entityKind, &entityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.Op{}, ytwrite.ErrNotFound
	}
	if err != nil {
		return ytwrite.Op{}, err
	}
	hook, hooked := ytApplyHooks[kind]
	if hooked || kind == ytwrite.KindIssueMove {
		if err := lockYTEntity(ctx, tx, entityKind, entityID); err != nil {
			return ytwrite.Op{}, err
		}
	}
	op, err := scanYTOp(tx.QueryRow(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return ytwrite.Op{}, err
	}
	switch op.State {
	case ytwrite.StateApplied:
		if op.YTResultID == ytID {
			return op, nil
		}
		return ytwrite.Op{}, ytwrite.ErrConflictingResult
	case ytwrite.StateDispatched, ytwrite.StateRemoteUnknown:
	default:
		return ytwrite.Op{}, fmt.Errorf("%w: apply from %s", ytwrite.ErrState, op.State)
	}
	// A create kind (issue_create, decision_create) checks every mapping hk
	// keeps for the entity and the issue before it writes any: on a conflict
	// it publishes neither the map row nor the binding, keeps what is there,
	// and records the conflict.
	reason, detail := "", ""
	adopted := false
	for _, m := range maps {
		adopted = adopted || m.Adopted
	}
	if hooked {
		if detail, err = hook.conflict(ctx, tx, op, ytID); err != nil {
			return ytwrite.Op{}, err
		}
		if detail != "" {
			reason, maps = ytwrite.ReasonBindingConflict, nil
		}
	}
	for _, m := range maps {
		tag, err := tx.Exec(ctx, `INSERT INTO yt_entity_map(entity_kind, hk_key, yt_id, yt_parent_id, op_id, created_at)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (entity_kind, hk_key) DO NOTHING`,
			m.EntityKind, m.HKKey, m.YTID, m.YTParentID, id, now.UTC())
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ytwrite.Op{}, ytwrite.ErrConflictingResult // the yt id already maps to another hk key
		}
		if err != nil {
			return ytwrite.Op{}, err
		}
		if tag.RowsAffected() == 0 {
			var have string
			if err := tx.QueryRow(ctx, `SELECT yt_id FROM yt_entity_map WHERE entity_kind=$1 AND hk_key=$2`, m.EntityKind, m.HKKey).Scan(&have); err != nil {
				return ytwrite.Op{}, err
			}
			if have != m.YTID {
				return ytwrite.Op{}, ytwrite.ErrConflictingResult
			}
		}
	}
	if hooked && detail == "" {
		if err := hook.bind(l.s, ctx, tx, op, ytID, now, adopted); err != nil {
			return ytwrite.Op{}, err
		}
	}
	if op.Kind == ytwrite.KindIssueMove {
		if err := bindYTMoveTx(ctx, tx, op); err != nil {
			return ytwrite.Op{}, err
		}
	}
	op, err = scanYTOp(tx.QueryRow(ctx, `UPDATE yt_write_ops SET state='applied', yt_result_id=$2, reason=$4, last_error=$5, resolved_at=$3
		WHERE id=$1 RETURNING `+ytOpColumns, id, ytID, now.UTC(), reason, clipText(detail, 2000)))
	if err != nil {
		return ytwrite.Op{}, err
	}
	return op, tx.Commit(ctx)
}

func (l *YTWriteLedger) Reject(ctx context.Context, now time.Time, id int64, reason, detail string) (ytwrite.Op, error) {
	return l.moveYTOp(ctx, "reject", "", `UPDATE yt_write_ops SET state='rejected', reason=$2, last_error=$3, resolved_at=$4
		WHERE id=$1 AND state IN ('prepared','dispatched','remote_unknown') RETURNING `+ytOpColumns,
		id, clipText(reason, 200), clipText(detail, 2000), now.UTC())
}

func (l *YTWriteLedger) Defer(ctx context.Context, _ time.Time, id int64, next time.Time, reason, detail string) (ytwrite.Op, error) {
	return l.moveYTOp(ctx, "defer", "", `UPDATE yt_write_ops SET state='prepared', next_attempt_at=$2, reason=$3, last_error=$4
		WHERE id=$1 AND state='dispatched' RETURNING `+ytOpColumns,
		id, next.UTC(), clipText(reason, 200), clipText(detail, 2000))
}

func (l *YTWriteLedger) MarkUnknown(ctx context.Context, _ time.Time, id int64, next time.Time, reason, detail string) (ytwrite.Op, error) {
	return l.moveYTOp(ctx, "mark unknown", ytwrite.StateRemoteUnknown, `UPDATE yt_write_ops SET state='remote_unknown', next_attempt_at=$2, reason=$3, last_error=$4
		WHERE id=$1 AND state='dispatched' RETURNING `+ytOpColumns,
		id, next.UTC(), clipText(reason, 200), clipText(detail, 2000))
}

func (l *YTWriteLedger) RecoverStale(ctx context.Context, id int64, attempts int, cutoff, next time.Time, detail string) (bool, error) {
	tag, err := l.s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='remote_unknown', next_attempt_at=$4, reason=$5, last_error=$6
 WHERE id=$1 AND state='dispatched' AND attempts=$2 AND dispatched_at <= $3`,
		id, attempts, cutoff.UTC(), next.UTC(), ytwrite.ReasonDispatcherLost, clipText(detail, 2000))
	return tag.RowsAffected() == 1, err
}

func (l *YTWriteLedger) TouchUnknown(ctx context.Context, _ time.Time, id int64, next time.Time, reason, detail string) (ytwrite.Op, error) {
	return l.moveYTOp(ctx, "touch unknown", "", `UPDATE yt_write_ops SET reconciles=reconciles+1, next_attempt_at=$2, reason=$3, last_error=$4
		WHERE id=$1 AND state='remote_unknown' RETURNING `+ytOpColumns,
		id, next.UTC(), clipText(reason, 200), clipText(detail, 2000))
}

func (l *YTWriteLedger) list(ctx context.Context, query string, args ...any) ([]ytwrite.Op, error) {
	rows, err := l.s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ytwrite.Op
	for rows.Next() {
		op, err := scanYTOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func ytLimit(n int) int {
	if n <= 0 || n > 1000 {
		return 100
	}
	return n
}

func (l *YTWriteLedger) Due(ctx context.Context, now time.Time, limit int) ([]ytwrite.Op, error) {
	if l.scoped {
		// Held prepared ops (not admitted, or the mode is off) are left out
		// before the limit, so a long held queue cannot fill every pass;
		// uncertain ops of this tenant are always listed for their evidence
		// search.
		return l.list(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops o
			WHERE o.state IN ('prepared','remote_unknown') AND o.next_attempt_at <= $1
			AND ((o.state = 'remote_unknown' AND o.tenant = $3)
				OR (o.state = 'prepared' AND `+l.s.ytAdmitSQL("$3")+`
					AND NOT EXISTS (SELECT 1 FROM yt_write_ops e
						WHERE e.entity_kind = o.entity_kind AND e.entity_id = o.entity_id AND e.id < o.id AND e.state NOT IN ('applied','rejected'))))
			ORDER BY o.id LIMIT $2`, now.UTC(), ytLimit(limit), l.tenant)
	}
	return l.list(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops o
		WHERE o.state IN ('prepared','remote_unknown') AND o.next_attempt_at <= $1
		AND (o.state = 'remote_unknown' OR NOT EXISTS (SELECT 1 FROM yt_write_ops e
			WHERE e.entity_kind = o.entity_kind AND e.entity_id = o.entity_id AND e.id < o.id AND e.state NOT IN ('applied','rejected')))
		ORDER BY o.id LIMIT $2`, now.UTC(), ytLimit(limit))
}

func (l *YTWriteLedger) Stale(ctx context.Context, cutoff time.Time, limit int) ([]ytwrite.Op, error) {
	return l.list(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops
		WHERE state='dispatched' AND dispatched_at <= $1 ORDER BY id LIMIT $2`, cutoff.UTC(), ytLimit(limit))
}

func (l *YTWriteLedger) ListState(ctx context.Context, state ytwrite.State, limit int) ([]ytwrite.Op, error) {
	if !state.Valid() {
		return nil, fmt.Errorf("%w: unknown state %q", ytwrite.ErrInvalid, state)
	}
	return l.list(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE state=$1 ORDER BY id LIMIT $2`, string(state), ytLimit(limit))
}

func (l *YTWriteLedger) MapGet(ctx context.Context, entityKind, hkKey string) (ytwrite.MapRow, bool, error) {
	m := ytwrite.MapRow{EntityKind: entityKind, HKKey: hkKey}
	err := l.s.pool.QueryRow(ctx, `SELECT yt_id, yt_parent_id, op_id FROM yt_entity_map WHERE entity_kind=$1 AND hk_key=$2`,
		entityKind, hkKey).Scan(&m.YTID, &m.YTParentID, &m.OpID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.MapRow{}, false, nil
	}
	return m, err == nil, err
}

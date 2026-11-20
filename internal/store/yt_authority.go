package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The canonical side per content kind (hk 4122, docs/youtrack-write-protocol.md
// §Authority). Authority hk is today's behaviour exactly. While a kind is
// youtrack: `youtrack audit` repairs it from YouTrack to PG and enqueues
// nothing, and a write the protocol cannot send for it is refused instead of
// committed with a born-rejected op.

// YTAuthorityKind is one content kind the switch flips.
type YTAuthorityKind string

const (
	YTAuthorityTasks     YTAuthorityKind = "tasks"
	YTAuthorityComments  YTAuthorityKind = "comments"
	YTAuthorityDocuments YTAuthorityKind = "documents"
	YTAuthorityDecisions YTAuthorityKind = "decisions"
)

// YTAuthorityKinds is every kind, in the order show and set report them.
var YTAuthorityKinds = []YTAuthorityKind{YTAuthorityTasks, YTAuthorityComments, YTAuthorityDocuments, YTAuthorityDecisions}

// YTAuthoritySide is the canonical side of a kind.
type YTAuthoritySide string

const (
	// YTAuthorityHK: PG is canonical (the default, and the rollback).
	YTAuthorityHK YTAuthoritySide = "hk"
	// YTAuthorityYouTrack: YouTrack is canonical.
	YTAuthorityYouTrack YTAuthoritySide = "youtrack"
)

// YTAuthority is one kind's row. A kind never switched has no SwitchedAt.
type YTAuthority struct {
	Kind       YTAuthorityKind `json:"kind"`
	Authority  YTAuthoritySide `json:"authority"`
	SwitchedAt *time.Time      `json:"switched_at"`
	SwitchedBy string          `json:"switched_by"`
	Note       string          `json:"note"`
}

// ErrYTAuthorityRefused is a switch to youtrack whose preconditions do not
// hold: the kind's write mode is off, or the kind has unresolved ops. The
// text after "yt_authority_refused: " names the kind and the precondition.
var ErrYTAuthorityRefused = errors.New("yt_authority_refused")

// ErrYTCommentUnsendable is a comment the write protocol cannot send while
// comments are canonical in YouTrack: it is refused, not committed. The text
// after "yt_comment_unsendable: " is the protocol's refusal.
var ErrYTCommentUnsendable = errors.New("yt_comment_unsendable")

// ytAuthorityPrincipals are the op principals whose writes belong to each
// kind: the ops that must be resolved before the kind switches.
var ytAuthorityPrincipals = map[YTAuthorityKind][]string{
	YTAuthorityTasks:     {YTIssuePrincipal, YTFieldsPrincipal, YTLinksPrincipal},
	YTAuthorityComments:  {YTCommentPrincipal},
	YTAuthorityDocuments: {ytDocCommentPrincipal, YTDocsPrincipal},
	YTAuthorityDecisions: {YTDecisionsPrincipal},
}

// ytAuthorityFeeds are the change-feed consumers that prepare each kind's ops
// from sync_log, with their models: a switch waits until they have read every
// change (hk 4122), so no PG change older than the switch is sent after it.
var ytAuthorityFeeds = map[YTAuthorityKind][]struct {
	consumer string
	models   []string
}{
	YTAuthorityTasks:     {{ytFieldsConsumer, []string{"task"}}, {ytLinksConsumer, []string{"task"}}},
	YTAuthorityDocuments: {{ytDocCommentsConsumer, []string{"document"}}, {ytDocsConsumer, []string{"document"}}},
	YTAuthorityDecisions: {{ytDecisionsConsumer, []string{"decision"}}},
}

// ytAuthorityKindOf is the kind whose ops principal prepares ("" for a
// principal no kind owns, such as a test's).
func ytAuthorityKindOf(principal string) YTAuthorityKind {
	for k, ps := range ytAuthorityPrincipals {
		if slices.Contains(ps, principal) {
			return k
		}
	}
	return ""
}

// lockYTAuthorityForOpTx takes FOR SHARE on the authority row of the kind
// that owns principal, in the transaction that prepares the op
// (prepareYTOpTx). A switch (SetYTAuthority, FOR UPDATE) therefore waits for
// every op preparation in flight and counts it once committed, and a
// preparation that starts during a switch waits for it.
func lockYTAuthorityForOpTx(ctx context.Context, tx transaction, principal string) error {
	kind := ytAuthorityKindOf(principal)
	if kind == "" {
		return nil
	}
	_, err := ytAuthorityShareTx(ctx, tx, kind)
	return err
}

// ytAuthoritySyncLockAttempts and ytAuthoritySyncLockWait bound how long a
// switch tries for the legacy sync's lock: a comment catch-up pass holds it
// for a moment, a sync or adopt run for its whole run.
const (
	ytAuthoritySyncLockAttempts = 50
	ytAuthoritySyncLockWait     = 100 * time.Millisecond
)

// tryYTSyncLockTx tries the legacy sync's single-run lock (8241873404,
// TryYouTrackSyncLock) at transaction scope, a few times, never waiting on
// it. Held, no sync, adopt or catch-up pass can start until the switch
// commits; one that holds it already makes the switch refuse.
func tryYTSyncLockTx(ctx context.Context, tx pgx.Tx) (bool, error) {
	for attempt := 1; ; attempt++ {
		var held bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(8241873404)`).Scan(&held); err != nil || held {
			return held, err
		}
		if attempt >= ytAuthoritySyncLockAttempts {
			return false, nil
		}
		t := time.NewTimer(ytAuthoritySyncLockWait)
		select {
		case <-ctx.Done():
			t.Stop()
			return false, ctx.Err()
		case <-t.C:
		}
	}
}

func validYTAuthorityKind(k YTAuthorityKind) bool { return slices.Contains(YTAuthorityKinds, k) }

func validYTAuthoritySide(a YTAuthoritySide) bool {
	return a == YTAuthorityHK || a == YTAuthorityYouTrack
}

const ytAuthorityColumns = `kind, authority, switched_at, switched_by, note`

func scanYTAuthority(row interface{ Scan(...any) error }, a *YTAuthority) error {
	if err := row.Scan(&a.Kind, &a.Authority, &a.SwitchedAt, &a.SwitchedBy, &a.Note); err != nil {
		return err
	}
	if a.SwitchedAt != nil {
		at := a.SwitchedAt.UTC()
		a.SwitchedAt = &at
	}
	return nil
}

// ytAuthoritiesTx reads every kind's row, in YTAuthorityKinds order; a kind
// with no row is hk. lock is "" or a row-lock clause.
func ytAuthoritiesTx(ctx context.Context, q querier, lock string) ([]YTAuthority, error) {
	rows, err := q.Query(ctx, `SELECT `+ytAuthorityColumns+` FROM yt_authority ORDER BY kind`+lock)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKind := map[YTAuthorityKind]YTAuthority{}
	for rows.Next() {
		var a YTAuthority
		if err := scanYTAuthority(rows, &a); err != nil {
			return nil, err
		}
		byKind[a.Kind] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]YTAuthority, 0, len(YTAuthorityKinds))
	for _, k := range YTAuthorityKinds {
		a, ok := byKind[k]
		if !ok || !validYTAuthoritySide(a.Authority) {
			a = YTAuthority{Kind: k, Authority: YTAuthorityHK}
		}
		out = append(out, a)
	}
	return out, nil
}

// ytAuthorityTx reads one kind's side without a lock; no row is hk.
func ytAuthorityTx(ctx context.Context, q querier, kind YTAuthorityKind) (YTAuthoritySide, error) {
	var side YTAuthoritySide
	err := q.QueryRow(ctx, sqlFor(q, `SELECT authority FROM yt_authority WHERE kind=$1`, `SELECT authority FROM yt_authority WHERE kind=?`), string(kind)).Scan(&side)
	if errors.Is(err, ErrNoRows) || (err == nil && !validYTAuthoritySide(side)) {
		return YTAuthorityHK, nil
	}
	return side, err
}

// ytAuthorityShareTx reads one kind's side FOR SHARE in a writer's
// transaction (the comment writers, the audit enqueue, every op
// preparation), so a switch waits for the writer in flight and the writer sees
// a switch that committed first. A missing row is created first (as hk, ON
// CONFLICT DO NOTHING), so there is always a row to lock: a switch that
// creates the same row waits for this insert, or this insert for the switch's.
func ytAuthorityShareTx(ctx context.Context, tx transaction, kind YTAuthorityKind) (YTAuthoritySide, error) {
	var side YTAuthoritySide
	err := tx.QueryRow(ctx, `SELECT authority FROM yt_authority WHERE kind=`+sqlFor(tx, `$1`, `?`)+` FOR SHARE`, string(kind)).Scan(&side)
	if errors.Is(err, ErrNoRows) {
		if _, err := tx.Exec(ctx, sqlFor(tx,
			`INSERT INTO yt_authority(kind) VALUES ($1) ON CONFLICT DO NOTHING`,
			`INSERT INTO yt_authority(kind) VALUES (?) ON DUPLICATE KEY UPDATE kind=kind`), string(kind)); err != nil {
			return "", err
		}
		err = tx.QueryRow(ctx, `SELECT authority FROM yt_authority WHERE kind=`+sqlFor(tx, `$1`, `?`)+` FOR SHARE`, string(kind)).Scan(&side)
	}
	if err != nil {
		return "", err
	}
	if !validYTAuthoritySide(side) {
		return YTAuthorityHK, nil
	}
	return side, nil
}

// YTAuthorities reads every kind's row.
func (s *Store) YTAuthorities(ctx context.Context) ([]YTAuthority, error) {
	return ytAuthoritiesTx(ctx, s.db(), "")
}

// YTAuthorityOf reads one kind's side; an unknown kind is hk.
func (s *Store) YTAuthorityOf(ctx context.Context, kind YTAuthorityKind) (YTAuthoritySide, error) {
	return ytAuthorityTx(ctx, s.db(), kind)
}

// ytAuthorityWriteModeOff explains why kind's write mode in this process is
// not on, or returns "" when it is. Tasks need the issue, the field and the link
// copies.
func (s *Store) ytAuthorityWriteModeOff(kind YTAuthorityKind) string {
	switch kind {
	case YTAuthorityTasks:
		if m, _ := s.ytIssuesNow(); m != YTIssuesOn {
			return "HANDOFFKEEP_YT_ISSUES_MODE is not on"
		}
		if m, _ := s.ytFieldsNow(); m != YTFieldsOn {
			return "HANDOFFKEEP_YT_FIELDS_MODE is not on"
		}
		if m, _ := s.ytLinksNow(); m != YTLinksOn {
			return "HANDOFFKEEP_YT_LINKS_MODE is not on"
		}
		return ""
	case YTAuthorityComments:
		if s.ytCommentsModeNow() != YTCommentsOn {
			return "HANDOFFKEEP_YT_COMMENTS_MODE is not on"
		}
		return ""
	case YTAuthorityDocuments:
		if s.ytDocCommentsNow() != YTDocCommentsOn {
			return "HANDOFFKEEP_YT_DOC_COMMENTS_MODE is not on"
		}
		// The articles (hk 4206): a task's documents reach its issue as
		// comments, every other document its article.
		if _, on := s.ytDocsNow(); !on {
			return "HANDOFFKEEP_YT_DOCS_MODE is not on"
		}
		return ""
	case YTAuthorityDecisions:
		if _, on := s.ytDecisionsNow(); !on {
			return "HANDOFFKEEP_YT_DECISIONS_MODE is not on"
		}
		return ""
	default:
		return "this release has no write mode for " + string(kind)
	}
}

// YTAuthoritySetMax bounds the operator's note.
const YTAuthoritySetMax = 2000

// SetYTAuthority switches kinds to side in one transaction and returns every
// kind's row before and after. A kind already on side is left as it is
// (its switched_at, switched_by and note are kept). A switch to youtrack is
// ErrYTAuthorityRefused unless no legacy sync, adopt or comment catch-up runs
// and, for each kind it changes, this process's write mode for the kind is
// on, the kind has no prepared, dispatched or remote_unknown op, and its feed
// consumers have read every change; if any kind is refused, none changes. A
// switch to hk always succeeds: it is the rollback.
//
// Locks (lockorder.go): a switch to youtrack first tries the sync lock
// (tryYTSyncLockTx, never waits); then FOR UPDATE on the yt_authority rows
// in kind order, which waits for every op preparation and comment in flight
// (they hold the row FOR SHARE); then plain reads of yt_write_ops,
// yt_feed_cursors and sync_log.
func (s *Store) SetYTAuthority(ctx context.Context, kinds []YTAuthorityKind, side YTAuthoritySide, by, note string) (before, after []YTAuthority, err error) {
	if len(kinds) == 0 || !validYTAuthoritySide(side) || strings.TrimSpace(by) == "" || !ValidActor(by) ||
		strings.TrimSpace(note) == "" || !validText(note, YTAuthoritySetMax) {
		return nil, nil, errors.New("invalid authority set: want kinds, hk or youtrack, --by and --note")
	}
	seen := map[YTAuthorityKind]bool{}
	for _, k := range kinds {
		if !validYTAuthorityKind(k) || seen[k] {
			return nil, nil, fmt.Errorf("invalid authority set: unknown or repeated kind %q", k)
		}
		seen[k] = true
	}
	if s.dialect == mysqlDialect && side == YTAuthorityYouTrack {
		// MySQL has no transaction-scoped advisory try-lock: the switch to
		// youtrack runs under the named sync lock on one connection (design
		// §11.1), retried like tryYTSyncLockTx before it refuses. A rollback
		// to hk takes no sync lock on either dialect.
		for attempt := 1; ; attempt++ {
			held, err := s.mysqlWriteTransactionWithLock(ctx, mysqlYTSyncLock, func(tx transaction) error {
				var e error
				before, after, e = s.setYTAuthorityTx(ctx, tx, seen, side, by, note)
				return e
			})
			if err != nil || held {
				return before, after, err
			}
			if attempt >= ytAuthoritySyncLockAttempts {
				return nil, nil, fmt.Errorf("%w: a youtrack sync, adopt or comment catch-up holds the sync lock; try again when it ends", ErrYTAuthorityRefused)
			}
			t := time.NewTimer(ytAuthoritySyncLockWait)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, nil, ctx.Err()
			case <-t.C:
			}
		}
	}
	// A rollback to hk reaches here on either dialect and takes no sync lock,
	// as on PostgreSQL.
	err = s.retryTx(ctx, func() error {
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if side == YTAuthorityYouTrack {
			held, err := tryYTSyncLockTx(ctx, pgxTx(tx))
			if err != nil {
				return err
			}
			if !held {
				return fmt.Errorf("%w: a youtrack sync, adopt or comment catch-up holds the sync lock; try again when it ends", ErrYTAuthorityRefused)
			}
		}
		before, after, err = s.setYTAuthorityTx(ctx, tx, seen, side, by, note)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		for _, k := range YTAuthorityKinds {
			if seen[k] {
				var changed bool
				for _, a := range before {
					if a.Kind == k && a.Authority != side {
						changed = true
					}
				}
				if changed {
					log.Printf("ytwrite.authority_set kind=%s authority=%s switched_by=%q note=%q", k, side, by, note)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

// setYTAuthorityTx is the body of a switch in one transaction (see
// SetYTAuthority for the contract and the lock order). On MySQL a switch that
// would change tasks or documents refuses: their preconditions read tables B4b
// and B5 do not port yet.
func (s *Store) setYTAuthorityTx(ctx context.Context, tx transaction, seen map[YTAuthorityKind]bool, side YTAuthoritySide, by, note string) (before, after []YTAuthority, err error) {
	var change []YTAuthorityKind
	// A row a database lost (or never seeded) is hk; give it a row to lock.
	if dialectFor(tx) == mysqlDialect {
		if _, err := tx.Exec(ctx, `INSERT INTO yt_authority(kind) VALUES ('tasks'),('comments'),('documents'),('decisions') ON DUPLICATE KEY UPDATE kind=kind`); err != nil {
			return nil, nil, err
		}
	} else {
		if _, err := tx.Exec(ctx, `INSERT INTO yt_authority(kind) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING`, ytAuthorityKindStrings(YTAuthorityKinds)); err != nil {
			return nil, nil, err
		}
	}
	before, err = ytAuthoritiesTx(ctx, tx, " FOR UPDATE")
	if err != nil {
		return nil, nil, err
	}
	for _, a := range before {
		if seen[a.Kind] && a.Authority != side {
			change = append(change, a.Kind)
		}
	}
	if side == YTAuthorityYouTrack {
		var refusals []string
		for _, k := range change {
			if dialectFor(tx) == mysqlDialect && (k == YTAuthorityTasks || k == YTAuthorityDocuments) {
				refusals = append(refusals, string(k)+": the "+string(k)+" switch reads areas MySQL does not port yet")
				continue
			}
			if off := s.ytAuthorityWriteModeOff(k); off != "" {
				refusals = append(refusals, string(k)+": "+off)
				continue
			}
			var pending int
			if err := tx.QueryRow(ctx, s.q(`SELECT count(*) FROM yt_write_ops WHERE principal = ANY($1)
				AND state IN ('prepared','dispatched','remote_unknown')`,
				`SELECT count(*) FROM yt_write_ops WHERE principal IN (`+placeholders(len(ytAuthorityPrincipals[k]))+`)
				AND state IN ('prepared','dispatched','remote_unknown')`), ytInArgs(s, ytAuthorityPrincipals[k])...).Scan(&pending); err != nil {
				return nil, nil, err
			}
			if pending > 0 {
				refusals = append(refusals, fmt.Sprintf("%s: %d ops are prepared, dispatched or remote_unknown (youtrack ops)", k, pending))
			}
			if k == YTAuthorityTasks {
				// The links sweep (hk 3841, I15): a link change the feed
				// consumed while the mode was off, or before its cursor
				// existed, reaches YouTrack only through the sweep, and no
				// feed cursor counts it. An incomplete sweep means such a
				// change may still be unsent. A plain read, like the feed
				// check below: the sweep row's writers hold no authority row.
				sw, err := ytLinksSweepStateTx(ctx, tx)
				if err != nil {
					return nil, nil, err
				}
				if !sw.Complete() {
					refusals = append(refusals, fmt.Sprintf("%s: the links sweep is incomplete (generation %d, complete through %d)", k, sw.Generation, sw.DoneGeneration))
				}
			}
			if k == YTAuthorityDocuments {
				// The document articles (hk 4206): no in-scope document may
				// hold content YouTrack lacks with no op writing it, which
				// covers what a feed cursor cannot (changes before it, a
				// binding published since, a create that was refused).
				g, _ := s.ytDocsNow()
				n, ids, waiting, err := ytArticlesUnwrittenTx(ctx, tx, g.tenant, g.split)
				if err != nil {
					return nil, nil, err
				}
				if n > 0 {
					refusals = append(refusals, fmt.Sprintf("%s: %d documents have content YouTrack lacks and no op writing it (%v; %s: %d)", k, n, ids, YTArticleWaitingFamilyRoot, waiting))
				}
			}
			for _, f := range ytAuthorityFeeds[k] {
				// One statement, so the prune boundary, the cursor and the
				// backlog come from one snapshot, as runYTFeedConsumer reads
				// them: a prune that commits in between cannot hide unread
				// rows.
				var prunedThrough int64
				var cursor *int64
				var behind int
				var feedArgs []any
				if s.dialect == mysqlDialect {
					feedArgs = append(ytInArgs(s, f.models), f.consumer)
				} else {
					feedArgs = append([]any{f.consumer}, ytInArgs(s, f.models)...)
				}
				if err := tx.QueryRow(ctx, s.q(`SELECT st.pruned_through, c.last_sync_id,
					(SELECT count(*) FROM sync_log l WHERE l.sync_id > coalesce(c.last_sync_id, 0) AND l.model = ANY($2::text[]))
					FROM sync_state st LEFT JOIN yt_feed_cursors c ON c.consumer = $1
					WHERE st.singleton`,
					`SELECT st.pruned_through, c.last_sync_id,
					(SELECT count(*) FROM sync_log l WHERE l.sync_id > coalesce(c.last_sync_id, 0) AND l.model IN (`+placeholders(len(f.models))+`))
					FROM sync_state st LEFT JOIN yt_feed_cursors c ON c.consumer = ?
					WHERE st.singleton`), feedArgs...).Scan(&prunedThrough, &cursor, &behind); err != nil {
					return nil, nil, err
				}
				if cursor == nil {
					refusals = append(refusals, fmt.Sprintf("%s: feed consumer %s has not run yet", k, f.consumer))
					continue
				}
				// Rows the prune removed before the consumer read them cannot
				// be counted; the feed itself stops on them.
				if prunedThrough > *cursor {
					refusals = append(refusals, fmt.Sprintf("%s: feed consumer %s is behind the sync_log prune (cursor %d, pruned through %d)", k, f.consumer, *cursor, prunedThrough))
					continue
				}
				if behind > 0 {
					refusals = append(refusals, fmt.Sprintf("%s: feed consumer %s has %d changes to read", k, f.consumer, behind))
				}
			}
		}
		if len(refusals) > 0 {
			return nil, nil, fmt.Errorf("%w: %s", ErrYTAuthorityRefused, strings.Join(refusals, "; "))
		}
	}
	if len(change) > 0 {
		var updArgs []any
		var updSQL string
		if s.dialect == mysqlDialect {
			updSQL = `UPDATE yt_authority SET authority=?, switched_at=?, switched_by=?, note=? WHERE kind IN (` + placeholders(len(change)) + `)`
			updArgs = append([]any{string(side), time.Now().UTC(), by, note}, ytInArgs(s, ytAuthorityKindStrings(change))...)
		} else {
			updSQL = `UPDATE yt_authority SET authority=$2, switched_at=$3, switched_by=$4, note=$5 WHERE kind = ANY($1)`
			updArgs = []any{ytAuthorityKindStrings(change), string(side), time.Now().UTC(), by, note}
		}
		if _, err := tx.Exec(ctx, updSQL, updArgs...); err != nil {
			return nil, nil, err
		}
	}
	after, err = ytAuthoritiesTx(ctx, tx, "")
	return before, after, err
}

// ytInArgs is a5Int64Args for string slices: one slice on PostgreSQL (=
// ANY($n)), one value each on MySQL (IN (...)).
func ytInArgs(s *Store, in []string) []any {
	if s.dialect != mysqlDialect {
		return []any{in}
	}
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func ytAuthorityKindStrings(kinds []YTAuthorityKind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

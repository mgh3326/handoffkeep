package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The YouTrack change-feed tracker (hk 3997, Y2b). The YouTrack projections
// of task fields, documents and decisions must see every change to those
// rows, and tasks alone have more than ten writers: an enqueue in each writer
// would miss one. The sync_log row triggers (sync_migration.go) already
// journal every insert, update and delete of tasks, documents, task_comments
// and sync_decisions in the writer's own transaction, and the commit-time
// allocator gives the committed rows ordered sync_ids. A feed consumer reads
// that log through its own cursor (yt_feed_cursors) and prepares its ops in
// the same transaction that advances the cursor, so a change is handled once
// or, on any error, not at all and read again.
//
// A consumer registers itself from its own file's init with
// registerYTFeedConsumer, so consumers added in parallel edit no shared list.
// The YouTrack write worker runs every registered consumer once per pass
// (RunYTFeedConsumers), after the comment catch-up.

// YTFeedPassMax bounds the sync_log rows one consumer reads in one pass.
const YTFeedPassMax = 500

// ytFeedLockID is the feed pass lock, pg_try_advisory_xact_lock, the first
// lock of every pass's transaction (lockorder.go). One pass runs at a time
// across processes, so a handler is the only transaction that may hold more
// than one entity lane lock.
const ytFeedLockID = 8241873997

// FeedRow is one sync_log row as a consumer sees it: the change of one model
// row (task, document, task_comment or decision) and its action, I, U or D.
// ModelID is the row's id as text (a decision's is its request id).
type FeedRow struct {
	SyncID  int64
	Model   string
	ModelID string
	Action  string
	At      time.Time
}

// FeedHandler prepares a consumer's ops for rows, in the pass's transaction
// tx: they commit with the cursor advance, and an error rolls both back. rows
// is in sync_id order with at most one row per (model, model_id), the last of
// the batch, and never empty. A handler reads the model rows without a lock
// and writes only through the YouTrack op log (prepareYTOpTx,
// prepareYTSnapshotTx): it takes no task row and no lock of levels 2-5
// (lockorder.go).
type FeedHandler func(ctx context.Context, tx pgx.Tx, rows []FeedRow) error

type ytFeedConsumer struct {
	name   string
	models []string
	handle FeedHandler
	// build, when set, makes the pass's handler from the store and the
	// pass's time (registerYTFeedStoreConsumer); handle is then unused.
	build func(s *Store, now time.Time) FeedHandler
}

// handler is c's handler for one pass of s at now.
func (c ytFeedConsumer) handler(s *Store, now time.Time) FeedHandler {
	if c.build != nil {
		return c.build(s, now)
	}
	return c.handle
}

// ytFeedConsumers is the registry, by name: each consumer (Y4b task fields
// in yt_fields.go, Y5b documents, Y6 decisions) adds its own entry.
var ytFeedConsumers = map[string]ytFeedConsumer{}

var ytFeedConsumerNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ytFeedModels are the sync_log models (the CHECK on sync_log.model).
var ytFeedModels = map[string]bool{"task": true, "document": true, "task_comment": true, "decision": true}

// registerYTFeedConsumer adds a consumer named name (its cursor's key, plain
// [a-z0-9_]) that reads the changes of models. It panics on a duplicate name,
// an unknown or repeated model, or a nil handler: a registry mistake fails
// at startup.
func registerYTFeedConsumer(name string, models []string, handle FeedHandler) {
	c, err := newYTFeedConsumer(name, models, handle)
	if err != nil {
		panic("store: " + err.Error())
	}
	if _, dup := ytFeedConsumers[name]; dup {
		panic(fmt.Sprintf("store: yt feed consumer %q registered twice", name))
	}
	ytFeedConsumers[name] = c
}

// registerYTFeedStoreConsumer is registerYTFeedConsumer for a consumer whose
// handler needs the store (its mode, its settings) or the pass's time: build
// makes the handler at the start of each pass's handling.
func registerYTFeedStoreConsumer(name string, models []string, build func(s *Store, now time.Time) FeedHandler) {
	if build == nil {
		panic(fmt.Sprintf("store: yt feed consumer %q has no handler", name))
	}
	c, err := newYTFeedConsumer(name, models, func(context.Context, pgx.Tx, []FeedRow) error { return nil })
	if err != nil {
		panic("store: " + err.Error())
	}
	if _, dup := ytFeedConsumers[name]; dup {
		panic(fmt.Sprintf("store: yt feed consumer %q registered twice", name))
	}
	c.build = build
	ytFeedConsumers[name] = c
}

func newYTFeedConsumer(name string, models []string, handle FeedHandler) (ytFeedConsumer, error) {
	if !ytFeedConsumerNameRE.MatchString(name) || handle == nil || len(models) == 0 {
		return ytFeedConsumer{}, fmt.Errorf("yt feed consumer %q: malformed name, no models or no handler", name)
	}
	seen := map[string]bool{}
	for _, m := range models {
		if !ytFeedModels[m] || seen[m] {
			return ytFeedConsumer{}, fmt.Errorf("yt feed consumer %q: unknown or repeated model %q", name, m)
		}
		seen[m] = true
	}
	return ytFeedConsumer{name: name, models: append([]string(nil), models...), handle: handle}, nil
}

// YTFeedPass is what one consumer's pass did. Created: the cursor was created
// in this pass, at the feed's max_sync_id. Busy: another pass held the feed
// lock, nothing was read. Pruned: the log was pruned beyond the cursor
// (PrunedThrough > Cursor), nothing was read. Read counts the rows read,
// Handled the rows given to the handler after collapsing repeats; Cursor is
// the cursor after the pass.
type YTFeedPass struct {
	Consumer      string
	Created       bool
	Busy          bool
	Pruned        bool
	PrunedThrough int64
	Read          int
	Handled       int
	Cursor        int64
}

// RunYTFeedConsumers runs one pass of every registered consumer, in name
// order, each reading up to limit rows (at most YTFeedPassMax). A consumer
// whose pass fails is logged and does not stop the others; the first error
// is returned with every pass that ran. With no consumer registered it does
// nothing.
func (s *Store) RunYTFeedConsumers(ctx context.Context, limit int) ([]YTFeedPass, error) {
	names := make([]string, 0, len(ytFeedConsumers))
	for name := range ytFeedConsumers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]YTFeedPass, 0, len(names))
	var first error
	for _, name := range names {
		pass, err := s.runYTFeedConsumer(ctx, ytFeedConsumers[name], limit, time.Now().UTC())
		if err != nil {
			log.Printf("ytwrite.feed_failed consumer=%q err=%q", name, err.Error())
			if first == nil {
				first = err
			}
			continue
		}
		out = append(out, pass)
	}
	return out, first
}

// runYTFeedConsumer is one pass of c in one transaction: the feed lock (only
// tried), c's cursor row (created at max_sync_id on the first pass), then up
// to limit rows of c's models above the cursor in sync_id order, then the
// handler, then the cursor advance: to the last sync_id read after a full
// page, to the snapshot's max_sync_id after a short one. A handler error
// rolls the pass back and leaves the cursor where it was, so the same rows
// come again. A log pruned beyond the cursor is never skipped: the pass
// reads nothing, logs ytwrite.feed_pruned and records one alert per cursor
// value (the YouTrack op alert's outbox, which `yt ops` problems reach the
// design lane through); the cursor stays until an operator or the backfill
// (Y3) moves it.
func (s *Store) runYTFeedConsumer(ctx context.Context, c ytFeedConsumer, limit int, now time.Time) (YTFeedPass, error) {
	pass := YTFeedPass{Consumer: c.name}
	if limit < 1 {
		return pass, nil
	}
	if limit > YTFeedPassMax {
		limit = YTFeedPassMax
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return pass, err
	}
	defer tx.Rollback(ctx)
	var held bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(ytFeedLockID)).Scan(&held); err != nil {
		return pass, err
	}
	if !held {
		pass.Busy = true
		return pass, nil
	}
	err = tx.QueryRow(ctx, `INSERT INTO yt_feed_cursors(consumer,last_sync_id,updated_at)
		SELECT $1,max_sync_id,$2 FROM sync_state WHERE singleton
		ON CONFLICT (consumer) DO NOTHING RETURNING last_sync_id`, c.name, now).Scan(&pass.Cursor)
	switch {
	case err == nil:
		pass.Created = true
		// History before the cursor is the backfill's (Y3), not the feed's.
		log.Printf("ytwrite.feed_cursor_created consumer=%q last_sync_id=%d", c.name, pass.Cursor)
	case !errors.Is(err, pgx.ErrNoRows):
		return pass, err
	default:
		if err := tx.QueryRow(ctx, `SELECT last_sync_id FROM yt_feed_cursors WHERE consumer=$1 FOR UPDATE`, c.name).Scan(&pass.Cursor); err != nil {
			return pass, err
		}
	}
	// One statement, so pruned_through, max_sync_id and the rows come from
	// one snapshot: a prune that commits later cannot have removed a row this
	// read skipped, and one that committed earlier shows in pruned_through,
	// which stops the read.
	rows, err := tx.Query(ctx, `SELECT st.pruned_through, st.max_sync_id, l.sync_id, l.model, l.model_id, l.action, l.at
		FROM sync_state st
		LEFT JOIN LATERAL (SELECT sync_id, model, model_id, action, at FROM sync_log
			WHERE st.pruned_through <= $1 AND sync_id > $1 AND model = ANY($2::text[])
			ORDER BY sync_id LIMIT $3) l ON true
		WHERE st.singleton
		ORDER BY l.sync_id`, pass.Cursor, c.models, limit)
	if err != nil {
		return pass, err
	}
	read := []FeedRow{}
	var maxSyncID int64
	for rows.Next() {
		var syncID *int64
		var model, modelID, action *string
		var at *time.Time
		if err := rows.Scan(&pass.PrunedThrough, &maxSyncID, &syncID, &model, &modelID, &action, &at); err != nil {
			rows.Close()
			return pass, err
		}
		if syncID != nil {
			read = append(read, FeedRow{SyncID: *syncID, Model: *model, ModelID: *modelID, Action: *action, At: at.UTC()})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return pass, err
	}
	if pass.PrunedThrough > pass.Cursor {
		pass.Pruned = true
		log.Printf("ytwrite.feed_pruned consumer=%q cursor=%d pruned_through=%d", c.name, pass.Cursor, pass.PrunedThrough)
		text := fmt.Sprintf("[yt-feed] pruned consumer=%s cursor=%d pruned_through=%d | the consumer reads nothing until its cursor moves (backfill)", c.name, pass.Cursor, pass.PrunedThrough)
		if _, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindYTWriteOp, "handoffkeep-design", fmt.Sprintf("yt-feed-%s-pruned-%d", c.name, pass.Cursor), text, now); err != nil {
			return pass, err
		}
		return pass, tx.Commit(ctx)
	}
	pass.Read = len(read)
	// A full page ends at the last row read. A short page read every row of
	// c's models up to max_sync_id in this snapshot, so the cursor moves to
	// max_sync_id: the rows of other models in between are not c's, and a
	// consumer whose models are idle keeps up with the prune boundary instead
	// of falling behind it. No row at or below max_sync_id can commit later:
	// the allocator assigns sync_ids and sets max_sync_id in one transaction
	// under its advisory lock (sync_migration.go), so every lower sync_id
	// committed (or was abandoned) before that one.
	next := pass.Cursor
	if len(read) > 0 {
		next = read[len(read)-1].SyncID
	}
	if len(read) < limit && maxSyncID > next {
		next = maxSyncID
	}
	if len(read) > 0 {
		batch := collapseFeedRows(read)
		pass.Handled = len(batch)
		if err := c.handler(s, now)(ctx, tx, batch); err != nil {
			return YTFeedPass{Consumer: c.name}, fmt.Errorf("yt feed consumer %s: %w", c.name, err)
		}
	}
	if next == pass.Cursor {
		return pass, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE yt_feed_cursors SET last_sync_id=$2, updated_at=$3 WHERE consumer=$1`, c.name, next, now); err != nil {
		return pass, err
	}
	if err := tx.Commit(ctx); err != nil {
		return YTFeedPass{Consumer: c.name}, err
	}
	pass.Cursor = next
	if pass.Read > 0 {
		log.Printf("ytwrite.feed consumer=%q read=%d handled=%d last_sync_id=%d", c.name, pass.Read, pass.Handled, next)
	}
	return pass, nil
}

// collapseFeedRows keeps, of the rows of one (model, model_id), only the
// last, at its own position: the result stays in sync_id order.
func collapseFeedRows(rows []FeedRow) []FeedRow {
	type key struct{ model, id string }
	last := make(map[key]int, len(rows))
	for i, r := range rows {
		last[key{r.Model, r.ModelID}] = i
	}
	out := make([]FeedRow, 0, len(last))
	for i, r := range rows {
		if last[key{r.Model, r.ModelID}] == i {
			out = append(out, r)
		}
	}
	return out
}

// SnapshotKey is the idempotency key of a snapshot op: the op kind, the
// entity id, the snapshot's sequence number for that entity and kind, and the
// SHA of the snapshot it sends, as <kind>:<id>:<seq>:<sha>. The sequence makes
// every prepared snapshot its own key, so a state the entity returns to (A ->
// B -> A) is sent again.
func SnapshotKey(kind, entityID string, seq int64, sha string) string {
	return kind + ":" + entityID + ":" + strconv.FormatInt(seq, 10) + ":" + sha
}

// prepareYTSnapshotTx prepares a snapshot op n in tx unless sha equals the
// last SHA prepared for (n.EntityKind, n.EntityID, n.Kind) and the op that
// sent it was not rejected, so an update that leaves the snapshot as it was (a
// last_activity_at repair) makes no op while a snapshot YouTrack refused is
// prepared again (hk 4057). Otherwise it increments that entity and kind's
// seq, sets n.Key to SnapshotKey(n.Kind, n.EntityID, seq, sha), prepares the
// op and records seq, sha and the op's id in yt_snapshot_state, all in the
// same transaction. prepared is false only for a skipped snapshot.
//
// Locks: the entity's lane lock (lockYTEntity) first, then its
// yt_snapshot_state row, then what prepareYTOpTx takes. Reading the last op's
// state by id takes no lock. No other writer locks a yt_snapshot_state row.
func prepareYTSnapshotTx(ctx context.Context, tx pgx.Tx, now time.Time, n ytwrite.NewOp, sha string) (ytwrite.Op, bool, error) {
	if sha == "" || n.Kind == "" || n.EntityKind == "" || n.EntityID == "" {
		return ytwrite.Op{}, false, errors.New("snapshot op needs a kind, an entity and a sha")
	}
	if err := lockYTEntity(ctx, tx, n.EntityKind, n.EntityID); err != nil {
		return ytwrite.Op{}, false, err
	}
	var lastSHA string
	var seq, lastOpID int64
	err := tx.QueryRow(ctx, `SELECT seq,last_sha,last_op_id FROM yt_snapshot_state WHERE entity_kind=$1 AND entity_id=$2 AND kind=$3 FOR UPDATE`,
		n.EntityKind, n.EntityID, n.Kind).Scan(&seq, &lastSHA, &lastOpID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.Op{}, false, err
	}
	if err == nil {
		skip, err := ytSnapshotUnchanged(ctx, tx, lastSHA, lastOpID, sha)
		if err != nil || skip {
			return ytwrite.Op{}, false, err
		}
	}
	seq++
	n.Key = SnapshotKey(n.Kind, n.EntityID, seq, sha)
	op, _, err := prepareYTOpTx(ctx, tx, now, n)
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yt_snapshot_state(entity_kind,entity_id,kind,seq,last_sha,last_op_id) VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT (entity_kind,entity_id,kind) DO UPDATE SET seq=EXCLUDED.seq, last_sha=EXCLUDED.last_sha, last_op_id=EXCLUDED.last_op_id`,
		n.EntityKind, n.EntityID, n.Kind, seq, sha, op.ID); err != nil {
		return ytwrite.Op{}, false, err
	}
	return op, true, nil
}

// ytSnapshotUnchanged reports whether a snapshot with sha needs no new op, given
// its entity's yt_snapshot_state (lastSHA, lastOpID): true when sha equals
// lastSHA and that op is not rejected. Any other state of the op (prepared,
// dispatched, remote_unknown, applied) or a missing op row keeps the skip,
// since that op is sent, being sent or settled; only a rejected op, which
// will never be sent, lets the same snapshot be prepared again. It reads the
// op by id without a lock.
func ytSnapshotUnchanged(ctx context.Context, tx pgx.Tx, lastSHA string, lastOpID int64, sha string) (bool, error) {
	if lastSHA != sha {
		return false, nil
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM yt_write_ops WHERE id=$1`, lastOpID).Scan(&state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return true, nil
	case err != nil:
		return false, err
	}
	return state != string(ytwrite.StateRejected), nil
}

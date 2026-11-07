package store

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The operator's inbox triage state (hk 3343): which attention item the
// operator has read, completed or snoozed, stored on the server so Home and
// Work agree and a different browser sees the same triage. One row per item
// key the console builds; the row exists only once something was written.

// Inbox write actions. read/unread and done/undone are each other's inverse;
// snooze hides the item until a time.
const (
	InboxActionRead   = "read"
	InboxActionUnread = "unread"
	InboxActionDone   = "done"
	InboxActionUndone = "undone"
	InboxActionSnooze = "snooze"
)

// InboxBatchLimit is how many items one write applies. A longer list is cut
// here and reported as Skipped, so the console can ask the operator to
// resubmit rather than dropping the rest silently.
const InboxBatchLimit = 50

// MaxInboxSnooze bounds how far ahead one snooze may end.
const MaxInboxSnooze = 366 * 24 * time.Hour

var (
	// ErrInvalidInboxWrite refuses a malformed call: an unknown action, an
	// empty actor or idempotency key.
	ErrInvalidInboxWrite = errors.New("invalid_inbox_write")
	// ErrInvalidInboxItem refuses an item key the console never builds.
	ErrInvalidInboxItem = errors.New("invalid_inbox_item")
	// ErrInvalidInboxSnooze refuses a snooze time that is not in the future
	// or is further out than MaxInboxSnooze.
	ErrInvalidInboxSnooze = errors.New("invalid_inbox_snooze")
	// ErrInboxUndoConflict reports an undo item a later write has moved; the
	// undo leaves that item as the later write left it.
	ErrInboxUndoConflict = errors.New("inbox_undo_conflict")
)

// inboxItemKeyRE is the attention item key the console builds:
// decision:<request|lane_event>:<id>, task:<id>, and deploy:<service>:<id>
// (a deploy row keeps the service so one task merged into two services stays
// two rows). The deploy form without a service is accepted as the brief names
// it. The service part matches store nameRE, the source of every registered
// deploy service name, so every name Home can show is triaged too.
var inboxItemKeyRE = regexp.MustCompile(`^(decision:(request|lane_event):[1-9][0-9]{0,14}|task:[1-9][0-9]{0,14}|deploy:([A-Za-z0-9._-]{1,128}:)?[1-9][0-9]{0,14})$`)

// ValidInboxItemKey reports whether key is an attention item key.
func ValidInboxItemKey(key string) bool { return inboxItemKeyRE.MatchString(key) }

// InboxState is one item's stored triage state.
type InboxState struct {
	ItemKey      string
	ReadAt       *time.Time
	DoneAt       *time.Time
	SnoozedUntil *time.Time
	Actor        string
	UpdatedAt    time.Time
}

// Woken reports whether a snooze has come due: the item is visible again and
// the console marks it 「다시 알림」 until the operator reads or completes it.
func (x InboxState) Woken(now time.Time) bool {
	return inboxWoken(x.DoneAt, x.SnoozedUntil, now)
}

func inboxWoken(doneAt, snoozedUntil *time.Time, now time.Time) bool {
	return doneAt == nil && snoozedUntil != nil && !snoozedUntil.After(now)
}

// InboxWrite is one item's outcome. Err is set when this item was refused or
// skipped; the other items of the same call still applied.
type InboxWrite struct {
	ItemKey string
	State   InboxState
	Err     error
}

// InboxWriteResult is what one write applied.
type InboxWriteResult struct {
	Items   []InboxWrite
	Skipped int
}

// ApplyInbox applies one action to up to InboxBatchLimit item keys. Each item
// is its own transaction so one refusal does not roll back the rest. The
// event key is the idempotency key: applying it again returns the current
// state and writes nothing.
func (s *Store) ApplyInbox(ctx context.Context, keys []string, action string, until *time.Time, actor, eventKey string) (InboxWriteResult, error) {
	out := InboxWriteResult{Items: []InboxWrite{}}
	if !validInboxAction(action) || strings.TrimSpace(actor) == "" || !validText(actor, 320) || strings.TrimSpace(eventKey) == "" || !validText(eventKey, 128) {
		return out, ErrInvalidInboxWrite
	}
	// Microsecond precision, the column's: the value returned here is the
	// value a later read sees.
	now := time.Now().UTC().Truncate(time.Microsecond)
	if action == InboxActionSnooze {
		if until == nil {
			return out, ErrInvalidInboxSnooze
		}
		end := until.UTC().Truncate(time.Microsecond)
		if !end.After(now) || end.Sub(now) > MaxInboxSnooze {
			return out, ErrInvalidInboxSnooze
		}
		until = &end
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if !ValidInboxItemKey(key) {
			out.Items = append(out.Items, InboxWrite{ItemKey: key, Err: ErrInvalidInboxItem})
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(seen) > InboxBatchLimit {
			out.Skipped++
			continue
		}
		state, err := s.applyInboxItem(ctx, key, action, until, actor, eventKey, now)
		out.Items = append(out.Items, InboxWrite{ItemKey: key, State: state, Err: err})
	}
	return out, nil
}

// UndoInbox restores every item of the write eventKey in one transaction: the
// read/done/snooze values the write replaced, and the decision request snooze
// it mirrored. An item a later write moved is left alone and reported with
// ErrInboxUndoConflict, so two tabs cannot undo each other's work. A second
// undo of the same key is a no-op.
func (s *Store) UndoInbox(ctx context.Context, eventKey, actor string) ([]InboxWrite, error) {
	if strings.TrimSpace(eventKey) == "" || !validText(eventKey, 128) || strings.TrimSpace(actor) == "" || !validText(actor, 320) {
		return nil, ErrInvalidInboxWrite
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT item_key,previous_read_at,previous_done_at,previous_snoozed_until,previous_request_id,previous_request_snoozed_until,applied_read_at,applied_done_at,applied_snoozed_until
		FROM operator_inbox_events WHERE event_key=$1 AND undone_at IS NULL ORDER BY item_key`, eventKey)
	if err != nil {
		return nil, err
	}
	type restore struct {
		key                                      string
		read, done, snooze                       *time.Time
		requestID                                *int64
		request                                  *time.Time
		appliedRead, appliedDone, appliedSnooze  *time.Time
	}
	restores := []restore{}
	for rows.Next() {
		var r restore
		if err := rows.Scan(&r.key, &r.read, &r.done, &r.snooze, &r.requestID, &r.request, &r.appliedRead, &r.appliedDone, &r.appliedSnooze); err != nil {
			rows.Close()
			return nil, err
		}
		restores = append(restores, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]InboxWrite, 0, len(restores))
	for _, r := range restores {
		current, found, err := inboxStateTx(ctx, tx, r.key, true)
		if err != nil {
			return nil, err
		}
		if !found || !sameInboxTime(current.ReadAt, r.appliedRead) || !sameInboxTime(current.DoneAt, r.appliedDone) || !sameInboxTime(current.SnoozedUntil, r.appliedSnooze) {
			out = append(out, InboxWrite{ItemKey: r.key, Err: ErrInboxUndoConflict})
			continue
		}
		requestGone := false
		if r.requestID != nil {
			var currentSnooze *time.Time
			err := tx.QueryRow(ctx, `SELECT snoozed_until FROM decision_requests WHERE id=$1 FOR UPDATE`, *r.requestID).Scan(&currentSnooze)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				requestGone = true
			case err != nil:
				return nil, err
			case !sameInboxTime(currentSnooze, r.appliedSnooze):
				out = append(out, InboxWrite{ItemKey: r.key, Err: ErrInboxUndoConflict})
				continue
			}
		}
		if r.read == nil && r.done == nil && r.snooze == nil {
			if _, err := tx.Exec(ctx, `DELETE FROM operator_inbox_state WHERE item_key=$1`, r.key); err != nil {
				return nil, err
			}
		} else if _, err := tx.Exec(ctx, `INSERT INTO operator_inbox_state(item_key,read_at,done_at,snoozed_until,actor,updated_at)
			VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT (item_key) DO UPDATE SET read_at=EXCLUDED.read_at,done_at=EXCLUDED.done_at,snoozed_until=EXCLUDED.snoozed_until,actor=EXCLUDED.actor,updated_at=EXCLUDED.updated_at`,
			r.key, r.read, r.done, r.snooze, actor, now); err != nil {
			return nil, err
		}
		if r.requestID != nil && !requestGone {
			if _, err := tx.Exec(ctx, `UPDATE decision_requests SET snoozed_until=$2,updated_at=$3 WHERE id=$1`, *r.requestID, r.request, now); err != nil {
				return nil, err
			}
		}
		out = append(out, InboxWrite{ItemKey: r.key, State: InboxState{ItemKey: r.key, ReadAt: r.read, DoneAt: r.done, SnoozedUntil: r.snooze, Actor: actor, UpdatedAt: now}})
	}
	if _, err := tx.Exec(ctx, `UPDATE operator_inbox_events SET undone_at=$2 WHERE event_key=$1 AND undone_at IS NULL`, eventKey, now); err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

// sameInboxTime reports whether two nullable times are the same instant,
// including both absent.
func sameInboxTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// InboxStates reads the stored triage state of keys, in batches of 500.
// Items with no row are absent from the map.
func (s *Store) InboxStates(ctx context.Context, keys []string) (map[string]InboxState, error) {
	out := make(map[string]InboxState, len(keys))
	for start := 0; start < len(keys); start += 500 {
		end := start + 500
		if end > len(keys) {
			end = len(keys)
		}
		rows, err := s.pool.Query(ctx, `SELECT item_key,read_at,done_at,snoozed_until,actor,updated_at FROM operator_inbox_state WHERE item_key=ANY($1)`, keys[start:end])
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var x InboxState
			if err := rows.Scan(&x.ItemKey, &x.ReadAt, &x.DoneAt, &x.SnoozedUntil, &x.Actor, &x.UpdatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			out[x.ItemKey] = x
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func validInboxAction(action string) bool {
	switch action {
	case InboxActionRead, InboxActionUnread, InboxActionDone, InboxActionUndone, InboxActionSnooze:
		return true
	}
	return false
}

// inboxRequestID reads the decision_requests id out of decision:request:<id>.
func inboxRequestID(key string) (int64, bool) {
	rest, ok := strings.CutPrefix(key, "decision:request:")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}

// applyInboxItem writes one item in its own transaction. Lock order: the
// operator_inbox_state row, then the decision_requests row a snooze mirrors.
// Only this writer takes inbox state rows, so the pair cannot close a cycle
// with a decision writer that locks only its request.
func (s *Store) applyInboxItem(ctx context.Context, key, action string, until *time.Time, actor, eventKey string, now time.Time) (InboxState, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InboxState{}, err
	}
	defer tx.Rollback(ctx)

	var replayed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_inbox_events WHERE event_key=$1 AND item_key=$2)`, eventKey, key).Scan(&replayed); err != nil {
		return InboxState{}, err
	}
	if replayed {
		// The same click delivered twice: report what the operator sees now.
		prev, _, err := inboxStateTx(ctx, tx, key, false)
		if err != nil {
			return InboxState{}, err
		}
		return prev, tx.Commit(ctx)
	}

	// Create the row before locking it. SELECT ... FOR UPDATE locks nothing
	// when no row exists, so two first writes on one item would both read an
	// empty state and the later upsert would drop the earlier one. An
	// all-null row means "no triage".
	if _, err := tx.Exec(ctx, `INSERT INTO operator_inbox_state(item_key,actor,updated_at)
		VALUES($1,$2,$3) ON CONFLICT (item_key) DO NOTHING`, key, actor, now); err != nil {
		return InboxState{}, err
	}
	prev, _, err := inboxStateTx(ctx, tx, key, true)
	if err != nil {
		return InboxState{}, err
	}

	next := prev
	next.ItemKey = key
	switch action {
	case InboxActionRead:
		// Reading acknowledges the item: it clears the unread mark and any
		// snooze, including one that has already come due (「다시 알림」).
		next.ReadAt = &now
		next.SnoozedUntil = nil
	case InboxActionUnread:
		next.ReadAt = nil
		next.SnoozedUntil = nil
	case InboxActionDone:
		if next.DoneAt == nil {
			next.DoneAt = &now
		}
	case InboxActionUndone:
		next.DoneAt = nil
	case InboxActionSnooze:
		next.SnoozedUntil = until
		next.ReadAt = nil
	}
	next.Actor = actor
	next.UpdatedAt = now
	if _, err := tx.Exec(ctx, `INSERT INTO operator_inbox_state(item_key,read_at,done_at,snoozed_until,actor,updated_at)
		VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT (item_key) DO UPDATE SET read_at=EXCLUDED.read_at,done_at=EXCLUDED.done_at,snoozed_until=EXCLUDED.snoozed_until,actor=EXCLUDED.actor,updated_at=EXCLUDED.updated_at`,
		key, next.ReadAt, next.DoneAt, next.SnoozedUntil, actor, now); err != nil {
		return InboxState{}, err
	}

	var requestID *int64
	var previousRequestSnooze *time.Time
	if action == InboxActionSnooze {
		if id, ok := inboxRequestID(key); ok {
			// Snoozing a decision request from the inbox also snoozes the
			// request itself, in this same transaction (hk 3343 §2).
			before, err := snoozeDecisionRecordTx(ctx, tx, id, *until, now)
			if err != nil {
				return InboxState{}, err
			}
			requestID = &id
			previousRequestSnooze = before
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operator_inbox_events(event_key,item_key,action,previous_read_at,previous_done_at,previous_snoozed_until,previous_request_id,previous_request_snoozed_until,applied_read_at,applied_done_at,applied_snoozed_until,actor,at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		eventKey, key, action, prev.ReadAt, prev.DoneAt, prev.SnoozedUntil, requestID, previousRequestSnooze, next.ReadAt, next.DoneAt, next.SnoozedUntil, actor, now); err != nil {
		return InboxState{}, err
	}
	return next, tx.Commit(ctx)
}

// inboxStateTx reads one row, optionally for update. A missing row reports
// found false with the key set so callers never build a keyless state.
func inboxStateTx(ctx context.Context, q rowQuerier, key string, lock bool) (InboxState, bool, error) {
	query := `SELECT item_key,read_at,done_at,snoozed_until,actor,updated_at FROM operator_inbox_state WHERE item_key=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var x InboxState
	err := q.QueryRow(ctx, query, key).Scan(&x.ItemKey, &x.ReadAt, &x.DoneAt, &x.SnoozedUntil, &x.Actor, &x.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return InboxState{ItemKey: key}, false, nil
	}
	if err != nil {
		return InboxState{}, false, err
	}
	return x, true, nil
}

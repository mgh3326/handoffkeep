package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The queues that keep attempt state. The key is the task id or the document
// key, as text.
const (
	YouTrackQueueTask = "task"
	YouTrackQueueDoc  = "doc"
)

// YouTrackSkipProjectMissing is the skip reason for an item whose hk project
// has no YouTrack mapping. The skip holds until a mapping is written.
const YouTrackSkipProjectMissing = "project_missing"

// youtrackAttemptErrorMax bounds the stored error text.
const youtrackAttemptErrorMax = 500

// RecordYouTrackAttempt writes the outcome of one attempt at a queue key.
//   - failure set: the key goes back with backoff (30s, doubling, capped at
//     one hour) and its attempt count grows.
//   - skip set: the key is parked until ClearYouTrackSkips runs (a mapping was
//     written). It is not due, and it sorts last.
//   - both empty: the attempt succeeded. The count resets and the key sorts
//     behind the keys that have waited longer, which keeps in-flight items
//     from holding the head of the batch.
//
// next_attempt_at is written by the database clock only, so the scans and the
// writes agree on one clock.
func (s *Store) RecordYouTrackAttempt(ctx context.Context, queue, key, failure, skip string) error {
	if queue != YouTrackQueueTask && queue != YouTrackQueueDoc {
		return fmt.Errorf("invalid youtrack attempt queue %q", queue)
	}
	if key == "" {
		return fmt.Errorf("youtrack attempt key is required")
	}
	if len(failure) > youtrackAttemptErrorMax {
		failure = truncateText(failure, youtrackAttemptErrorMax)
	}
	var err error
	switch {
	case skip != "":
		_, err = s.pool.Exec(ctx, `INSERT INTO youtrack_attempts(queue,item_key,next_attempt_at,attempts,last_error,skip_reason,updated_at)
			VALUES($1,$2,'infinity'::timestamptz,0,'',$3,now())
			ON CONFLICT(queue,item_key) DO UPDATE SET next_attempt_at='infinity'::timestamptz, last_error='', skip_reason=EXCLUDED.skip_reason, updated_at=now()`,
			queue, key, skip)
	case failure != "":
		_, err = s.pool.Exec(ctx, `INSERT INTO youtrack_attempts(queue,item_key,next_attempt_at,attempts,last_error,skip_reason,updated_at)
			VALUES($1,$2,now()+make_interval(secs=>30),1,$3,'',now())
			ON CONFLICT(queue,item_key) DO UPDATE SET attempts=youtrack_attempts.attempts+1, last_error=EXCLUDED.last_error, skip_reason='',
				next_attempt_at=now()+make_interval(secs=>LEAST(30*power(2,LEAST(youtrack_attempts.attempts,8)),3600)), updated_at=now()`,
			queue, key, failure)
	default:
		_, err = s.pool.Exec(ctx, `INSERT INTO youtrack_attempts(queue,item_key,next_attempt_at,attempts,last_error,skip_reason,updated_at)
			VALUES($1,$2,now(),0,'','',now())
			ON CONFLICT(queue,item_key) DO UPDATE SET next_attempt_at=now(), attempts=0, last_error='', skip_reason='', updated_at=now()`,
			queue, key)
	}
	return err
}

// ClearYouTrackSkips makes every parked key due again. Writing a project
// mapping calls it, so a skipped item is judged against the new mapping.
func (s *Store) ClearYouTrackSkips(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `DELETE FROM youtrack_attempts WHERE skip_reason<>''`)
	return err
}

// youtrackAttemptRank is the next-attempt time of a queue key, read from the
// attempt row alias. A key with no row ranks at the epoch. A queue scan is due
// when this is at or before now() (database clock), and orders by it, then by
// the key, so the order is total: keys that have waited longest come first.
func youtrackAttemptRank(alias string) string {
	return "COALESCE(" + alias + ".next_attempt_at, to_timestamp(0))"
}

package store

import (
	"context"
	"log"
	"time"
)

// YTPayloadRedactBatch bounds one payload-redaction batch, so a large backlog
// never holds one long write. The alert tick redacts one batch per pass; a
// pass that fills its batch leaves the rest for the next tick, which is what
// makes the redaction resumable.
const YTPayloadRedactBatch = 500

// ytPayloadRedactAge is how old a resolved op must be before its payload is
// redacted (operator decision dr-5056-2, answer A). The worker's cutoff
// carries a margin inside the guard's 30-day window, so a row the worker
// selects is never refused by the guard over Go/database clock skew.
const ytPayloadRedactAge = 30 * 24 * time.Hour

// YTPayloadRedactCutoffMargin is the margin above.
const YTPayloadRedactCutoffMargin = time.Minute

// RedactYTPayloads NULLs the payloads of up to limit applied or rejected ops
// resolved before now-30d, leaving payload_sha as the receipt. It is the hk
// 5056 companion to the widened yt_write_ops guard: the UPDATE the guard
// allows is exactly this one. A second run finds nothing (payload IS NOT
// NULL), so the redaction is idempotent, and a full batch leaves the rest
// for the next tick, so it is resumable. It logs one line per batch that
// redacts rows.
func (s *Store) RedactYTPayloads(ctx context.Context, now time.Time, limit int) (int64, error) {
	n := ytLimit(limit)
	cutoff := now.UTC().Add(-ytPayloadRedactAge - YTPayloadRedactCutoffMargin)
	redacted, err := s.db().Exec(ctx, sqlFor(s,
		`UPDATE yt_write_ops SET payload = NULL WHERE id IN (
			SELECT id FROM yt_write_ops
			WHERE state IN ('applied','rejected') AND payload IS NOT NULL AND resolved_at <= $1
			ORDER BY id LIMIT $2)`,
		`UPDATE yt_write_ops SET payload = NULL
			WHERE state IN ('applied','rejected') AND payload IS NOT NULL AND resolved_at <= ?
			ORDER BY id LIMIT ?`), cutoff, n)
	if err != nil {
		return 0, err
	}
	if redacted > 0 {
		log.Printf("ytwrite.payload_redacted redacted=%d", redacted)
	}
	return redacted, nil
}

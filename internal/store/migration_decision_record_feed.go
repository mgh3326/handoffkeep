package store

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// 20261112-decision-record-feed journals every write of a decision_requests
// row in sync_log (hk 4054), as a model decision row whose model_id is
// "record:<decision_requests id>" (DecisionRecordFeedID). The DEC issue of a
// decision is keyed by decision_requests.id, and the decision rows the feed
// had are the task's refs.decision_request (sync_decisions, keyed by request
// id): a decision from a chat question, a lane event, a job escalation or a
// task flag, and the answer of one, never touches refs.decision_request, so
// those rows do not see them. Twelve statements in three files write
// decision_requests; one row trigger journals them all in the writer's own
// transaction, as the triggers on tasks and documents do (sync_migration.go),
// so ResolveDecision, and every other answer path, emits its row in the
// answer's transaction.
//
// Why model decision and not a new model: sync_log.model is CHECK-limited and
// the /v1/sync client (internal/remote/sync.go, `handoffkeep sync delta`)
// refuses a page with any other model, so a new value would break every
// released client, and the previous release during blue-green or after a
// rollback. A "record:" id is never a request id (those are "dr-..."): a
// reader of requests fetches it through /v1/sync/rows, finds no request and
// treats it as gone, which docs/sync-feed.md already allows.
//
// No After: sync_log (version 30) and decision_requests (version 35) are
// integer versions, applied before every registered ID.
//
// Expand-only: one new trigger function and one trigger; no column, no
// constraint, no row. The previous release writes decision_requests as before
// and the trigger journals its writes too. Locks: CREATE TRIGGER takes SHARE
// ROW EXCLUSIVE on decision_requests briefly. A decision write now also takes
// the sync_log allocator's commit-time advisory lock, which every task write
// already takes.
func init() {
	registerMigration(migration{ID: "20261112-decision-record-feed", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, decisionRecordFeedStatements)
	}})
}

var decisionRecordFeedStatements = []string{
	`CREATE OR REPLACE FUNCTION handoffkeep_sync_emit_decision_record() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE row_id TEXT;
 BEGIN
   IF TG_OP='DELETE' THEN row_id := OLD.id::text; ELSE row_id := NEW.id::text; END IF;
   INSERT INTO sync_log(model,model_id,action,at) VALUES('decision','record:'||row_id,substr(TG_OP,1,1),clock_timestamp());
   RETURN NULL;
 END $$`,
	`DROP TRIGGER IF EXISTS sync_decision_records ON decision_requests`,
	`CREATE TRIGGER sync_decision_records AFTER INSERT OR UPDATE OR DELETE ON decision_requests FOR EACH ROW EXECUTE FUNCTION handoffkeep_sync_emit_decision_record()`,
}

// decisionRecordFeedPrefix starts the sync_log model_id of a
// decision_requests row (model decision).
const decisionRecordFeedPrefix = "record:"

// DecisionRecordFeedID is the sync_log model_id (model decision) of
// decision_requests row id.
func DecisionRecordFeedID(id int64) string {
	return decisionRecordFeedPrefix + strconv.FormatInt(id, 10)
}

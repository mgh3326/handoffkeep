//go:build mysql

package store

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// insertMySQLYTOp prepares one op directly, so the guard tests have a row to
// mutate. It bypasses the ledger only for the INSERT: the row is exactly the
// shape Prepare writes.
func insertMySQLYTOp(t *testing.T, s *Store, kind string) int64 {
	t.Helper()
	var id int64
	err := s.mysqlWriteTransaction(t.Context(), func(tx transaction) error {
		var err error
		id, err = tx.InsertID(t.Context(), `INSERT INTO yt_write_ops(principal, kind, idem_key, payload_sha, payload, entity_kind, entity_id,
			yt_target_id, actor, parent_id, child_seq, state, yt_result_id, reason, last_error, attempts, reconciles,
			created_at, next_attempt_at, tenant)
			VALUES (?,?,?,?,CAST(? AS JSON),?,?,?,?,0,0,'prepared','','','',0,0,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),?)`,
			"hk:test", kind, "key-"+kind, "sha", `{}`, "task", "1", "", "actor", "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMySQLYTWriteOpsGuards(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	id := insertMySQLYTOp(t, s, "comment_append")

	// Identity and payload are immutable.
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET payload_sha='other' WHERE id=?`, id); err == nil {
		t.Error("guard accepted a payload_sha change")
	}
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET entity_id='2' WHERE id=?`, id); err == nil {
		t.Error("guard accepted an entity_id change")
	}
	// An op cannot make an off-list state move.
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET state='applied', yt_result_id='HK-1', resolved_at=UTC_TIMESTAMP(6) WHERE id=?`, id); err == nil {
		t.Error("guard accepted prepared->applied")
	}
	// The tenant is immutable.
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET tenant='other' WHERE id=?`, id); err == nil {
		t.Error("guard accepted a tenant change")
	}
	// The ledger is append-only.
	if _, err := s.mysqlPool.ExecContext(ctx, `DELETE FROM yt_write_ops WHERE id=?`, id); err == nil {
		t.Error("guard accepted a delete")
	}

	// The retarget exception (hk 4263): a queued comment op takes its target
	// at its claim.
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET state='dispatched', attempts=1, dispatched_at=UTC_TIMESTAMP(6),
		yt_target_id='HK-1', payload_sha='newsha' WHERE id=?`, id); err != nil {
		t.Errorf("retarget rejected: %v", err)
	}
	// The comment hold (hk 4457): a queued op stays prepared.
	id2 := insertMySQLYTOp(t, s, "comment_append")
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET attempts=attempts+1, next_attempt_at=UTC_TIMESTAMP(6) WHERE id=?`, id2); err != nil {
		t.Errorf("comment hold rejected: %v", err)
	}

	// issue_create apply without its binding and map is refused.
	id3 := insertMySQLYTOp(t, s, "issue_create")
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET state='dispatched', attempts=1, dispatched_at=UTC_TIMESTAMP(6), yt_target_id='HK-9', payload_sha='r' WHERE id=?`, id3); err != nil {
		t.Fatalf("issue dispatch rejected: %v", err)
	}
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET state='applied', yt_result_id='HK-9', resolved_at=UTC_TIMESTAMP(6) WHERE id=?`, id3); err == nil {
		t.Error("issue apply without binding accepted")
	}
	// decision_create apply without its binding is refused.
	id4 := insertMySQLYTOp(t, s, "decision_create")
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET state='dispatched', attempts=1, dispatched_at=UTC_TIMESTAMP(6), yt_target_id='HK-9' WHERE id=?`, id4); err != nil {
		t.Fatalf("decision dispatch rejected: %v", err)
	}
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET state='applied', yt_result_id='HK-9', resolved_at=UTC_TIMESTAMP(6) WHERE id=?`, id4); err == nil {
		t.Error("decision apply without binding accepted")
	}
	// With reason binding_conflict the apply passes (the op is adopted).
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE yt_write_ops SET state='applied', yt_result_id='HK-9', reason='binding_conflict', resolved_at=UTC_TIMESTAMP(6) WHERE id=?`, id4); err != nil {
		t.Errorf("binding_conflict apply rejected: %v", err)
	}

	// @hk_bulk_load bypasses every guard for the copy tool.
	conn, err := s.mysqlPool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET @hk_bulk_load=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE yt_write_ops SET payload_sha='copy', tenant='copied' WHERE id=?`, id); err != nil {
		t.Errorf("bulk-load identity update rejected: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM yt_write_ops WHERE id=?`, id2); err != nil {
		t.Errorf("bulk-load delete rejected: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `SET @hk_bulk_load=NULL`); err != nil {
		t.Fatal(err)
	}
}

// TestMySQLYTWriteLedgerPrepareClaim applies one comment op through the real
// ledger on MySQL: prepare, claim with the retarget, apply.
func TestMySQLYTWriteLedgerRoundTrip(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	insertMySQLTestTask(t, s)

	n, err := ytwrite.CommentOp(ytwrite.CommentRequest{Principal: YTCommentPrincipal, Key: YTCommentKey(1), Actor: "tester", TaskID: 1, Tenant: "t1", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	l := &YTWriteLedger{s: s}
	op, prepared, err := l.Prepare(ctx, n)
	if err != nil || !prepared {
		t.Fatalf("prepare: prepared=%t err=%v", prepared, err)
	}
	again, prepared, err := l.Prepare(ctx, n)
	if err != nil || prepared || again.ID != op.ID {
		t.Fatalf("idempotent prepare: %+v prepared=%t err=%v", again, prepared, err)
	}
	got, err := l.Get(ctx, n.Principal, n.Kind, n.Key)
	if err != nil || got.ID != op.ID {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if err := l.Reject(ctx, op.ID, "test", "detail", time.Now().UTC()); err != nil {
		t.Fatalf("reject: %v", err)
	}
}

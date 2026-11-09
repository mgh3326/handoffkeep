package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// ytLedgerFactory opens one schema for the whole test and hands every subtest an
// empty op log, with ids restarting at 1: truncating is much cheaper than a
// migrated schema per subtest, and the suites run their subtests in sequence.
func ytLedgerFactory(t *testing.T) ytwritetest.NewLedger {
	s := schemaStore(t, "ytw")
	return func(t *testing.T) ytwrite.Ledger {
		t.Helper()
		if _, err := s.pool.Exec(t.Context(), `TRUNCATE yt_entity_map, yt_write_ops RESTART IDENTITY`); err != nil {
			t.Fatal(err)
		}
		return s.YTWriteLedger()
	}
}

func TestYTWriteLedgerContract(t *testing.T) { ytwritetest.RunLedger(t, ytLedgerFactory(t)) }

func TestYTWriteProtocolOverPostgres(t *testing.T) { ytwritetest.RunProtocol(t, ytLedgerFactory(t)) }

// The trigger is the last line of defence for the protocol's rules: whatever a
// future writer does, an op's payload never changes and its lifecycle only
// moves forward.
func TestYTWriteOpsGuards(t *testing.T) {
	s := schemaStore(t, "ytwg")
	ctx := context.Background()
	l := s.YTWriteLedger()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	mk := func(key string) ytwrite.Op {
		t.Helper()
		op, _, err := l.Prepare(ctx, now, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend, Key: key, PayloadSHA: "s" + key,
			Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "1", YTTargetID: "2-1", Actor: "a"})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	exec := func(q string, args ...any) error {
		_, err := s.pool.Exec(ctx, q, args...)
		return err
	}
	wantErr := func(err error, sub string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), sub) {
			t.Fatalf("err = %v, want it to contain %q", err, sub)
		}
	}
	a := mk("a")
	for _, col := range []string{`payload='{"body":"y"}'`, `payload_sha='z'`, `principal='q'`, `kind='other'`, `idem_key='k2'`,
		`entity_id='2'`, `entity_kind='job'`, `yt_target_id='9-9'`, `actor='b'`, `created_at=now()`, `child_seq=3`} {
		wantErr(exec(`UPDATE yt_write_ops SET `+col+` WHERE id=$1`, a.ID), "immutable")
	}
	wantErr(exec(`DELETE FROM yt_write_ops WHERE id=$1`, a.ID), "append-only")
	wantErr(exec(`UPDATE yt_write_ops SET state='applied', yt_result_id='c-1', resolved_at=now() WHERE id=$1`, a.ID), "cannot move from prepared to applied")
	wantErr(exec(`UPDATE yt_write_ops SET state='bogus' WHERE id=$1`, a.ID), "cannot move from prepared to bogus")
	if _, out, err := l.Claim(ctx, now, a.ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim = %s %v", out, err)
	}
	if _, err := l.Apply(ctx, now, a.ID, "c-1", nil); err != nil {
		t.Fatal(err)
	}
	wantErr(exec(`UPDATE yt_write_ops SET reason='late' WHERE id=$1`, a.ID), "cannot change")
	wantErr(exec(`UPDATE yt_write_ops SET state='rejected', resolved_at=now() WHERE id=$1`, a.ID), "cannot change")
	// A resolved row without its timestamp, and applied without an id, cannot exist.
	b := mk("b")
	wantErr(exec(`UPDATE yt_write_ops SET state='rejected' WHERE id=$1`, b.ID), "violates check constraint")
	// Map rows: one hk key per yt id and the reverse.
	if err := exec(`INSERT INTO yt_entity_map(entity_kind, hk_key, yt_id, op_id, created_at) VALUES ('comment','op:1','c-1',$1,now())`, a.ID); err != nil {
		t.Fatal(err)
	}
	wantErr(exec(`INSERT INTO yt_entity_map(entity_kind, hk_key, yt_id, op_id, created_at) VALUES ('comment','op:2','c-1',$1,now())`, a.ID), "yt_entity_map_entity_kind_yt_id_key")
	wantErr(exec(`INSERT INTO yt_entity_map(entity_kind, hk_key, yt_id, op_id, created_at) VALUES ('comment','op:1','c-2',$1,now())`, a.ID), "yt_entity_map_pkey")
}

//go:build mysql

package store

import (
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// TestMySQLYTWriteOpStuckAlertBindsEveryCutoff runs the stuck-op alert pass on
// MySQL. Its statement names the cutoff three times; PostgreSQL reuses $1, and
// MySQL needs one argument per ?, so the pass failed every tick on MySQL with
// "expected 4 arguments, got 2" (hk 5611).
func TestMySQLYTWriteOpStuckAlertBindsEveryCutoff(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	key := "hk5611-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, _, err := s.YTWriteLedger().Prepare(ctx, t0.Add(-11*time.Minute), ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend, Key: key, PayloadSHA: key,
		Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: key, YTTargetID: "2-" + key, Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.EnqueueYTWriteOpAlerts(ctx, t0)
	if err != nil {
		t.Fatalf("stuck alert pass on MySQL: %v", err)
	}
	if n < 1 {
		t.Fatalf("stuck alert pass on MySQL = %d notices, want at least 1 (a prepared op 11 minutes old)", n)
	}
}

//go:build mysql

package store

import (
	"context"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// A cancelled request must not poison the snapshot's pooled connection: the
// cleanup runs detached from cancellation, so ROLLBACK is still sent and the
// connection goes back usable. On the pre-fix head ROLLBACK fails on the
// cancelled context and the test fails there.
func TestMySQLSnapshotRollbackAfterCancelReleasesConnection(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.mysqlPool.SetMaxOpenConns(1)

	ctx, cancel := context.WithCancel(t.Context())
	tx, err := s.beginMySQLSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback after cancel: %v", err)
	}
	// The single pooled connection must take a write right away; a
	// connection returned inside its read-only transaction would refuse it.
	task, err := s.CreateTask(t.Context(), Task{Lane: "snapshot-cancel", Title: "snapshot cancel", Kind: "implement", CreatedBy: "snapshot-test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatalf("pooled connection unusable after cancelled snapshot: %v", err)
	}
	if task.ID < 1 {
		t.Fatalf("task=%+v", task)
	}
}

// A failed START must not return the connection to the pool with the SET's
// one-shot isolation pending: the next transaction on it would run at
// REPEATABLE READ instead of the pool's READ COMMITTED default. Cancelling
// between SET and START fails START deterministically; the discarded
// connection is replaced, so the next transaction reads READ-COMMITTED. On
// the pre-fix head the same connection is reused and reports
// REPEATABLE-READ.
func TestMySQLSnapshotFailedStartDiscardsPendingIsolation(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.mysqlPool.SetMaxOpenConns(1)

	entered := make(chan struct{})
	release := make(chan struct{})
	beginMySQLSnapshotSeam = func(context.Context) {
		close(entered)
		<-release
	}
	defer func() { beginMySQLSnapshotSeam = nil }()
	type snapshotResult struct {
		err error
	}
	done := make(chan snapshotResult, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		_, err := s.beginMySQLSnapshot(ctx)
		done <- snapshotResult{err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot seam not reached")
	}
	cancel()
	close(release)
	var res snapshotResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot begin did not finish")
	}
	beginMySQLSnapshotSeam = nil
	if res.err == nil {
		t.Fatal("START with a cancelled context succeeded")
	}
	tx, err := s.mysqlPool.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var isolation string
	if err := tx.QueryRow(`SELECT @@transaction_isolation`).Scan(&isolation); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if isolation != "READ-COMMITTED" {
		t.Fatalf("isolation=%q, want READ-COMMITTED", isolation)
	}
}

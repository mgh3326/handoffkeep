package store

import (
	"context"
	"testing"
	"time"
)

func TestWorkerLeaseDetectsTerminatedSession(t *testing.T) {
	s := schemaStore(t, "worker_lease")
	lease, held, err := s.TryWorkerLock(t.Context(), HubForwardWorkerLock)
	if err != nil || !held {
		t.Fatalf("held=%v err=%v", held, err)
	}
	defer lease.Release()
	var pid int
	if err := lease.conn.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lost := make(chan error, 1)
	go func() { lost <- lease.Watch(ctx) }()
	var terminated bool
	if err := s.pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminated=%v err=%v", terminated, err)
	}
	select {
	case err := <-lost:
		if err == nil || err == context.Canceled {
			t.Fatalf("watch error=%v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("watch did not detect the terminated session")
	}
}

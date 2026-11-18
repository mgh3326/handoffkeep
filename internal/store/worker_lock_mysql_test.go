//go:build mysql

package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestMySQLWorkerLeaseReleaseIsIdempotent(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	lease, held, err := s.TryWorkerLock(ctx, HubForwardWorkerLock)
	if err != nil || !held {
		t.Fatalf("held=%v err=%v", held, err)
	}
	lease.Release()
	lease.Release()
	again, held, err := s.TryWorkerLock(ctx, HubForwardWorkerLock)
	if err != nil || !held {
		t.Fatalf("after double release: held=%v err=%v", held, err)
	}
	again.Release()
	again.Release()
}

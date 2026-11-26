//go:build mysql

package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestMySQLWorkerLocksAreScopedToDatabase(t *testing.T) {
	ctx := t.Context()
	firstURL := testdb.MySQLDatabase(t)
	secondURL := testdb.MySQLDatabase(t)
	first, err := Open(ctx, firstURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	second, err := Open(ctx, secondURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	sameDatabase, err := Open(ctx, firstURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sameDatabase.Close)

	firstLease, held, err := first.TryWorkerLock(ctx, HubForwardWorkerLock)
	if err != nil || !held {
		t.Fatalf("first database held=%v err=%v; want acquired", held, err)
	}
	defer firstLease.Release()
	secondLease, held, err := second.TryWorkerLock(ctx, HubForwardWorkerLock)
	if err != nil || !held {
		t.Fatalf("second database held=%v err=%v; want acquired independently", held, err)
	}
	defer secondLease.Release()
	if firstLease.LockName() == secondLease.LockName() {
		t.Fatalf("different databases have the same worker lock name %q", firstLease.LockName())
	}

	contendingLease, held, err := sameDatabase.TryWorkerLock(ctx, HubForwardWorkerLock)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		contendingLease.Release()
		t.Fatal("second store on the same database acquired a held worker lock")
	}
}

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

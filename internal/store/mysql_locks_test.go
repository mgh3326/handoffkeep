package store

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestMySQLLockKeyNamespaces(t *testing.T) {
	a := mysqlLockKey("entity", "a\x00b")
	if !bytes.Equal(a, mysqlLockKey("entity", "a\x00b")) || bytes.Equal(a, mysqlLockKey("entity\x00a", "b")) {
		t.Fatal("lock key must be stable and namespaces must not overlap")
	}
	name := mysqlSessionLockName("worker", "1")
	if !strings.HasPrefix(name, "hk:") || len(name) > 64 {
		t.Fatalf("invalid session lock name: %q", name)
	}
}

func TestTryWorkerLockKeepsMySQLPortedPath(t *testing.T) {
	mysqlStore := &Store{dialect: mysqlDialect}
	_, held, err := mysqlStore.TryWorkerLock(context.Background(), WorkerLock(-1))
	if held || err == nil || err.Error() != "unknown worker lock" {
		t.Fatalf("TryWorkerLock() = held %v, err %v; want the existing MySQL lock validation", held, err)
	}
}

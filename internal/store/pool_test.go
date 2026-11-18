package store

import (
	"net/url"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

func TestPoolInstallsQueryTracer(t *testing.T) {
	// MinConns is zero: inspect pool configuration without opening a connection.
	u := (&url.URL{Scheme: "postgres", Host: t.Name(), Path: "/test"}).String()
	pool, err := newPool(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, ok := pool.Config().ConnConfig.Tracer.(telemetry.QueryTracer); !ok {
		t.Fatal("store pool is missing query tracing")
	}
}

// TestServingPoolReservesWorkerConnections pins the serve pool floor (review
// of #504): each session-lock worker holds one connection for its lifetime, so
// a pool at the URL's small explicit maximum must be raised to leave query
// headroom. A store opened the plain way keeps the URL's value.
func TestServingPoolReservesWorkerConnections(t *testing.T) {
	u := (&url.URL{Scheme: "postgres", Host: t.Name(), Path: "/test", RawQuery: "pool_max_conns=2"}).String()
	plain, err := newPool(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if got := plain.Config().MaxConns; got != 2 {
		t.Fatalf("plain pool MaxConns = %d, want 2", got)
	}
	serving, err := newPoolFloored(t.Context(), u, nil, workerPoolFloor)
	if err != nil {
		t.Fatal(err)
	}
	defer serving.Close()
	if got := serving.Config().MaxConns; got != workerPoolFloor {
		t.Fatalf("serving pool MaxConns = %d, want %d", got, workerPoolFloor)
	}
	if int(workerPoolFloor) <= len(sessionWorkerLocks) {
		t.Fatalf("worker pool floor %d leaves no headroom for %d leases", workerPoolFloor, len(sessionWorkerLocks))
	}
	// A URL that asks for more than the floor keeps its own maximum.
	big := (&url.URL{Scheme: "postgres", Host: t.Name(), Path: "/test", RawQuery: "pool_max_conns=20"}).String()
	p, err := newPoolFloored(t.Context(), big, nil, workerPoolFloor)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := p.Config().MaxConns; got != 20 {
		t.Fatalf("large pool MaxConns = %d, want 20", got)
	}
}

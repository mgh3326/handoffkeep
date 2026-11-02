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

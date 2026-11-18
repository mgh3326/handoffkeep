package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

// newPool keeps the query hook on both legacy and separately migrated runtime
// connections. Without a sampled request in the context, the tracer is a no-op.
func newPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return newPoolParams(ctx, url, nil)
}

// newPoolParams is newPool with extra run-time parameters that every
// connection of the pool sets at startup.
func newPoolParams(ctx context.Context, url string, params map[string]string) (*pgxpool.Pool, error) {
	return newPoolFloored(ctx, url, params, 0)
}

// newPoolFloored is newPoolParams with a connection floor. Serve passes
// workerPoolFloor because every session-lock worker holds one connection for
// its whole life: without the floor a pool at pgx's default maximum can be
// fully held by leases, leaving nothing for store queries or API requests.
// The URL's own pool_max_conns is honoured when it is larger.
func newPoolFloored(ctx context.Context, url string, params map[string]string, floor int32) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if floor > 0 && config.MaxConns < floor {
		config.MaxConns = floor
	}
	config.ConnConfig.Tracer = telemetry.QueryTracer{}
	for k, v := range params {
		config.ConnConfig.RuntimeParams[k] = v
	}
	return pgxpool.NewWithConfig(ctx, config)
}

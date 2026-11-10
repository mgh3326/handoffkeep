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
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	config.ConnConfig.Tracer = telemetry.QueryTracer{}
	for k, v := range params {
		config.ConnConfig.RuntimeParams[k] = v
	}
	return pgxpool.NewWithConfig(ctx, config)
}

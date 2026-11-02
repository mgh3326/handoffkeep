package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

// newPool keeps the query hook on both legacy and separately migrated runtime
// connections. Without a sampled request in the context, the tracer is a no-op.
func newPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	config.ConnConfig.Tracer = telemetry.QueryTracer{}
	return pgxpool.NewWithConfig(ctx, config)
}

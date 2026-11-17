package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mgh3326/handoffkeep/internal/mysqlurl"
)

func openMySQL(ctx context.Context, rawURL string) (*Store, error) {
	cfg, err := mysqlurl.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open mysql pool: %w", err)
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if err = migrateMySQL(ctx, db, mysqlMigrations); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{mysqlPool: db, dialect: mysqlDialect, queries: sqlQuerier{db}}, nil
}

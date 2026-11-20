package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mgh3326/handoffkeep/internal/mysqlurl"
)

func openMySQL(ctx context.Context, rawURL string) (*Store, error) {
	db, err := openMySQLPool(ctx, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err = migrateMySQL(ctx, db, mysqlMigrations); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{mysqlPool: db, dialect: mysqlDialect, queries: sqlQuerier{db}}, nil
}

// openMySQLReadOnly is OpenReadOnly for a mysql:// URL: it runs no migration,
// and every connection sets transaction_read_only, so MySQL refuses any write
// through the pool (error 1792), as default_transaction_read_only does on
// PostgreSQL.
func openMySQLReadOnly(ctx context.Context, rawURL string) (*Store, error) {
	db, err := openMySQLPool(ctx, rawURL, map[string]string{"transaction_read_only": "1"})
	if err != nil {
		return nil, err
	}
	return &Store{mysqlPool: db, dialect: mysqlDialect, queries: sqlQuerier{db}}, nil
}

// openMySQLPool opens and pings a MySQL pool for rawURL, with extra session
// variables set on every connection.
func openMySQLPool(ctx context.Context, rawURL string, session map[string]string) (*sql.DB, error) {
	cfg, err := mysqlurl.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	for k, v := range session {
		cfg.Params[k] = v
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open mysql pool: %w", err)
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Project historical deploy documents before body storage moves. Creating the
// table locks only the new table; the backfill takes ACCESS SHARE on documents.
// Previous-release writes after the snapshot use the read-side fallback.
func init() {
	registerMigration(migration{ID: "20261109-deploy-record-projection", Apply: func(ctx context.Context, tx pgx.Tx) error {
		if err := execAll(ctx, tx, []string{`CREATE TABLE deploy_record_projection (key TEXT PRIMARY KEY, sha256 TEXT NOT NULL, record BYTEA NOT NULL, decoded BOOLEAN NOT NULL)`}); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT key,sha256,body,created_at FROM documents WHERE strpos(key,'deploy/')=1`)
		if err != nil {
			return err
		}
		var records []StoredDeployRecord
		for rows.Next() {
			var key, sha, body string
			var created time.Time
			if err = rows.Scan(&key, &sha, &body, &created); err != nil {
				rows.Close()
				return err
			}
			records = append(records, parseStoredDeployRecord(key, sha, body, created))
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		for _, x := range records {
			if err = writeDeployProjection(ctx, tx, x); err != nil {
				return err
			}
		}
		return nil
	}})
}

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeployRecord gained FailedStepName after the projection table existed, and a
// projection row stores the marshalled record: rows written before it have no
// such key. Rewrite every projected record from its document (documents are
// untouched, so this is expand-only) so readers no longer parse bodies for
// old rows. The read-side fallback still covers rows the previous release
// writes during the blue-green rollout.
//
// Lock: rows are updated in place in deploy_record_projection; documents are
// read under ACCESS SHARE. Both tables already exist, so no DDL lock is taken.
func init() {
	registerMigration(migration{ID: "20261114-deploy-record-projection-failed-step", After: []string{"20261109-deploy-record-projection"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT d.key,d.sha256,d.body,d.created_at FROM documents d JOIN deploy_record_projection p ON p.key=d.key AND p.sha256=d.sha256 WHERE strpos(d.key,'deploy/')=1`)
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

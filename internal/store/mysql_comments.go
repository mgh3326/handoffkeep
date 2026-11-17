package store

import (
	"context"
	"time"
)

// mysqlInsertTaskCommentTx is the isolated insert seam. B3 must compose it
// with validation, MCP audit and YouTrack projection before exposing it.
func mysqlInsertTaskCommentTx(ctx context.Context, tx transaction, taskID int64, author, body string, now time.Time) (int64, error) {
	if _, err := mysqlTransactionLock(ctx, tx, mysqlLockKey("id", "824180046"), false); err != nil {
		return 0, err
	}
	return tx.InsertID(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES(?,?,?,?)`, taskID, body, author, now.UTC())
}

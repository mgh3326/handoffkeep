package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// mysqlWriteTransaction retries a whole transaction, never a statement. A
// pinned connection lets us clear its trigger identity before returning it to
// the pool; an uncertain cleanup discards the physical connection.
func (s *Store) mysqlWriteTransaction(ctx context.Context, work func(transaction) error) error {
	_, err := s.mysqlWriteTransactionWithLock(ctx, "", work)
	return err
}

// mysqlWriteTransactionWithLock acquires a session lock before BEGIN and
// releases it after the transaction. 8241873404 uses the shared name hk:yt-sync.
func (s *Store) mysqlWriteTransactionWithLock(ctx context.Context, name string, work func(transaction) error) (bool, error) {
	conn, err := s.mysqlPool.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if name != "" {
		var got sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?,0)`, name).Scan(&got); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			return false, err
		}
		if !got.Valid {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			return false, fmt.Errorf("GET_LOCK returned NULL")
		}
		if got.Int64 != 1 {
			return false, nil
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var released sql.NullInt64
			if err := conn.QueryRowContext(cleanup, `SELECT RELEASE_LOCK(?)`, name).Scan(&released); err != nil || !released.Valid || released.Int64 != 1 {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}()
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, `SET @hk_txn = NULL`); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	for attempt := 0; ; attempt++ {
		err := mysqlWriteTransactionOnce(ctx, conn, work)
		var e *mysql.MySQLError
		if attempt != 0 || !errors.As(err, &e) || (e.Number != 1213 && e.Number != 1205) {
			return true, err
		}
	}
}

func mysqlWriteTransactionOnce(ctx context.Context, conn *sql.Conn, work func(transaction) error) error {
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET @hk_txn = ?`, token[:]); err != nil {
		return err
	}
	if err = work(sqlTransaction{tx}); err != nil {
		return err
	}
	if err = mysqlAllocateSync(ctx, tx, token[:]); err != nil {
		return err
	}
	return tx.Commit()
}

func mysqlAllocateSync(ctx context.Context, tx *sql.Tx, token []byte) error {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_log WHERE txn=? AND sync_id IS NULL)`, token).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		return nil
	}
	var last int64
	if err := tx.QueryRowContext(ctx, `SELECT max_sync_id FROM sync_state WHERE singleton=1 FOR UPDATE`).Scan(&last); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT entry_id FROM sync_log WHERE txn=? AND sync_id IS NULL ORDER BY entry_id`, token)
	if err != nil {
		return err
	}
	var entries []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range entries {
		last++
		result, err := tx.ExecContext(ctx, `UPDATE sync_log SET sync_id=? WHERE entry_id=? AND sync_id IS NULL`, last, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("allocate sync entry %d: affected %d rows", id, n)
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE sync_state SET max_sync_id=? WHERE singleton=1`, last)
	return err
}

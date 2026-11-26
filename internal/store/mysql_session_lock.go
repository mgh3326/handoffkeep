package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strconv"
	"time"
)

const mysqlYTSyncLock = "hk:yt-sync"

func mysqlTrySessionLock(ctx context.Context, pool *sql.DB, name string) (*sql.Conn, bool, error) {
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	return mysqlTrySessionLockOnConn(ctx, conn, name)
}

func mysqlTryWorkerSessionLock(ctx context.Context, pool *sql.DB, id WorkerLock) (*sql.Conn, string, bool, error) {
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, "", false, err
	}
	var database sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&database); err != nil {
		_ = conn.Close()
		return nil, "", false, err
	}
	if !database.Valid || database.String == "" {
		_ = conn.Close()
		return nil, "", false, fmt.Errorf("mysql worker lock has no selected database")
	}
	name := mysqlSessionLockName("worker", database.String+"\x00"+strconv.FormatInt(int64(id), 10))
	locked, held, err := mysqlTrySessionLockOnConn(ctx, conn, name)
	return locked, name, held, err
}

func mysqlTrySessionLockOnConn(ctx context.Context, conn *sql.Conn, name string) (*sql.Conn, bool, error) {
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?,0)`, name).Scan(&got); err != nil || !got.Valid {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		conn.Close()
		if err == nil {
			err = fmt.Errorf("GET_LOCK returned NULL")
		}
		return nil, false, err
	}
	if got.Int64 != 1 {
		conn.Close()
		return nil, false, nil
	}
	return conn, true, nil
}

func mysqlReleaseSessionLock(conn *sql.Conn, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var released sql.NullInt64
	err := conn.QueryRowContext(ctx, `SELECT RELEASE_LOCK(?)`, name).Scan(&released)
	if err != nil || !released.Valid || released.Int64 != 1 {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		if err == nil {
			err = fmt.Errorf("RELEASE_LOCK did not release %s", name)
		}
	}
	closeErr := conn.Close()
	if err != nil {
		return err
	}
	return closeErr
}

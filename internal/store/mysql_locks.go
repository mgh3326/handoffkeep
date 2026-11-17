package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
)

// mysqlLockKey gives every lock a stable, bounded binary identity. Callers
// must use the same namespace and value for every participant in a lock.
func mysqlLockKey(namespace, value string) []byte {
	h := sha256.New()
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(namespace)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(namespace))
	_, _ = h.Write([]byte(value))
	sum := h.Sum(nil)
	return sum
}

func mysqlSessionLockName(namespace, value string) string {
	return "hk:" + hex.EncodeToString(mysqlLockKey(namespace, value))[:60]
}

// mysqlTransactionLock holds the row until commit or rollback. Insert first so
// blocking lock identities need not be pre-populated. Try-lock identities must
// be provisioned in advance: an INSERT could itself wait on another holder.
// NOWAIT returns false only for contention; other errors reach the caller.
func mysqlTransactionLock(ctx context.Context, tx transaction, key []byte, try bool) (bool, error) {
	if !try {
		if _, err := tx.Exec(ctx, `INSERT INTO hk_locks(lock_key) VALUES (?) ON DUPLICATE KEY UPDATE lock_key=lock_key`, key); err != nil {
			return false, err
		}
	}
	query := `SELECT lock_key FROM hk_locks WHERE lock_key=? FOR UPDATE`
	if try {
		query += ` NOWAIT`
	}
	var found []byte
	if err := tx.QueryRow(ctx, query, key).Scan(&found); err != nil {
		var e *mysql.MySQLError
		if try && errors.As(err, &e) && e.Number == 3572 {
			return false, nil
		}
		return false, err
	}
	if len(found) == 0 {
		return false, fmt.Errorf("mysql lock row is empty")
	}
	return true, nil
}

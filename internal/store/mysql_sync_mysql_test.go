//go:build mysql

package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// Exercise the production transaction wrapper and a concurrent cursor reader.
func TestMySQLSyncCommitOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	firstSync := insertMySQLTestTask(t, s)
	const writers, perWriter = 10, 1000
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("%d/%d", w, i)
				err := s.mysqlWriteTransaction(ctx, func(tx transaction) error {
					_, err := mysqlInsertTaskCommentTx(ctx, tx, 1, "test", id, time.Now().UTC())
					return err
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	seen := make(map[string]bool, writers*perWriter)
	cursor, commentCursor := firstSync, int64(0)
	comments := make(map[string]bool, writers*perWriter)
	finished := false
	for {
		rows, err := s.mysqlPool.QueryContext(ctx, `SELECT sync_id,model_id FROM sync_log WHERE sync_id>? ORDER BY sync_id LIMIT 100`, cursor)
		if err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var id string
			var seq int64
			if err := rows.Scan(&seq, &id); err != nil {
				rows.Close()
				cancel()
				<-done
				t.Fatal(err)
			}
			if seq != cursor+1 || seen[id] {
				rows.Close()
				cancel()
				<-done
				t.Fatalf("cursor=%d seq=%d id=%s duplicate=%t", cursor, seq, id, seen[id])
			}
			cursor, seen[id], n = seq, true, n+1
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		commentRows, err := s.mysqlPool.QueryContext(ctx, `SELECT id,body FROM task_comments WHERE id>? ORDER BY id LIMIT 100`, commentCursor)
		if err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		cn := 0
		for commentRows.Next() {
			var id int64
			var body string
			if err := commentRows.Scan(&id, &body); err != nil {
				commentRows.Close()
				cancel()
				<-done
				t.Fatal(err)
			}
			if id <= commentCursor || comments[body] {
				commentRows.Close()
				cancel()
				<-done
				t.Fatalf("comment cursor=%d id=%d duplicate=%t", commentCursor, id, comments[body])
			}
			commentCursor, comments[body], cn = id, true, cn+1
		}
		err = commentRows.Err()
		commentRows.Close()
		if err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		if finished && n == 0 && cn == 0 {
			break
		}
		select {
		case <-done:
			finished = true
		default:
		}
	}
	close(errs)
	for err := range errs {
		t.Errorf("writer: %v", err)
	}
	if len(seen) != writers*perWriter {
		t.Errorf("saw %d rows, want %d", len(seen), writers*perWriter)
	}
	if len(comments) != writers*perWriter {
		t.Errorf("saw %d comments, want %d", len(comments), writers*perWriter)
	}
	var pending int
	if err := s.mysqlPool.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_log WHERE sync_id IS NULL`).Scan(&pending); err != nil || pending != 0 {
		t.Errorf("pending=%d err=%v", pending, err)
	}
}

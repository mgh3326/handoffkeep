package store

import (
	"errors"
	"testing"
	"time"
)

func TestSearchTimeoutEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset", "", defaultSearchTimeout},
		{"duration", "3s", 3 * time.Second},
		{"spaces", " 250ms ", 250 * time.Millisecond},
		{"not a duration", "soon", defaultSearchTimeout},
		{"zero", "0s", defaultSearchTimeout},
		{"negative", "-1s", defaultSearchTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HANDOFFKEEP_SEARCH_TIMEOUT", c.env)
			if got := searchTimeout(); got != c.want {
				t.Fatalf("searchTimeout() = %s, want %s", got, c.want)
			}
		})
	}
}

// A search blocked on a lock must give up at its deadline with
// ErrSearchTimeout, and the cancel must reach the server: the blocked backend
// has to leave the lock queue while the lock is still held. A client that
// merely stopped waiting would leave the statement queued until the lock is
// released.
func TestSearchTimeoutCancelsServerQuery(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `LOCK TABLE documents IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HANDOFFKEEP_SEARCH_TIMEOUT", "200ms")
	start := time.Now()
	_, err = s.Search(ctx, "배포", "docs", "", 10)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSearchTimeout) {
		t.Fatalf("Search under a held lock: err = %v, want ErrSearchTimeout", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Search returned after %s, want about its 200ms deadline", elapsed)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%FROM documents WHERE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d search backend(s) still queued on documents 5s after the deadline: the cancel did not reach the server", waiting)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

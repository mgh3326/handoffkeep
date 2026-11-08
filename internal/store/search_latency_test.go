package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

// TestSearchLatency measures Store.Search on a throwaway schema seeded to the
// size of production (about 56k documents and 56k tasks) with Korean text. It
// is off unless HANDOFFKEEP_SEARCH_LATENCY=1, which CI's search-latency run
// sets (hk 2658). Run it on a commit before a change and on the change, then
// compare result_sha for each query: equal digests mean an identical result
// set. Each query logs a search.plan block (EXPLAIN ANALYZE, BUFFERS of the
// statement Search runs), one search.latency line (p50 and p95 over
// HANDOFFKEEP_SEARCH_LATENCY_REPS calls) and one search.result line.
func TestSearchLatency(t *testing.T) {
	if os.Getenv("HANDOFFKEEP_SEARCH_LATENCY") != "1" {
		t.Skip("set HANDOFFKEEP_SEARCH_LATENCY=1 to run the search latency measurement")
	}
	s := schemaStore(t, "search_latency")
	ctx := t.Context()
	seedSearchLatency(ctx, t, s, 56000)
	reps := 5
	if raw := os.Getenv("HANDOFFKEEP_SEARCH_LATENCY_REPS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			t.Fatalf("HANDOFFKEEP_SEARCH_LATENCY_REPS=%q: want a positive integer", raw)
		}
		reps = n
	}
	for _, c := range searchLatencyQueries {
		measureSearchLatency(ctx, t, s, c.name, c.q, c.scope, reps)
	}
}

var searchLatencyQueries = []struct{ name, q, scope string }{
	{"ko2_beri", "베리", "all"},
	{"ko2_deploy", "배포", "all"},
	{"ko3_phrase", "배포 절차", "all"},
	{"en_deploy", "deploy", "all"},
	{"mixed", "deploy 배포", "all"},
	{"nohit", "zzqxnohit", "all"},
	{"tasks_ko2", "배포", "tasks"},
	{"tasks_ko3", "배포 절차", "tasks"},
}

// hangulSQL is a deterministic Korean string of n syllables in words of three,
// derived from the SQL expression key, so the seeded text is the same on every
// run and on every commit being compared.
func hangulSQL(key string, n int) string {
	return fmt.Sprintf(`(SELECT string_agg(chr(44032 + ((hashtext(%s || '.' || k::text) & 2147483647) %% 11172)) || CASE WHEN k %% 3 = 0 THEN ' ' ELSE '' END, '' ORDER BY k) FROM generate_series(1, %d) k)`, key, n)
}

func seedSearchLatency(ctx context.Context, t *testing.T, s *Store, n int) {
	t.Helper()
	stmts := []string{
		`INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at)
		 SELECT 'bench/' || g, 'note', 'bench', '',
		   ` + hangulSQL("g::text", 60) + `
		   || CASE WHEN g % 50 = 0 THEN ' 배포 절차 ' ELSE '' END
		   || CASE WHEN g % 7 = 0 THEN ' 베리 ' ELSE '' END
		   || CASE WHEN g % 11 = 0 THEN ' deploy 배포 ' ELSE '' END,
		   md5(g::text), 'bench', now() - (g || ' seconds')::interval, now() - (g || ' seconds')::interval
		 FROM generate_series(1, ` + strconv.Itoa(n) + `) g`,
		`INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at)
		 SELECT 'bench',
		   ` + hangulSQL("g::text || 't'", 12) + `
		   || CASE WHEN g % 40 = 0 THEN ' 배포 절차 점검 ' ELSE '' END
		   || CASE WHEN g % 9 = 0 THEN ' 베리 ' ELSE '' END,
		   'implement', 'backlog', 'bench', now() - (g || ' seconds')::interval, now(), now()
		 FROM generate_series(1, ` + strconv.Itoa(n) + `) g`,
		`ANALYZE documents`,
		`ANALYZE tasks`,
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func measureSearchLatency(ctx context.Context, t *testing.T, s *Store, name, q, scope string, reps int) {
	t.Helper()
	var stmt string
	var args []any
	capture := context.WithValue(ctx, searchCaptureKey{}, searchCapture(func(sql string, a []any) { stmt, args = sql, a }))
	first, firstErr := s.Search(capture, q, scope, "", 20)
	if stmt != "" {
		logSearchPlan(ctx, t, s, name, stmt, args)
	}

	durations := make([]time.Duration, 0, reps)
	errs := 0
	for i := 0; i < reps; i++ {
		start := time.Now()
		_, err := s.Search(ctx, q, scope, "", 20)
		durations = append(durations, time.Since(start))
		if err != nil {
			errs++
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[(len(durations)-1)/2]
	p95 := durations[(len(durations)*95+99)/100-1]
	log.Printf("search.latency query=%s scope=%s reps=%d errors=%d p50_ms=%.1f p95_ms=%.1f", name, scope, reps, errs, ms(p50), ms(p95))

	h := sha256.New()
	for _, x := range first {
		fmt.Fprintf(h, "%s|%s|%s|%s|%s|%d\n", x.Scope, x.Key, x.Session, x.Title, x.Snippet, x.CreatedAt.UnixNano())
	}
	log.Printf("search.result query=%s scope=%s rows=%d err=%q result_sha=%s", name, scope, len(first), errText(firstErr), hex.EncodeToString(h.Sum(nil)))
}

func logSearchPlan(ctx context.Context, t *testing.T, s *Store, name, stmt string, args []any) {
	t.Helper()
	rows, err := s.pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+stmt, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain %s scan: %v", name, err)
		}
		log.Printf("search.plan query=%s %s", name, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain %s: %v", name, err)
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func errText(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

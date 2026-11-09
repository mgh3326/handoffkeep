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

// TestSearchLatency measures Store.Search on synthetic data shaped to the
// production document count, body-size distribution, and common Korean and
// English terms. It is off unless HANDOFFKEEP_SEARCH_LATENCY=1. Compare
// result_sha across commits for each query; equal digests mean identical
// result sets. Each query logs EXPLAIN (ANALYZE, BUFFERS), latency and digest.
func TestSearchLatency(t *testing.T) {
	if os.Getenv("HANDOFFKEEP_SEARCH_LATENCY") != "1" {
		t.Skip("set HANDOFFKEEP_SEARCH_LATENCY=1 to run the search latency measurement")
	}
	s := schemaStore(t, "search_latency")
	ctx := t.Context()
	seedSearchLatency(ctx, t, s, 11075)
	reps := 5
	if raw := os.Getenv("HANDOFFKEEP_SEARCH_LATENCY_REPS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			t.Fatalf("HANDOFFKEEP_SEARCH_LATENCY_REPS=%q: want a positive integer", raw)
		}
		reps = n
	}
	for _, c := range searchLatencyQueries {
		p95 := measureSearchLatency(ctx, t, s, c.name, c.q, c.scope, reps)
		if c.name == "all_ko2_deploy" && (p95 >= time.Second || p95 <= 0) {
			t.Errorf("all-scope two-character Korean p95 = %s, want < 1s", p95)
		}
	}
}

var searchLatencyQueries = []struct{ name, q, scope string }{
	{"all_ko2_deploy", "배포", "all"},
	{"all_ko2_beri", "베리", "all"},
	{"all_ko3_phrase", "배포 절차", "all"},
	{"all_en_deploy", "deploy", "all"},
	{"all_mixed", "deploy 배포", "all"},
	{"all_nohit", "zzqxnohit", "all"},
	{"docs_ko2", "배포", "docs"},
	{"memory_ko2", "배포", "memory"},
	{"ctx_ko2", "배포", "ctx"},
	{"tasks_ko2", "배포", "tasks"},
	{"tasks_ko3", "배포 절차", "tasks"},
}

// hangulSQL is a deterministic Korean string of n syllables in words of three,
// derived from the SQL expression key, so the seeded text is the same on every
// run and on every commit being compared.
func hangulSQL(key string, n int) string {
	return fmt.Sprintf(`(SELECT string_agg(chr(44032 + ((hashtext(%s || '.' || k::text) & 2147483647) %% 11172)) || CASE WHEN k %% 3 = 0 THEN ' ' ELSE '' END, '' ORDER BY k) FROM generate_series(1, %d) k)`, key, n)
}

// seedEpoch pins the seeded timestamps. The result digest includes created_at,
// so now() would make every run's digest differ.
const seedEpoch = `'2026-01-01T00:00:00Z'::timestamptz`

func seedSearchLatency(ctx context.Context, t *testing.T, s *Store, n int) {
	t.Helper()
	stmts := []string{
		`INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at)
		 SELECT 'bench/' || g, 'note', 'bench', '',
		   prefix || left(repeat('운영 기록과 절차를 설명하는 synthetic document. English archive content. ',
		     ceil(GREATEST(0, body_chars-length(prefix))::numeric/61)::integer), GREATEST(0, body_chars-length(prefix))),
		   md5(g::text), 'bench', ` + seedEpoch + ` + (g || ' seconds')::interval, ` + seedEpoch + ` + (g || ' seconds')::interval
		 FROM (SELECT g,
		   CASE WHEN g % 7 = 0 THEN '배포 절차 ' ELSE '' END ||
		   CASE WHEN g % 11 = 0 THEN 'deploy release ' ELSE '' END ||
		   CASE WHEN g % 17 = 0 THEN '베리 ' ELSE '' END prefix,
		   CASE WHEN g <= 5537 THEN 3613
		     WHEN g <= 9967 THEN 3613 + ((hashtext(g::text) & 2147483647) % 11738)
		     WHEN g <= 10964 THEN 16987
		     WHEN g < ` + strconv.Itoa(n) + ` THEN 143693
		     ELSE 501157 END body_chars
		 FROM generate_series(1, ` + strconv.Itoa(n) + `) g) sized`,
		`INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at)
		 SELECT 'bench',
		   ` + hangulSQL("g::text || 't'", 12) + `
		   || CASE WHEN g % 40 = 0 THEN ' 배포 절차 점검 ' ELSE '' END
		   || CASE WHEN g % 9 = 0 THEN ' 베리 ' ELSE '' END,
		   'implement', 'backlog', 'bench', ` + seedEpoch + ` + (g || ' seconds')::interval, ` + seedEpoch + `, ` + seedEpoch + `
 		 FROM generate_series(1, 56000) g`,
		`INSERT INTO checkpoints(session,kind,title,body,refs,created_by,created_at)
		 SELECT 'bench','checkpoint','deploy status ' || g,
		   CASE WHEN g % 5 = 0 THEN '배포 절차 deploy ' ELSE '' END || repeat('운영 기록과 english archive ', 40),
		   '{}'::jsonb,'bench',` + seedEpoch + ` + (g || ' seconds')::interval
		 FROM generate_series(1, 2000) g`,
		`INSERT INTO memory(agent,name,description,memory_type,content,updated_by,updated_at)
		 SELECT 'bench','reference-' || g, '운영 기록 deploy', 'reference',
		   CASE WHEN g % 5 = 0 THEN '배포 절차 ' ELSE '' END || repeat('memory example content ', 40),
		   'bench',` + seedEpoch + ` + (g || ' seconds')::interval
		 FROM generate_series(1, 200) g`,
		`ANALYZE documents`,
		`ANALYZE tasks`,
		`ANALYZE checkpoints`,
		`ANALYZE memory`,
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func measureSearchLatency(ctx context.Context, t *testing.T, s *Store, name, q, scope string, reps int) time.Duration {
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
	return p95
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

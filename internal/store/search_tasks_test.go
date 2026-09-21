package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// searchTestStore returns a Store migrated inside a fresh throwaway schema so
// task-search tests never observe rows seeded by other suites sharing the test
// database.
func searchTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL task search tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("search_tasks_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, e := pgxpool.New(context.Background(), url)
		if e == nil {
			_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			c.Close()
		}
	})
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := &Store{pool: pool}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s, pool
}

func seedTaskRow(t *testing.T, pool *pgxpool.Pool, lane, title, state string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at) VALUES($1,$2,'implement',$3,'search-test',now(),now()) RETURNING id`, lane, title, state).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func resultKeys(xs []SearchResult) []string {
	out := []string{}
	for _, x := range xs {
		out = append(out, x.Key)
	}
	return out
}

func TestSearchTasksExactIDLeads(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	b := seedTaskRow(t, pool, "lane-a", "unrelated beta work", "backlog")
	// A task whose title contains the other's id digits must never outrank the
	// exact id match (mutant A-7a).
	a := seedTaskRow(t, pool, "lane-a", fmt.Sprintf("follow-up on #%d regression", b), "backlog")
	for _, q := range []string{"#" + strconv.FormatInt(b, 10), strconv.FormatInt(b, 10)} {
		xs, err := s.Search(ctx, q, "tasks", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(xs) == 0 || xs[0].Key != strconv.FormatInt(b, 10) {
			t.Fatalf("q=%s first=%+v", q, resultKeys(xs))
		}
		pos := -1
		for i, x := range xs {
			if x.Key == strconv.FormatInt(a, 10) {
				pos = i
			}
		}
		if pos <= 0 {
			t.Fatalf("q=%s title-hit position=%d keys=%v", q, pos, resultKeys(xs))
		}
	}
	xs, err := s.Search(ctx, "#999999999", "tasks", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 0 {
		t.Fatalf("missing id must be empty, got %v", resultKeys(xs))
	}
	xs, err = s.Search(ctx, "99999999999999999999999", "tasks", "", 10)
	if err != nil || len(xs) != 0 {
		t.Fatalf("oversized numeric q: xs=%v err=%v", resultKeys(xs), err)
	}
}

func TestSearchTasksCapAndTruncated(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		seedTaskRow(t, pool, "lane-a", fmt.Sprintf("capprobe target %d", i), "backlog")
	}
	xs, err := s.Search(ctx, "capprobe", "tasks", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 5 {
		t.Fatalf("limit=5 got %d", len(xs))
	}
	for _, x := range xs {
		if !x.Truncated {
			t.Fatal("truncated page must mark every row")
		}
	}
	xs, err = s.Search(ctx, "capprobe", "tasks", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 7 {
		t.Fatalf("limit=10 got %d", len(xs))
	}
	for _, x := range xs {
		if x.Truncated {
			t.Fatal("complete page must not claim truncation")
		}
	}
	// Default page is 20 and the hard cap is 50.
	for i := 0; i < 55; i++ {
		seedTaskRow(t, pool, "lane-a", fmt.Sprintf("capfill target %d", i), "backlog")
	}
	xs, err = s.Search(ctx, "capfill", "tasks", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 20 {
		t.Fatalf("default limit got %d", len(xs))
	}
	xs, err = s.Search(ctx, "capfill", "tasks", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 50 || !xs[0].Truncated {
		t.Fatalf("cap 50 got %d truncated=%v", len(xs), xs[0].Truncated)
	}
}

func TestSearchTasksIncludesClosedStates(t *testing.T) {
	s, pool := searchTestStore(t)
	m := seedTaskRow(t, pool, "lane-a", "closedprobe merged work", "merged")
	d := seedTaskRow(t, pool, "lane-a", "closedprobe dropped work", "dropped")
	seedTaskRow(t, pool, "lane-a", "closedprobe open work", "backlog")
	xs, err := s.Search(context.Background(), "closedprobe", "tasks", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	keys := resultKeys(xs)
	for _, want := range []int64{m, d} {
		found := false
		for _, k := range keys {
			if k == strconv.FormatInt(want, 10) {
				found = true
			}
		}
		if !found {
			t.Fatalf("closed task %d missing from %v", want, keys)
		}
	}
	if len(xs) != 3 {
		t.Fatalf("want 3 got %v", keys)
	}
}

func TestSearchTasksKoreanQueries(t *testing.T) {
	s, pool := searchTestStore(t)
	deploy := seedTaskRow(t, pool, "lane-a", "배포 대기 보류 작업", "backlog")
	install := seedTaskRow(t, pool, "lane-a", "설치 절차 점검", "backlog")
	for q, want := range map[string]int64{"배포 대기": deploy, "설치": install, "배포 대": deploy} {
		xs, err := s.Search(context.Background(), q, "tasks", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(xs) == 0 || xs[0].Key != strconv.FormatInt(want, 10) {
			t.Fatalf("q=%q keys=%v", q, resultKeys(xs))
		}
	}
}

func TestSearchTasksWildcardLiteral(t *testing.T) {
	s, pool := searchTestStore(t)
	pct := seedTaskRow(t, pool, "lane-a", "100% rollout plan", "backlog")
	us := seedTaskRow(t, pool, "lane-a", "under_score task", "backlog")
	seedTaskRow(t, pool, "lane-a", "plain ordinary work", "backlog")
	ctx := context.Background()
	xs, err := s.Search(ctx, "%", "tasks", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 || xs[0].Key != strconv.FormatInt(pct, 10) {
		t.Fatalf("%% must be literal, got %v", resultKeys(xs))
	}
	xs, err = s.Search(ctx, "_", "tasks", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 || xs[0].Key != strconv.FormatInt(us, 10) {
		t.Fatalf("_ must be literal, got %v", resultKeys(xs))
	}
	for _, q := range []string{"'", "''; DROP TABLE tasks;--", "\"", `a\\b`} {
		if _, err = s.Search(ctx, q, "tasks", "", 10); err != nil {
			t.Fatalf("q=%q err=%v", q, err)
		}
	}
	long := make([]byte, 513)
	for i := range long {
		long[i] = 'x'
	}
	if _, err = s.Search(ctx, string(long), "tasks", "", 10); err == nil {
		t.Fatal("oversized query must be rejected")
	}
}

func TestSearchTasksCommentsOptional(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	hit := seedTaskRow(t, pool, "lane-a", "commentprobe in title", "backlog")
	other := seedTaskRow(t, pool, "lane-a", "unrelated title here", "backlog")
	// Table absent (pre-#537 schema): search must still succeed.
	xs, err := s.Search(ctx, "commentprobe", "tasks", "", 10)
	if err != nil || len(xs) != 1 {
		t.Fatalf("absent task_comments: xs=%v err=%v", resultKeys(xs), err)
	}
	if _, err = pool.Exec(ctx, `CREATE TABLE task_comments(id BIGSERIAL PRIMARY KEY, task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, body TEXT NOT NULL, author TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,'코멘트 본문 고유키워드','tester',now())`, other); err != nil {
		t.Fatal(err)
	}
	// Comment-body hit surfaces the owning task.
	xs, err = s.Search(ctx, "고유키워드", "tasks", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 || xs[0].Key != strconv.FormatInt(other, 10) {
		t.Fatalf("comment match keys=%v", resultKeys(xs))
	}
	// A task matching title and comment yields one row.
	if _, err = pool.Exec(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,'commentprobe in body','tester',now())`, hit); err != nil {
		t.Fatal(err)
	}
	xs, err = s.Search(ctx, "commentprobe", "tasks", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 {
		t.Fatalf("duplicate task rows: %v", resultKeys(xs))
	}
}

func TestSearchTasksLaneFilterAndOtherScopes(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	a := seedTaskRow(t, pool, "lane-a", "laneprobe alpha", "backlog")
	seedTaskRow(t, pool, "lane-b", "laneprobe beta", "backlog")
	xs, err := s.Search(ctx, "laneprobe", "tasks", "lane-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 || xs[0].Key != strconv.FormatInt(a, 10) {
		t.Fatalf("lane filter keys=%v", resultKeys(xs))
	}
	// scope "all" must not grow task rows; existing scopes keep working.
	if _, err = s.CreateCheckpoint(ctx, Checkpoint{Session: "ss", Kind: "checkpoint", Title: "laneprobe checkpoint", Body: "x", CreatedBy: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.PutDocument(ctx, Document{Key: "k/laneprobe", Kind: "note", Body: "laneprobe body", CreatedBy: "t"}); err != nil {
		t.Fatal(err)
	}
	xs, err = s.Search(ctx, "laneprobe", "all", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range xs {
		if x.Scope == "tasks" {
			t.Fatalf("scope=all leaked tasks row %+v", x)
		}
	}
	if len(xs) != 2 {
		t.Fatalf("scope=all want ctx+docs got %v", xs)
	}
	xs, err = s.Search(ctx, "laneprobe", "docs", "", 10)
	if err != nil || len(xs) != 1 || xs[0].Scope != "docs" {
		t.Fatalf("docs scope=%v", xs)
	}
}

func TestSearchDocsLinkBodyDoc(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	if _, _, err := s.PutDocument(ctx, Document{Key: "k/bodydoc", Kind: "note", Body: "docprobe body", CreatedBy: "t"}); err != nil {
		t.Fatal(err)
	}
	// Without tasks.body_doc the docs result carries no task link.
	xs, err := s.Search(ctx, "docprobe", "docs", "", 10)
	if err != nil || len(xs) != 1 {
		t.Fatalf("docs xs=%v err=%v", xs, err)
	}
	if len(xs[0].Refs["tasks"]) != 0 {
		t.Fatalf("unexpected task link without body_doc: %+v", xs[0].Refs)
	}
	if _, err = pool.Exec(ctx, `ALTER TABLE tasks ADD COLUMN body_doc TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	id := seedTaskRow(t, pool, "lane-a", "has body doc", "backlog")
	if _, err = pool.Exec(ctx, `UPDATE tasks SET body_doc='k/bodydoc' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	xs, err = s.Search(ctx, "docprobe", "docs", "", 10)
	if err != nil || len(xs) != 1 {
		t.Fatalf("docs xs=%v err=%v", xs, err)
	}
	got := xs[0].Refs["tasks"]
	if len(got) != 1 || got[0] != strconv.FormatInt(id, 10) {
		t.Fatalf("task link=%v", got)
	}
}

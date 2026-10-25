package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

func wrkCIStore(t *testing.T) *Store {
	t.Helper()
	database := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if database == "" {
		if os.Getenv("CI") == "true" || os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("CI must run PostgreSQL authority tests")
		}
		t.Skip("PostgreSQL authority tests run only in CI")
	}
	ctx := t.Context()
	admin, e := pgxpool.New(ctx, database)
	if e != nil {
		t.Fatal(e)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	schema, role := "wrk_test_"+suffix, "wrk_runtime_"+suffix
	if _, e = admin.Exec(ctx, `CREATE SCHEMA `+schema); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(database)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	owner, e := Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	if e = owner.MigrateWrk(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = admin.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT`); e != nil {
		t.Fatal(e)
	}
	for _, sql := range []string{`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role, `GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role, `GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + role, `REVOKE UPDATE,DELETE,TRUNCATE ON ` + schema + `.wrk_records,` + schema + `.wrk_contents,` + schema + `.wrk_attempts FROM ` + role} {
		if _, e = admin.Exec(ctx, sql); e != nil {
			t.Fatal(e)
		}
	}
	q.Set("role", role)
	u.RawQuery = q.Encode()
	runtime, e := OpenRuntime(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		runtime.Close()
		owner.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_, _ = admin.Exec(context.Background(), `DROP ROLE `+role)
		admin.Close()
	})
	b, e := os.ReadFile(filepath.Join("..", "..", "testdata", "o3", "vectors", "transition.json"))
	if e != nil {
		t.Fatal(e)
	}
	var pack struct {
		Cases []struct {
			Given struct {
				Policy json.RawMessage `json:"policy"`
			} `json:"given"`
		} `json:"cases"`
	}
	if json.Unmarshal(b, &pack) != nil {
		t.Fatal("policy fixture")
	}
	policy, e := wrkrecords.ParsePolicy(pack.Cases[0].Given.Policy)
	if e != nil {
		t.Fatal(e)
	}
	if e = runtime.ConfigureWrk(ctx, "enforce", policy, wrkrecords.NewUUID(), false); e != nil {
		t.Fatal(e)
	}
	_, e = owner.pool.Exec(ctx, `INSERT INTO tasks(id,lane,parent_lane,title,kind,state,claimed_by,created_by,created_at,updated_at) VALUES(9001,'builder-a','director-1','authority test','implement','backlog','','test',now(),now()),(9002,'builder-b','director-1','second task','implement','backlog','','test',now(),now())`)
	if e != nil {
		t.Fatal(e)
	}
	return runtime
}
func storeClaim(job, owner string, task int64) wrkrecords.Object {
	return wrkrecords.Object{"job_id": job, "owner_lane": owner, "parent_lane": "director-1", "task_id": task, "host_client_id": "desktop", "role": "builder", "tier": "T3", "review_of": nil, "predecessor_job_id": nil, "supersedes": nil}
}
func wrkStoreAppend(t *testing.T, s *Store, actor, job, kind string, p wrkrecords.Object) wrkrecords.WriteResponse {
	t.Helper()
	claim, seq := int64(0), int64(0)
	if got, e := s.WrkJob(t.Context(), actor, job); e == nil {
		claim, seq = got.Summary.ClaimSeq, got.Summary.SnapshotSeq
	}
	r, e := s.WrkAppend(t.Context(), actor, job, wrkrecords.NewUUID(), wrkrecords.Request{Kind: kind, ClaimSeq: claim, ExpectedSeq: seq, Payload: p})
	if e != nil {
		t.Fatalf("%s/%s: %v", job, kind, e)
	}
	return r
}
func storeOpen(head string) wrkrecords.Object {
	return wrkrecords.Object{"head": head, "tester_job_id": nil, "extension_grant_seq": nil, "repeat_grant_seq": nil}
}
func storeActivate(t *testing.T, s *Store, job, owner string, task int64) {
	t.Helper()
	wrkStoreAppend(t, s, "director-1", job, "claim", storeClaim(job, owner, task))
	wrkStoreAppend(t, s, "director-1", job, "activate", wrkrecords.Object{"reason_code": "test_activation", "seal_seq": nil})
}
func TestWrkRuntimeCannotMutateHistory(t *testing.T) {
	s := wrkCIStore(t)
	for _, q := range []string{`UPDATE wrk_records SET seq=seq WHERE false`, `DELETE FROM wrk_records WHERE false`, `UPDATE wrk_contents SET size_bytes=size_bytes WHERE false`, `DELETE FROM wrk_contents WHERE false`} {
		if _, e := s.pool.Exec(t.Context(), q); e == nil {
			t.Fatalf("runtime privilege permitted %s", q)
		}
	}
}
func TestWrkConcurrentAppendAndCommitReplay(t *testing.T) {
	s := wrkCIStore(t)
	storeActivate(t, s, "job-a", "builder-a", 9001)
	wrkStoreAppend(t, s, "desktop", "job-a", "round_open", storeOpen(strings.Repeat("a", 40)))
	wrkStoreAppend(t, s, "desktop", "job-a", "round_open", storeOpen(strings.Repeat("b", 40)))
	job, e := s.WrkJob(t.Context(), "desktop", "job-a")
	if e != nil {
		t.Fatal(e)
	}
	req := wrkrecords.Request{Kind: "round_open", ClaimSeq: job.Summary.ClaimSeq, ExpectedSeq: job.Summary.SnapshotSeq, Payload: storeOpen(strings.Repeat("c", 40))}
	keys := []string{wrkrecords.NewUUID(), wrkrecords.NewUUID()}
	results := make([]wrkrecords.WriteResponse, 2)
	errs := make([]error, 2)
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			results[i], errs[i] = s.WrkAppend(t.Context(), "desktop", "job-a", keys[i], req)
		}(i)
	}
	group.Wait()
	wins := 0
	winner := 0
	for i, err := range errs {
		if err == nil {
			wins++
			winner = i
		} else if wrkrecords.Code(err) != "sequence_conflict" {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("winners=%d", wins)
	}
	replay, e := s.WrkAppend(t.Context(), "desktop", "job-a", keys[winner], req)
	if e != nil || replay.Created || replay.Record.RecordHash != results[winner].Record.RecordHash {
		t.Fatalf("lost reply replay: %v", e)
	}
	latest, e := s.WrkJob(t.Context(), "desktop", "job-a")
	if e != nil || latest.Summary.Used != 3 {
		t.Fatalf("used=%d err=%v", latest.Summary.Used, e)
	}
	req.ExpectedSeq = latest.Summary.SnapshotSeq
	req.Payload = storeOpen(strings.Repeat("d", 40))
	_, e = s.WrkAppend(t.Context(), "desktop", "job-a", wrkrecords.NewUUID(), req)
	if wrkrecords.Code(e) != "round_cap" {
		t.Fatalf("fourth ordinary round: %v", e)
	}
}
func TestWrkGrantConsumersRaceAndVoidNeverRefunds(t *testing.T) {
	s := wrkCIStore(t)
	storeActivate(t, s, "job-a", "builder-a", 9001)
	for _, h := range []string{"a", "b", "c"} {
		wrkStoreAppend(t, s, "desktop", "job-a", "round_open", storeOpen(strings.Repeat(h, 40)))
	}
	grant := wrkStoreAppend(t, s, "director-1", "job-a", "grant", wrkrecords.Object{"grant_type": "extension", "round_no": 4, "head": strings.Repeat("d", 40), "extra": 1, "earlier_round_no": nil, "tester_job_id": nil, "expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano), "reason_code": "race"})
	p := storeOpen(strings.Repeat("d", 40))
	p["extension_grant_seq"] = grant.Record.Seq
	r := wrkrecords.Request{Kind: "round_open", ClaimSeq: 1, ExpectedSeq: grant.Record.Seq, Payload: p}
	var group sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			_, errs[i] = s.WrkAppend(t.Context(), "desktop", "job-a", wrkrecords.NewUUID(), r)
		}(i)
	}
	group.Wait()
	wins := 0
	for _, e := range errs {
		if e == nil {
			wins++
		} else if wrkrecords.Code(e) != "sequence_conflict" {
			t.Fatal(e)
		}
	}
	if wins != 1 {
		t.Fatalf("grant winners %d", wins)
	}
	v := wrkStoreAppend(t, s, "director-1", "job-a", "grant", wrkrecords.Object{"grant_type": "void", "round_no": 4, "head": nil, "extra": 0, "earlier_round_no": nil, "tester_job_id": nil, "expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano), "reason_code": "void"})
	void := wrkStoreAppend(t, s, "desktop", "job-a", "void", wrkrecords.Object{"round_no": 4, "grant_seq": v.Record.Seq, "reason_code": "abort"})
	r.ExpectedSeq = void.Record.Seq
	_, e := s.WrkAppend(t.Context(), "desktop", "job-a", wrkrecords.NewUUID(), r)
	if wrkrecords.Code(e) != "grant_consumed" {
		t.Fatalf("refunded consumed grant: %v", e)
	}
}
func TestWrkCrossJobOrderEndRace(t *testing.T) {
	s := wrkCIStore(t)
	storeActivate(t, s, "job-a", "builder-a", 9001)
	storeActivate(t, s, "job-z", "builder-b", 9002)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var group sync.WaitGroup
	errs := make([]error, 2)
	for i, job := range []string{"job-z", "job-a"} {
		group.Add(1)
		go func(i int, job string) {
			defer group.Done()
			_, errs[i] = s.WrkAppend(ctx, "desktop", job, wrkrecords.NewUUID(), wrkrecords.Request{Kind: "end", ClaimSeq: 1, ExpectedSeq: 2, Payload: wrkrecords.Object{"end_type": "reaped", "reason_code": "race"}})
		}(i, job)
	}
	group.Wait()
	for _, e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
}

func TestWrkFourStartupCases(t *testing.T) {
	// Case 1: default off on a fresh legacy schema performs no O3 DDL.
	s := wrkCIStore(t)
	runtimeURL := s.pool.Config().ConnString()
	u, e := url.Parse(runtimeURL)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Del("role")
	ownerSchema := strings.Split(q.Get("search_path"), ",")[0]
	q.Set("search_path", ownerSchema+"_legacy,public")
	u.RawQuery = q.Encode()
	ownerURL := u.String()
	admin, e := pgxpool.New(t.Context(), runtimeURL)
	if e != nil {
		t.Fatal(e)
	}
	admin.Close()
	// Use the existing owner connection string, not the runtime role, for DDL.
	original := q.Get("search_path")
	q.Set("search_path", ownerSchema+",public")
	u.RawQuery = q.Encode()
	provision, e := pgxpool.New(t.Context(), u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer provision.Close()
	legacySchema := strings.Split(original, ",")[0]
	if _, e = provision.Exec(t.Context(), `CREATE SCHEMA `+legacySchema); e != nil {
		t.Fatal(e)
	}
	defer provision.Exec(context.Background(), `DROP SCHEMA `+legacySchema+` CASCADE`)
	off, e := OpenServer(t.Context(), ownerURL, "", "off", nil, "", false)
	if e != nil {
		t.Fatal(e)
	}
	var count int
	if e = off.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relname LIKE 'wrk_%'`).Scan(&count); e != nil || count != 0 {
		t.Fatalf("off created O3 tables: count=%d err=%v", count, e)
	}
	off.Close()
	// Case 2: explicit distinct migration path provisions the O3 schema.
	q.Set("search_path", ownerSchema+",public")
	u.RawQuery = q.Encode()
	migrateURL := u.String()
	enabled, e := OpenServer(t.Context(), runtimeURL, migrateURL, "enforce", s.WrkEngine().Policy, s.WrkEngine().Generation, false)
	if e != nil {
		t.Fatal(e)
	}
	enabled.Close()
	// Case 3: installed tables enforce role safety even while API off.
	off, e = OpenServer(t.Context(), runtimeURL, "", "off", nil, "", false)
	if e != nil {
		t.Fatal(e)
	}
	off.Close()
	if bad, e := OpenServer(t.Context(), migrateURL, "", "off", nil, "", false); e == nil {
		bad.Close()
		t.Fatal("off accepted migration owner as runtime")
	}
	// Case 4: enabled startup never falls back to a single URL or boot UUID.
	for _, setting := range []struct{ migration, generation string }{{"", s.WrkEngine().Generation}, {runtimeURL, s.WrkEngine().Generation}, {migrateURL, ""}} {
		if bad, e := OpenServer(t.Context(), runtimeURL, setting.migration, "enforce", s.WrkEngine().Policy, setting.generation, false); e == nil {
			bad.Close()
			t.Fatal("enabled startup accepted missing distinct migration path/generation")
		}
	}
}

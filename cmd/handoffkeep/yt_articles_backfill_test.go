package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// ybfRun runs one articles-backfill and decodes its report.
func ybfRun(t *testing.T, args ...string) (ytArticlesReport, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(args, &out, io.Discard)
	var r ytArticlesReport
	if jerr := json.Unmarshal(out.Bytes(), &r); jerr != nil {
		t.Fatalf("%v: output %q is not a report (%v; run err %v)", args, out.String(), jerr, err)
	}
	return r, err
}

// ybfAction is document doc's action in r.
func ybfAction(t *testing.T, r ytArticlesReport, doc int64) ytArticleAction {
	t.Helper()
	for _, a := range r.Actions {
		if a.DocumentID == doc {
			return a
		}
	}
	t.Fatalf("document %d has no action in %+v", doc, r.Actions)
	return ytArticleAction{}
}

// TestArticlesBackfillPlansThenEnqueues (hk 5265): a dry run plans every
// missing document and writes nothing; --apply prepares one op per missing
// document through the protocol, --limit bounds the ops, a rerun prepares no
// second op, one root-capable create per family and project waits for its
// root, a family whose root create is uncertain is held (per project), a moved
// candidate enqueues nothing, and a long document waiting on its family's root
// is shown and counted.
func TestArticlesBackfillPlansThenEnqueues(t *testing.T) {
	_, dbURL := testdb.Schema(t, "yta_enqueue")
	ctx := t.Context()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	fake := &ytartFake{hits: map[string]int{}}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	tenant := u.Host

	for _, p := range []store.YouTrackProject{{HKProject: "rda", ProjectID: "0-7", ShortName: "RDA"}, {HKProject: "sma", ProjectID: "0-8", ShortName: "SMA"}} {
		if _, err := st.AddTaskProject(ctx, p.HKProject, "test"); err != nil {
			t.Fatal(err)
		}
		if err := st.PutYouTrackProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	newTask := func(project string) int64 {
		t.Helper()
		p := project
		task, err := st.CreateTask(ctx, store.Task{Lane: "ybf-lane", Title: "enqueue", Kind: "implement", CreatedBy: "test", Project: &p})
		if err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	rdaTask := newTask("rda")
	put := func(key, body string, taskID *int64) int64 {
		t.Helper()
		d, _, err := st.PutDocument(ctx, store.Document{Key: key, Kind: "note", Body: body, CreatedBy: "test", TaskID: taskID})
		if err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	// m1 and m5 are short missing documents of family note on rda, m2 a long
	// one of the same family (its split waits on the family's root). u0 and u1
	// are shared documents of family uncertain: one root-capable create per
	// family and project, so u1 waits for u0's root.
	m1 := put("note/rda/fresh", "fresh", &rdaTask)
	m2 := put("note/rda/huge", strings.Repeat("line\n", 50*1024), &rdaTask)
	m5 := put("note/rda/second", "second", &rdaTask)
	u0 := put("uncertain/first", "first", nil)
	u1 := put("uncertain/second", "second", nil)

	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", "fake-test-key")
	t.Setenv("HANDOFFKEEP_YT_DOCS_MODE", "on")
	t.Setenv("HANDOFFKEEP_YT_ARTICLE_SPLIT", "on")
	// The store is the same gate the command opens (for the direct store calls).
	st.SetYTDocsMode(store.YTDocsOn, tenant, map[string]store.YTArticleProject{
		store.YTArticleBucketShared:     {ID: "0-3", ShortName: "SHRDOC"},
		store.YTArticleBucketUnassigned: {ID: "0-1", ShortName: "HK"},
	})
	st.SetYTArticleSplit(true)

	create, split := string(ytwrite.KindArticleCreate), string(ytwrite.KindArticleSplit)
	ops := func(kind string, doc int64) int {
		t.Helper()
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM yt_write_ops WHERE principal = $1 AND kind = $2 AND entity_id = $3`,
			store.YTDocsPrincipal, kind, strconv.FormatInt(doc, 10)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	total := func() int {
		t.Helper()
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM yt_write_ops WHERE principal = $1`, store.YTDocsPrincipal).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// candidate is the listed candidate of doc, as classification verified it.
	candidate := func(doc int64) store.YTArticleCandidate {
		t.Helper()
		docs, err := st.YTArticleDocs(ctx, "", 0, store.YTArticleDocPageMax)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range docs {
			if d.DocumentID == doc {
				return d.YTArticleCandidate
			}
		}
		t.Fatalf("document %d is not listed", doc)
		return store.YTArticleCandidate{}
	}

	// A dry run (the default) plans every missing document, reads YouTrack
	// and writes nothing. m5 and u1 wait on their family's root-capable create.
	dry, err := ybfRun(t, "yt", "articles-backfill")
	if err != nil {
		t.Fatal(err)
	}
	if dry.Mode != "dry_run" || dry.Enqueued == nil || *dry.Enqueued != 3 || fake.writes != 0 || total() != 0 {
		t.Fatalf("dry run = mode %q, enqueued %v; %d writes, %d ops", dry.Mode, dry.Enqueued, fake.writes, total())
	}
	if a := ybfAction(t, dry, m1); a.Action != "enqueue_create" || a.Class != ytArticleMissing {
		t.Errorf("m1 dry-run action = %+v", a)
	}
	if a := ybfAction(t, dry, m2); a.Action != "enqueue_split" || a.Reason != store.YTArticleWaitingFamilyRoot {
		t.Errorf("m2 dry-run action = %+v, want enqueue_split waiting on its family root", a)
	}
	if a := ybfAction(t, dry, m5); a.Action != "wait" || a.Reason != "family_root_pending" {
		t.Errorf("m5 dry-run action = %+v, want wait for the family's pending root create", a)
	}
	if a := ybfAction(t, dry, u1); a.Action != "wait" || a.Reason != "family_root_pending" {
		t.Errorf("u1 dry-run action = %+v, want wait for u0's pending root create", a)
	}

	// --apply --limit 1 prepares the first document's create and nothing more.
	r, err := ybfRun(t, "yt", "articles-backfill", "--apply", "--limit", "1", "--by", "test:enqueue")
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != "apply" || r.Enqueued == nil || *r.Enqueued != 1 || r.BoundNow == nil || *r.BoundNow != 0 {
		t.Fatalf("apply --limit 1 = mode %q, enqueued %v, bound %v", r.Mode, r.Enqueued, r.BoundNow)
	}
	if a := ybfAction(t, r, m1); a.Action != "create_prepared" {
		t.Errorf("m1 action = %+v, want create_prepared", a)
	}
	if a := ybfAction(t, r, m2); a.Action != "wait" || a.Reason != "limit" {
		t.Errorf("m2 action = %+v, want wait for the limit", a)
	}
	var target string
	if err := conn.QueryRow(ctx, `SELECT yt_target_id FROM yt_write_ops WHERE principal = $1 AND kind = $2 AND entity_id = $3`,
		store.YTDocsPrincipal, create, strconv.FormatInt(m1, 10)).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != "0-7" || ops(create, m1) != 1 || total() != 1 {
		t.Fatalf("after --limit 1: m1's op targets %q, %d create ops for m1, %d ops in all", target, ops(create, m1), total())
	}

	// --apply prepares the rest: the split of m2 and u0, and not m5 or u1,
	// whose family's root create is still pending.
	r, err = ybfRun(t, "yt", "articles-backfill", "--apply", "--by", "test:enqueue")
	if err != nil {
		t.Fatal(err)
	}
	if r.Enqueued == nil || *r.Enqueued != 2 {
		t.Fatalf("apply enqueued %v, want 2", r.Enqueued)
	}
	if a := ybfAction(t, r, u1); a.Action != "wait" || a.Reason != "family_root_pending" {
		t.Errorf("u1 action = %+v, want wait for u0's pending root create", a)
	}
	if a := ybfAction(t, r, m1); a.Action != "create_exists" {
		t.Errorf("m1 on the second apply = %+v, want create_exists", a)
	}
	if a := ybfAction(t, r, m2); a.Action != "split_prepared" {
		t.Errorf("m2 action = %+v, want split_prepared", a)
	}
	if ops(create, m1) != 1 || ops(split, m2) != 1 || ops(create, u0) != 1 || ops(create, u1) != 0 || ops(create, m5) != 0 || total() != 3 {
		t.Fatalf("after --apply: m1 %d, m2 split %d, u0 %d, u1 %d, m5 %d, %d ops in all", ops(create, m1), ops(split, m2), ops(create, u0), ops(create, u1), ops(create, m5), total())
	}

	// The store refuses a second root-capable create while the family's first
	// one is unresolved, even when the command did not plan it.
	if got, err := st.PrepareYTArticleBackfill(ctx, m5, time.Now().UTC(), candidate(m5)); err != nil || got != "family_root_pending" || ops(create, m5) != 0 {
		t.Fatalf("m5 at the store = %q, %v with %d ops; want family_root_pending and no op", got, err, ops(create, m5))
	}

	// A rerun prepares nothing: no second op for any document.
	r, err = ybfRun(t, "yt", "articles-backfill", "--apply", "--by", "test:enqueue")
	if err != nil {
		t.Fatal(err)
	}
	if r.Enqueued == nil || *r.Enqueued != 0 || total() != 3 {
		t.Fatalf("rerun enqueued %v, %d ops in all; want none", r.Enqueued, total())
	}
	if a := ybfAction(t, r, m2); a.Action != "split_exists" {
		t.Errorf("m2 on the rerun = %+v, want split_exists", a)
	}
	// --limit counts only the ops a run would add: with every op present a
	// dry run with --limit 1 plans none and waits on none.
	if dry, err := ybfRun(t, "yt", "articles-backfill", "--limit", "1"); err != nil || *dry.Enqueued != 0 || ybfAction(t, dry, m1).Action != "create_exists" || ybfAction(t, dry, m2).Action != "split_exists" {
		t.Fatalf("dry run --limit 1 with every op present = enqueued %v, %v; %+v", dry.Enqueued, err, dry.Actions)
	}

	// A family whose root create is uncertain (remote_unknown, no root
	// recorded) gets no new op on its project: u2 waits.
	u2 := put("uncertain/third", "third", nil)
	// The protocol moves an op prepared, then dispatched, then remote_unknown;
	// a trigger refuses prepared to remote_unknown directly.
	for _, state := range []string{"dispatched", "remote_unknown"} {
		if _, err := conn.Exec(ctx, `UPDATE yt_write_ops SET state = $1, dispatched_at = CASE WHEN $1 = 'dispatched' THEN now() ELSE dispatched_at END WHERE principal = $2 AND kind = $3 AND entity_id = $4`,
			state, store.YTDocsPrincipal, create, strconv.FormatInt(u0, 10)); err != nil {
			t.Fatal(err)
		}
	}
	r, err = ybfRun(t, "yt", "articles-backfill", "--apply", "--by", "test:enqueue")
	if err != nil {
		t.Fatal(err)
	}
	if a := ybfAction(t, r, u2); a.Action != "wait" || a.Reason != "family_root_uncertain" {
		t.Errorf("u2 action = %+v, want wait for the uncertain family root", a)
	}
	if ops(create, u2) != 0 || ops(create, u0) != 1 || total() != 3 {
		t.Fatalf("uncertain family: u2 has %d ops, u0 %d, %d in all", ops(create, u2), ops(create, u0), total())
	}

	// The hold is per project: a document of the same family on the rda
	// project is not held by the shared project's uncertain root.
	u3 := put("uncertain/rda-doc", "rda doc", &rdaTask)
	if r, err = ybfRun(t, "yt", "articles-backfill", "--apply", "--by", "test:enqueue"); err != nil {
		t.Fatal(err)
	}
	if a := ybfAction(t, r, u3); a.Action != "create_prepared" || ops(create, u3) != 1 || total() != 4 {
		t.Fatalf("u3 (rda project) = %+v with %d ops in all %d; want create_prepared", a, ops(create, u3), total())
	}

	// A task move committed before the backfill's locks: the document no
	// longer reads as the candidate verified, so nothing is enqueued.
	movedTask := newTask("rda")
	m3 := put("note/rda/moved", "moved", &movedTask)
	verified := candidate(m3)
	if _, err := conn.Exec(ctx, `UPDATE tasks SET project = 'sma' WHERE id = $1`, movedTask); err != nil {
		t.Fatal(err)
	}
	if got, err := st.PrepareYTArticleBackfill(ctx, m3, time.Now().UTC(), verified); err != nil || got != "candidate_changed" || ops(create, m3) != 0 {
		t.Fatalf("a moved candidate = %q, %v with %d ops; want candidate_changed and no op", got, err, ops(create, m3))
	}

	// A task move that commits while prepare runs: whichever way the lock
	// orders them, the outcome is the same, and no op is written.
	racingTask := newTask("rda")
	m4 := put("note/rda/racing", "racing", &racingTask)
	verified4 := candidate(m4)
	moveTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := moveTx.Exec(ctx, `UPDATE tasks SET project = 'sma' WHERE id = $1`, racingTask); err != nil {
		t.Fatal(err)
	}
	type prepared struct {
		outcome string
		err     error
	}
	done := make(chan prepared, 1)
	go func() {
		got, err := st.PrepareYTArticleBackfill(ctx, m4, time.Now().UTC(), verified4)
		done <- prepared{got, err}
	}()
	if err := moveTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.err != nil || got.outcome != "candidate_changed" || ops(create, m4) != 0 {
		t.Fatalf("a racing move = %q, %v with %d ops; want candidate_changed and no op", got.outcome, got.err, ops(create, m4))
	}

	// Two short documents of one new family on one project, prepared at the
	// same time: exactly one root-capable create is inserted, and the other
	// waits for it (the family lock makes the pending check and the insert one).
	ra := put("race/rda/a", "a", &rdaTask)
	rb := put("race/rda/b", "b", &rdaTask)
	ca, cb := candidate(ra), candidate(rb)
	raced := make(chan prepared, 2)
	for _, c := range []struct {
		id   int64
		want store.YTArticleCandidate
	}{{ra, ca}, {rb, cb}} {
		go func() {
			got, err := st.PrepareYTArticleBackfill(ctx, c.id, time.Now().UTC(), c.want)
			raced <- prepared{got, err}
		}()
	}
	created, waited := 0, 0
	for range 2 {
		got := <-raced
		if got.err != nil {
			t.Fatal(got.err)
		}
		switch got.outcome {
		case "create_prepared":
			created++
		case "family_root_pending":
			waited++
		}
	}
	if created != 1 || waited != 1 || ops(create, ra)+ops(create, rb) != 1 {
		t.Fatalf("two concurrent short documents of one family: %d created, %d waited, %d creates in all; want one of each", created, waited, ops(create, ra)+ops(create, rb))
	}

	// The audit shows the long documents waiting on their root, not as an
	// exception, while the split flag is on; with it off they are not_updated.
	// m6 is a long document from before the import: the audit counts it as the
	// backfill would split it.
	m6 := put("note/rda/old-huge", strings.Repeat("line\n", 50*1024), &rdaTask)
	if _, err := conn.Exec(ctx, `UPDATE documents SET created_at=$1 WHERE id=$2`, store.YTImportEndedAt.Add(-time.Hour), m6); err != nil {
		t.Fatal(err)
	}
	a := &ytAuditor{st: st, tenant: tenant, articles: true, split: true, families: true}
	docs, err := a.documents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(docs.WaitingFamilyRoot.DocumentIDs, []int64{m2, m6}) || docs.NotUpdated.Count != 0 {
		t.Fatalf("audit with the split on = waiting %v, not_updated %d", docs.WaitingFamilyRoot.DocumentIDs, docs.NotUpdated.Count)
	}
	a.split = false
	if docs, err = a.documents(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(docs.NotUpdated.DocumentIDs, []int64{m2, m6}) || docs.WaitingFamilyRoot.Count != 0 {
		t.Fatalf("audit with the split off = not_updated %v, waiting %d", docs.NotUpdated.DocumentIDs, docs.WaitingFamilyRoot.Count)
	}
}

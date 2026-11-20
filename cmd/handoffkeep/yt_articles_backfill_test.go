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
// second op, a family whose root create is uncertain is held, and a long
// document waiting on its family's root shows waiting_family_root.
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

	if _, err := st.AddTaskProject(ctx, "rda", "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "rda", ProjectID: "0-7", ShortName: "RDA"}); err != nil {
		t.Fatal(err)
	}
	rda := "rda"
	task, err := st.CreateTask(ctx, store.Task{Lane: "ybf-lane", Title: "enqueue", Kind: "implement", CreatedBy: "test", Project: &rda})
	if err != nil {
		t.Fatal(err)
	}
	put := func(key, body string, taskID *int64) int64 {
		t.Helper()
		d, _, err := st.PutDocument(ctx, store.Document{Key: key, Kind: "note", Body: body, CreatedBy: "test", TaskID: taskID})
		if err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	// m1 is a short missing document of family note and m2 a long one of the
	// same family, whose split waits for a root (none is recorded). u0 and u1
	// are shared documents of family uncertain.
	m1 := put("note/rda/fresh", "fresh", &task.ID)
	m2 := put("note/rda/huge", strings.Repeat("line\n", 50*1024), &task.ID)
	u0 := put("uncertain/first", "first", nil)
	u1 := put("uncertain/second", "second", nil)

	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", "fake-test-key")
	t.Setenv("HANDOFFKEEP_YT_DOCS_MODE", "on")
	t.Setenv("HANDOFFKEEP_YT_ARTICLE_SPLIT", "on")

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

	// A dry run (the default) plans every missing document, reads YouTrack
	// and writes nothing.
	dry, err := ybfRun(t, "yt", "articles-backfill")
	if err != nil {
		t.Fatal(err)
	}
	if dry.Mode != "dry_run" || dry.Enqueued == nil || *dry.Enqueued != 4 || fake.writes != 0 || total() != 0 {
		t.Fatalf("dry run = mode %q, enqueued %v; %d writes, %d ops", dry.Mode, dry.Enqueued, fake.writes, total())
	}
	if a := ybfAction(t, dry, m1); a.Action != "enqueue_create" || a.Class != ytArticleMissing {
		t.Errorf("m1 dry-run action = %+v", a)
	}
	if a := ybfAction(t, dry, m2); a.Action != "enqueue_split" || a.Reason != store.YTArticleWaitingFamilyRoot {
		t.Errorf("m2 dry-run action = %+v, want enqueue_split waiting on its family root", a)
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
	if ops(create, m1) != 1 || total() != 1 {
		t.Fatalf("after --limit 1: %d create ops for m1, %d ops in all", ops(create, m1), total())
	}

	// --apply prepares the rest: one op per document, the split of m2 included.
	r, err = ybfRun(t, "yt", "articles-backfill", "--apply", "--by", "test:enqueue")
	if err != nil {
		t.Fatal(err)
	}
	if r.Enqueued == nil || *r.Enqueued != 3 {
		t.Fatalf("apply enqueued %v, want 3", r.Enqueued)
	}
	if a := ybfAction(t, r, m1); a.Action != "create_exists" {
		t.Errorf("m1 on the second apply = %+v, want create_exists", a)
	}
	if a := ybfAction(t, r, m2); a.Action != "split_prepared" {
		t.Errorf("m2 action = %+v, want split_prepared", a)
	}
	if ops(create, m1) != 1 || ops(split, m2) != 1 || ops(create, u0) != 1 || ops(create, u1) != 1 || total() != 4 {
		t.Fatalf("after --apply: m1 %d, m2 split %d, u0 %d, u1 %d, %d ops in all", ops(create, m1), ops(split, m2), ops(create, u0), ops(create, u1), total())
	}

	// A rerun prepares nothing: no second op for any document.
	r, err = ybfRun(t, "yt", "articles-backfill", "--apply", "--by", "test:enqueue")
	if err != nil {
		t.Fatal(err)
	}
	if r.Enqueued == nil || *r.Enqueued != 0 || total() != 4 {
		t.Fatalf("rerun enqueued %v, %d ops in all; want none", r.Enqueued, total())
	}
	if a := ybfAction(t, r, m2); a.Action != "create_exists" {
		t.Errorf("m2 on the rerun = %+v, want create_exists", a)
	}

	// A family whose root create is uncertain (remote_unknown, no root
	// recorded) gets no new op: u2 waits.
	u2 := put("uncertain/third", "third", nil)
	if _, err := conn.Exec(ctx, `UPDATE yt_write_ops SET state='remote_unknown' WHERE principal = $1 AND kind = $2 AND entity_id = $3`,
		store.YTDocsPrincipal, create, strconv.FormatInt(u0, 10)); err != nil {
		t.Fatal(err)
	}
	r, err = ybfRun(t, "yt", "articles-backfill", "--apply", "--by", "test:enqueue")
	if err != nil {
		t.Fatal(err)
	}
	if a := ybfAction(t, r, u2); a.Action != "wait" || a.Reason != "family_root_uncertain" {
		t.Errorf("u2 action = %+v, want wait for the uncertain family root", a)
	}
	if ops(create, u2) != 0 || ops(create, u0) != 1 || total() != 4 {
		t.Fatalf("uncertain family: u2 has %d ops, u0 %d, %d in all", ops(create, u2), ops(create, u0), total())
	}

	// The audit shows the long document waiting on its root, not as an
	// exception, while the split flag is on; with it off it is not_updated.
	a := &ytAuditor{st: st, tenant: tenant, articles: true, split: true}
	docs, err := a.documents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(docs.WaitingFamilyRoot.DocumentIDs, []int64{m2}) || docs.NotUpdated.Count != 0 {
		t.Fatalf("audit with the split on = waiting %v, not_updated %d", docs.WaitingFamilyRoot.DocumentIDs, docs.NotUpdated.Count)
	}
	a.split = false
	if docs, err = a.documents(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(docs.NotUpdated.DocumentIDs, []int64{m2}) || docs.WaitingFamilyRoot.Count != 0 {
		t.Fatalf("audit with the split off = not_updated %v, waiting %d", docs.NotUpdated.DocumentIDs, docs.WaitingFamilyRoot.Count)
	}
}

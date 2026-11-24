package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// retentionMySQLStore opens a fresh MySQL store; doc_retention_merge_mysql_test.go
// sets it under the mysql tag so the merge-link tests run on both dialects.
var retentionMySQLStore func(t *testing.T) *Store

// forRetentionDialects runs fn on a PostgreSQL schema store and, under the
// mysql tag, on a MySQL store too.
func forRetentionDialects(t *testing.T, prefix string, fn func(t *testing.T, s *Store)) {
	t.Run("postgres", func(t *testing.T) { fn(t, schemaStore(t, prefix)) })
	if retentionMySQLStore != nil {
		t.Run("mysql", func(t *testing.T) { fn(t, retentionMySQLStore(t)) })
	}
}

// mlWorld builds merge-link fixtures with dialect-neutral statements.
type mlWorld struct {
	t   *testing.T
	s   *Store
	now time.Time
}

func newMLWorld(t *testing.T, s *Store) mlWorld {
	return mlWorld{t: t, s: s, now: time.Now().UTC()}
}

func (w mlWorld) daysAgo(n int) *time.Time {
	v := w.now.Add(-time.Duration(n) * 24 * time.Hour)
	return &v
}

func (w mlWorld) cutoff() time.Time {
	return w.now.Add(-time.Duration(DocRetentionDays) * 24 * time.Hour)
}

func (w mlWorld) exec(pg, my string, args ...any) {
	w.t.Helper()
	if _, err := w.s.db().Exec(w.t.Context(), w.s.q(pg, my), args...); err != nil {
		w.t.Fatal(err)
	}
}

func (w mlWorld) task(ref string) int64 {
	w.t.Helper()
	task, _, err := w.s.IntakeTask(w.t.Context(), IntakeInput{Source: "merge-link-test", SourceRef: ref, Title: "merge link " + ref, Body: "body", CreatedBy: "tester"})
	if err != nil {
		w.t.Fatal(err)
	}
	return task.ID
}

// change files a change on repo/pr with state and merged_at, linked to the
// tasks, with one review row at head (reviewer_job_id NULL, as in production).
func (w mlWorld) change(repo string, pr int, state string, mergedAt *time.Time, head string, tasks ...int64) int64 {
	w.t.Helper()
	ctx := w.t.Context()
	opened := w.now.Add(-400 * 24 * time.Hour)
	w.exec(`INSERT INTO changes(repo, pr_number, state, head_sha, base_sha, opened_at, merged_at, created_at, updated_at) VALUES($1,$2,$3,$4,'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',$5,$6,$7,$8)`,
		`INSERT INTO changes(repo, pr_number, state, head_sha, base_sha, opened_at, merged_at, created_at, updated_at) VALUES(?,?,?,?,'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',?,?,?,?)`,
		repo, pr, state, head, opened, mergedAt, w.now, w.now)
	var id int64
	if err := w.s.db().QueryRow(ctx, w.s.q(`SELECT id FROM changes WHERE repo=$1 AND pr_number=$2`, `SELECT id FROM changes WHERE repo=? AND pr_number=?`), repo, pr).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	w.exec(`INSERT INTO reviews(change_id, reviewed_head, round, pass, lens, recorded_at) VALUES($1,$2,1,1,'correctness',$3)`,
		`INSERT INTO reviews(change_id, reviewed_head, round, pass, lens, recorded_at) VALUES(?,?,1,1,'correctness',?)`, id, head, w.now)
	for _, task := range tasks {
		w.exec(`INSERT INTO change_tasks(change_id, task_id, relation, created_at) VALUES($1,$2,'implements',$3)`,
			`INSERT INTO change_tasks(change_id, task_id, relation, created_at) VALUES(?,?,'implements',?)`, id, task, w.now)
	}
	return id
}

func (w mlWorld) doc(key, job string, task *int64) Document {
	w.t.Helper()
	kind := "brief"
	switch {
	case strings.HasPrefix(key, "screens/"):
		kind = "evidence"
	case strings.HasPrefix(key, "reports/"):
		kind = "report"
	}
	d, _, err := w.s.PutDocument(w.t.Context(), Document{Key: key, Kind: kind, Body: "body of " + key, CreatedBy: "tester", Job: job, TaskID: task})
	if err != nil {
		w.t.Fatal(err)
	}
	return d
}

// jobDocs files the four documents a review job writes, every one on task.
func (w mlWorld) jobDocs(job string, task *int64) map[string]Document {
	return map[string]Document{
		DocRetentionMwrPrefix:        w.doc("review/"+job, job, task),
		DocRetentionBriefsMwrPrefix:  w.doc("briefs/"+job, job, task),
		DocRetentionScreensMwrPrefix: w.doc("screens/"+job+"/pane.txt", job, task),
		DocRetentionKeptPrefix:       w.doc("reports/"+job, job, task),
	}
}

func (w mlWorld) bind(doc Document) {
	w.exec(`INSERT INTO yt_article_bindings(document_id,tenant,article_id,project_id,origin,bound_by,bound_at) VALUES($1,'tenant',$2,'PROJ','import_verified','test',$3)`,
		`INSERT INTO yt_article_bindings(document_id,tenant,article_id,project_id,origin,bound_by,bound_at) VALUES(?,'tenant',?,'PROJ','import_verified','test',?)`,
		doc.ID, fmt.Sprintf("article-%d", doc.ID), w.now)
}

func (w mlWorld) exists(key string) bool {
	w.t.Helper()
	_, found, err := w.s.GetDocument(w.t.Context(), key)
	if err != nil {
		w.t.Fatal(err)
	}
	return found
}

func (w mlWorld) run(apply bool) DocRetentionResult {
	w.t.Helper()
	res, err := w.s.PruneRetention(w.t.Context(), w.cutoff(), 100, apply, "merge-link-test")
	if err != nil {
		w.t.Fatal(err)
	}
	return res
}

// assertFamilies checks the dry run's per-family classes (bytes aside) and
// that every family adds up.
func (w mlWorld) assertFamilies(res DocRetentionResult, want map[string]DocRetentionFamily) {
	w.t.Helper()
	if len(res.MergeLink) != len(docRetentionMergeLinkFamilies) {
		w.t.Fatalf("merge_link families = %v, want exactly %v", res.MergeLink, docRetentionMergeLinkFamilies)
	}
	for _, fam := range docRetentionMergeLinkFamilies {
		got := res.MergeLink[fam]
		if sum := got.EligibleRows + got.Bound + got.Fresh + got.Unmerged + got.Unlinked + got.Ambiguous; sum != got.Total {
			w.t.Fatalf("%s: classes add to %d, total %d (%+v)", fam, sum, got.Total, got)
		}
		if (got.EligibleRows == 0) != (got.EligibleBytes == 0) {
			w.t.Fatalf("%s: eligible rows %d with bytes %d", fam, got.EligibleRows, got.EligibleBytes)
		}
		got.EligibleBytes = 0
		exp := want[fam]
		if got != exp {
			w.t.Fatalf("%s: merge_link = %+v, want %+v", fam, got, exp)
		}
	}
}

func (w mlWorld) assertKeys(deleted, kept []string) {
	w.t.Helper()
	for _, k := range deleted {
		if w.exists(k) {
			w.t.Fatalf("%s kept, want deleted", k)
		}
		var n int64
		if err := w.s.db().QueryRow(w.t.Context(), w.s.q(`SELECT count(*) FROM doc_prune_log WHERE document_key=$1`, `SELECT COUNT(*) FROM doc_prune_log WHERE document_key=?`), k).Scan(&n); err != nil {
			w.t.Fatal(err)
		}
		if n != 1 {
			w.t.Fatalf("%s: %d doc_prune_log rows, want 1", k, n)
		}
	}
	for _, k := range kept {
		if !w.exists(k) {
			w.t.Fatalf("%s deleted, want kept", k)
		}
	}
}

func sha40(c byte) string { return strings.Repeat(string(c), 40) }

// TestRetentionMergeLinkKeepsOpenPRDocsOnTaskWithOlderMergedPR: one task
// implements PR A (merged 40 days ago, reviewed at head a...) and PR B (open,
// reviewed at head b...). Every document of B's review job is kept; review,
// briefs and screens of A's job are deleted; reports of A is kept.
func TestRetentionMergeLinkKeepsOpenPRDocsOnTaskWithOlderMergedPR(t *testing.T) {
	forRetentionDialects(t, "doc_retention_ml_open", func(t *testing.T, s *Store) {
		w := newMLWorld(t, s)
		s.setRetentionMergeLinkApply(true)
		task := w.task("open-and-merged")
		w.change("example/app", 701, "merged", w.daysAgo(40), sha40('a'), task)
		w.change("example/app", 702, "open", nil, sha40('b'), task)
		jobA := "mwr-701-aaaaaaa-p1-1700000001"
		jobB := "mwr-702-bbbbbbb-p1-1700000002"
		a := w.jobDocs(jobA, &task)
		b := w.jobDocs(jobB, &task)

		dry := w.run(false)
		if !dry.MergeLinkApply || dry.Deleted != 0 {
			t.Fatalf("dry run = %+v, want merge_link_apply and nothing deleted", dry)
		}
		one := DocRetentionFamily{EligibleRows: 1, Unmerged: 1, Total: 2}
		w.assertFamilies(dry, map[string]DocRetentionFamily{DocRetentionMwrPrefix: one, DocRetentionBriefsMwrPrefix: one, DocRetentionScreensMwrPrefix: one})

		applied := w.run(true)
		if applied.Deleted != 3 || applied.MergeLinkDeleted != 3 || applied.ReviewDeleted != 0 || applied.ScreensDeleted != 0 {
			t.Fatalf("apply = %+v, want 3 deleted by the merge link alone", applied)
		}
		var kept []string
		for _, d := range b {
			kept = append(kept, d.Key)
		}
		kept = append(kept, a[DocRetentionKeptPrefix].Key)
		w.assertKeys([]string{a[DocRetentionMwrPrefix].Key, a[DocRetentionBriefsMwrPrefix].Key, a[DocRetentionScreensMwrPrefix].Key}, kept)
		if again := w.run(true); again.Deleted != 0 {
			t.Fatalf("second apply = %+v, want nothing", again)
		}
	})
}

// TestRetentionMergeLinkKeepsAmbiguousAndUnlinked: two task ids on one job,
// two changes on the task with the job's PR number and head, a sha7 no review
// head starts with, a job id that does not parse, and a job with no task are
// all kept and counted in their class.
func TestRetentionMergeLinkKeepsAmbiguousAndUnlinked(t *testing.T) {
	forRetentionDialects(t, "doc_retention_ml_amb", func(t *testing.T, s *Store) {
		w := newMLWorld(t, s)
		s.setRetentionMergeLinkApply(true)
		old := w.daysAgo(40)
		t1, t2, t3, t4, t5 := w.task("t1"), w.task("t2"), w.task("t3"), w.task("t4"), w.task("t5")

		// Two task ids across the job's documents: ambiguous.
		w.change("example/app", 711, "merged", old, sha40('1'), t1)
		twoTasks := "mwr-711-1111111-p1-1"
		w.doc("review/"+twoTasks, twoTasks, &t1)
		w.doc("briefs/"+twoTasks, twoTasks, &t2)
		// Two changes on the task with PR 712 and a head starting 2222222: ambiguous.
		w.change("example/one", 712, "merged", old, sha40('2'), t3)
		w.change("example/two", 712, "merged", old, sha40('2'), t3)
		twoChanges := "mwr-712-2222222-p1-2"
		w.doc("review/"+twoChanges, twoChanges, &t3)
		// No review head starts with the sha7: unlinked.
		w.change("example/app", 713, "merged", old, sha40('3'), t4)
		noHead := "mwr-713-4444444-p1-3"
		w.doc("review/"+noHead, noHead, &t4)
		// A job id that does not parse: unlinked.
		w.doc("review/mwr-713-zz-p1-4", "mwr-713-zz-p1-4", &t4)
		w.doc("screens/mwr-713-3333333-p1-5/pane.txt", "", nil)
		// A parsing job whose documents carry no task: unlinked.
		w.change("example/app", 715, "merged", old, sha40('5'), t5)
		noTask := "mwr-715-5555555-p1-6"
		w.doc("review/"+noTask, noTask, nil)
		w.doc("reports/"+noTask, noTask, nil)

		dry := w.run(false)
		w.assertFamilies(dry, map[string]DocRetentionFamily{
			DocRetentionMwrPrefix:        {Ambiguous: 2, Unlinked: 3, Total: 5},
			DocRetentionBriefsMwrPrefix:  {Ambiguous: 1, Total: 1},
			DocRetentionScreensMwrPrefix: {Unlinked: 1, Total: 1},
		})
		if applied := w.run(true); applied.Deleted != 0 {
			t.Fatalf("apply = %+v, want nothing deleted", applied)
		}
	})
}

// TestRetentionMergeLinkAgeAndState: merged 29 days ago is fresh and kept,
// merged 31 days ago is deleted, closed unmerged is kept as unmerged and a
// bound briefs/mwr- is kept as bound.
func TestRetentionMergeLinkAgeAndState(t *testing.T) {
	forRetentionDialects(t, "doc_retention_ml_age", func(t *testing.T, s *Store) {
		w := newMLWorld(t, s)
		s.setRetentionMergeLinkApply(true)
		task := w.task("age")
		w.change("example/app", 721, "merged", w.daysAgo(29), sha40('6'), task)
		w.change("example/app", 722, "merged", w.daysAgo(31), sha40('7'), task)
		w.change("example/app", 723, "closed", nil, sha40('8'), task)
		w.change("example/app", 724, "merged", w.daysAgo(40), sha40('9'), task)
		fresh := w.doc("review/mwr-721-6666666-p1-1", "mwr-721-6666666-p1-1", &task)
		old := w.doc("review/mwr-722-7777777-p1-2", "mwr-722-7777777-p1-2", &task)
		closed := w.doc("review/mwr-723-8888888-p1-3", "mwr-723-8888888-p1-3", &task)
		bound := w.doc("briefs/mwr-724-9999999-p1-4", "mwr-724-9999999-p1-4", &task)
		w.bind(bound)

		dry := w.run(false)
		w.assertFamilies(dry, map[string]DocRetentionFamily{
			DocRetentionMwrPrefix:       {EligibleRows: 1, Fresh: 1, Unmerged: 1, Total: 3},
			DocRetentionBriefsMwrPrefix: {Bound: 1, Total: 1},
		})
		if got := dry.MergeLink[DocRetentionMwrPrefix].EligibleBytes; got != int64(len("body of "+old.Key)) {
			t.Fatalf("eligible bytes = %d, want %d", got, len("body of "+old.Key))
		}
		applied := w.run(true)
		if applied.Deleted != 1 || applied.MergeLinkDeleted != 1 {
			t.Fatalf("apply = %+v, want 1 deleted", applied)
		}
		w.assertKeys([]string{old.Key}, []string{fresh.Key, closed.Key, bound.Key})
	})
}

// TestRetentionMergeLinkOffDeletesNothingNew: with the switch off an apply
// run deletes no merge-link-only document (the hk 5057 legs still delete),
// and a dry run reports the document as eligible.
func TestRetentionMergeLinkOffDeletesNothingNew(t *testing.T) {
	forRetentionDialects(t, "doc_retention_ml_off", func(t *testing.T, s *Store) {
		w := newMLWorld(t, s)
		s.setRetentionMergeLinkApply(false)
		task := w.task("off")
		w.change("example/app", 731, "merged", w.daysAgo(40), sha40('c'), task)
		job := "mwr-731-ccccccc-p1-1"
		docs := w.jobDocs(job, &task)
		// An hk 5057 screens/* document by ended job: still deleted.
		w.exec(`INSERT INTO jobs(job_id, task_id, host, state, created_by, created_at, updated_at, ended_at) VALUES($1,0,'test-host','completed','tester',$2,$3,$4)`,
			`INSERT INTO jobs(job_id, task_id, host, state, created_by, created_at, updated_at, ended_at) VALUES(?,0,'test-host','completed','tester',?,?,?)`,
			"job-ml-off", w.now, w.now, *w.daysAgo(40))
		endedScreen := w.doc("screens/job-ml-off/pane.txt", "job-ml-off", nil)

		dry := w.run(false)
		if dry.MergeLinkApply {
			t.Fatalf("dry run = %+v, want merge_link_apply false", dry)
		}
		one := DocRetentionFamily{EligibleRows: 1, Total: 1}
		w.assertFamilies(dry, map[string]DocRetentionFamily{DocRetentionMwrPrefix: one, DocRetentionBriefsMwrPrefix: one, DocRetentionScreensMwrPrefix: one})
		if dry.Matched != 1 || dry.Bytes != int64(len("body of "+endedScreen.Key)) {
			t.Fatalf("dry run hk 5057 fields = %+v, want only the ended screen", dry)
		}

		applied := w.run(true)
		if applied.Deleted != 1 || applied.ScreensDeleted != 1 || applied.MergeLinkDeleted != 0 || applied.MergeLink != nil {
			t.Fatalf("apply = %+v, want only the ended screen deleted", applied)
		}
		var kept []string
		for _, d := range docs {
			kept = append(kept, d.Key)
		}
		w.assertKeys([]string{endedScreen.Key}, kept)
	})
}

// TestRetentionKeepsReportsMwr: reports/mwr- is never a merge-link family
// and is never deleted, even when its job is linked and long merged.
func TestRetentionKeepsReportsMwr(t *testing.T) {
	for _, fam := range docRetentionMergeLinkFamilies {
		if strings.HasPrefix(fam, "reports/") || fam == DocRetentionKeptPrefix {
			t.Fatalf("merge-link families %v include %s", docRetentionMergeLinkFamilies, fam)
		}
	}
	forRetentionDialects(t, "doc_retention_ml_reports", func(t *testing.T, s *Store) {
		w := newMLWorld(t, s)
		s.setRetentionMergeLinkApply(true)
		task := w.task("reports")
		w.change("example/app", 741, "merged", w.daysAgo(300), sha40('d'), task)
		job := "mwr-741-ddddddd-p1-1"
		review := w.doc("review/"+job, job, &task)
		report := w.doc("reports/"+job, job, &task)
		other := w.doc("reports/mwr-741-ddddddd-p2-2", job, &task)

		dry := w.run(false)
		if _, ok := dry.MergeLink[DocRetentionKeptPrefix]; ok {
			t.Fatalf("dry run classifies reports/mwr-: %+v", dry.MergeLink)
		}
		w.assertFamilies(dry, map[string]DocRetentionFamily{DocRetentionMwrPrefix: {EligibleRows: 1, Total: 1}})
		applied := w.run(true)
		if applied.Deleted != 1 {
			t.Fatalf("apply = %+v, want only the review prompt deleted", applied)
		}
		w.assertKeys([]string{review.Key}, []string{report.Key, other.Key})
	})
}

// TestRetentionMergeLinkCompactJobIDs: the compact job id current mergewake
// writes (mwr-<pr>-<sha7>p<n><suffix>, no hyphen after the sha7) links like
// the hyphenated one, and the head prefix is exactly the first 7 characters.
// An 8-hex segment or a non-p suffix does not parse.
func TestRetentionMergeLinkCompactJobIDs(t *testing.T) {
	forRetentionDialects(t, "doc_retention_ml_compact", func(t *testing.T, s *Store) {
		w := newMLWorld(t, s)
		s.setRetentionMergeLinkApply(true)
		task := w.task("compact")
		w.change("example/app", 751, "merged", w.daysAgo(40), "0d338a8"+strings.Repeat("e", 33), task)
		w.change("example/app", 752, "open", nil, "1818a0e"+strings.Repeat("f", 33), task)
		merged := w.jobDocs("mwr-751-0d338a8p1t3lmlwo", &task)
		open := w.jobDocs("mwr-752-1818a0ep2t3lfq4f", &task)
		eightHex := w.doc("review/mwr-751-0d338a8ep1t3lmlwo", "mwr-751-0d338a8ep1t3lmlwo", &task)
		notP := w.doc("review/mwr-751-0d338a8x1t3lmlwo", "mwr-751-0d338a8x1t3lmlwo", &task)

		dry := w.run(false)
		one := DocRetentionFamily{EligibleRows: 1, Unmerged: 1, Total: 2}
		w.assertFamilies(dry, map[string]DocRetentionFamily{
			DocRetentionMwrPrefix:        {EligibleRows: 1, Unmerged: 1, Unlinked: 2, Total: 4},
			DocRetentionBriefsMwrPrefix:  one,
			DocRetentionScreensMwrPrefix: one,
		})
		if applied := w.run(true); applied.MergeLinkDeleted != 3 {
			t.Fatalf("apply = %+v, want the merged compact job's 3 documents", applied)
		}
		kept := []string{merged[DocRetentionKeptPrefix].Key, eightHex.Key, notP.Key}
		for _, d := range open {
			kept = append(kept, d.Key)
		}
		w.assertKeys([]string{merged[DocRetentionMwrPrefix].Key, merged[DocRetentionBriefsMwrPrefix].Key, merged[DocRetentionScreensMwrPrefix].Key}, kept)
	})
}

// TestRetentionMergeLinkRechecksUnderLock: between the merge leg's pick and
// its batch transaction one picked document is bound to a YouTrack article,
// another is upserted onto an open PR's job, and a third job gains a
// document on a second task. The recheck under the row lock sees all three
// and deletes only what is still eligible.
func TestRetentionMergeLinkRechecksUnderLock(t *testing.T) {
	forRetentionDialects(t, "doc_retention_ml_recheck", func(t *testing.T, s *Store) {
		w := newMLWorld(t, s)
		s.setRetentionMergeLinkApply(true)
		task, other := w.task("recheck"), w.task("recheck-other")
		w.change("example/app", 761, "merged", w.daysAgo(40), sha40('a'), task)
		w.change("example/app", 762, "merged", w.daysAgo(40), sha40('b'), task)
		w.change("example/app", 763, "merged", w.daysAgo(40), sha40('c'), task)
		w.change("example/app", 764, "open", nil, sha40('d'), task)
		j1, j2, j3 := "mwr-761-aaaaaaap1x", "mwr-762-bbbbbbbp1x", "mwr-763-cccccccp1x"
		d1 := w.jobDocs(j1, &task)
		d2 := w.jobDocs(j2, &task)
		d3 := w.jobDocs(j3, &task)
		fired := false
		s.retentionMergeSeam = func() {
			if fired {
				return
			}
			fired = true
			w.bind(d1[DocRetentionMwrPrefix])
			moved := d2[DocRetentionBriefsMwrPrefix]
			if _, _, err := s.PutDocument(t.Context(), Document{Key: moved.Key, Kind: moved.Kind, Body: "moved to the open PR's job", CreatedBy: "tester", Job: "mwr-764-dddddddp1x", TaskID: &task}); err != nil {
				t.Fatal(err)
			}
			w.doc("briefs/"+j3+"-extra", j3, &other)
		}
		applied := w.run(true)
		if !fired {
			t.Fatal("merge seam never ran")
		}
		deleted := []string{d1[DocRetentionBriefsMwrPrefix].Key, d1[DocRetentionScreensMwrPrefix].Key, d2[DocRetentionMwrPrefix].Key, d2[DocRetentionScreensMwrPrefix].Key}
		if applied.MergeLinkDeleted != int64(len(deleted)) {
			t.Fatalf("apply = %+v, want %d merge-link deletions", applied, len(deleted))
		}
		kept := []string{d1[DocRetentionMwrPrefix].Key, d1[DocRetentionKeptPrefix].Key, d2[DocRetentionBriefsMwrPrefix].Key, d2[DocRetentionKeptPrefix].Key, "briefs/" + j3 + "-extra"}
		for _, d := range d3 {
			kept = append(kept, d.Key)
		}
		w.assertKeys(deleted, kept)
	})
}

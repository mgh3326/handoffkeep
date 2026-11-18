package store

import (
	"errors"
	"testing"
	"time"
)

func retentionChange(t *testing.T, s *Store, repo string, pr int, state string, mergedAt *time.Time) int64 {
	t.Helper()
	ctx := t.Context()
	var id int64
	opened := time.Now().UTC()
	if mergedAt != nil && mergedAt.After(opened) {
		opened = mergedAt.Add(-time.Hour)
	}
	if err := s.pool.QueryRow(ctx, `INSERT INTO changes(repo, pr_number, state, head_sha, base_sha, merge_sha, opened_at, merged_at, created_at, updated_at)
VALUES($1,$2,$3,'head','base',NULL,$4,$5,now(),now()) RETURNING id`, repo, pr, state, opened, mergedAt).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func retentionReview(t *testing.T, s *Store, changeID int64, head, job string) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO reviews(change_id, reviewed_head, round, pass, lens, reviewer_job_id, recorded_at)
VALUES($1,$2,1,1,'correctness',$3,now())`, changeID, head, job); err != nil {
		t.Fatal(err)
	}
}

func retentionJob(t *testing.T, s *Store, jobID, state string, endedAt *time.Time) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO jobs(job_id, task_id, host, state, created_by, created_at, updated_at, ended_at)
VALUES($1,0,'test-host',$2,'tester',now(),now(),$3)`, jobID, state, endedAt); err != nil {
		t.Fatal(err)
	}
}

func retentionDoc(t *testing.T, s *Store, key, kind, job string, taskID *int64) Document {
	t.Helper()
	d, _, err := s.PutDocument(t.Context(), Document{Key: key, Kind: kind, Body: "body of " + key, CreatedBy: "tester", Job: job, TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestPruneRetentionDeletesOnlyMergedEndedOld covers the retention contract:
// a review/mwr-* document is deleted only when its change merged at least 30
// days ago, a screens/* document only when its job ended that long ago, a
// bound document is skipped and counted, other prefixes are never touched,
// deletions are audited, and a second run deletes nothing.
func TestPruneRetentionDeletesOnlyMergedEndedOld(t *testing.T) {
	s := schemaStore(t, "doc_retention")
	ctx := t.Context()
	const day = 24 * time.Hour
	now := time.Now().UTC()
	old := now.Add(-40 * day)
	fresh := now.Add(-5 * day)
	cutoff := now.Add(-30 * day)

	mergedOld := retentionChange(t, s, "example/app", 101, "merged", &old)
	mergedFresh := retentionChange(t, s, "example/app", 102, "merged", &fresh)
	openOld := retentionChange(t, s, "example/app", 103, "open", nil)
	retentionReview(t, s, mergedOld, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "mwr-101-aaa-p1-1")
	retentionReview(t, s, mergedFresh, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "mwr-102-bbb-p1-2")
	retentionReview(t, s, openOld, "cccccccccccccccccccccccccccccccccccccccc", "mwr-103-ccc-p1-3")
	retentionJob(t, s, "job-old", "completed", &old)
	retentionJob(t, s, "job-fresh", "completed", &fresh)
	retentionJob(t, s, "job-running", "spawned", nil)

	reviewOld := retentionDoc(t, s, "review/mwr-101-aaa-p1-1", "brief", "mwr-101-aaa-p1-1", nil)
	retentionDoc(t, s, "review/mwr-102-bbb-p1-2", "brief", "mwr-102-bbb-p1-2", nil)
	retentionDoc(t, s, "review/mwr-103-ccc-p1-3", "brief", "mwr-103-ccc-p1-3", nil)
	retentionDoc(t, s, "review/mwr-999-zzz-p1-9", "brief", "mwr-999-zzz-p1-9", nil)
	bound := retentionDoc(t, s, "review/mwr-101-second-p1-5", "brief", "mwr-101-aaa-p1-1", nil)
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_article_bindings(document_id,tenant,article_id,project_id,origin,bound_by,bound_at) VALUES($1,'tenant','article-1','PROJ','import_verified','test',now())`, bound.ID); err != nil {
		t.Fatal(err)
	}
	task := flowTask(t, s, s.pool, "implement", "backlog", false)
	if _, err := s.pool.Exec(ctx, `INSERT INTO change_tasks(change_id, task_id, relation, created_at) VALUES($1,$2,'implements',now())`, mergedOld, task.ID); err != nil {
		t.Fatal(err)
	}
	reviewViaTask := retentionDoc(t, s, "review/mwr-101-viatask-p1-6", "brief", "", &task.ID)
	screensOld := retentionDoc(t, s, "screens/job-old/pane.txt", "evidence", "job-old", nil)
	retentionDoc(t, s, "screens/job-fresh/pane.txt", "evidence", "job-fresh", nil)
	retentionDoc(t, s, "screens/job-running/pane.txt", "evidence", "job-running", nil)
	screensFallback := retentionDoc(t, s, "screens/job-old/other.txt", "evidence", "", nil)
	retentionDoc(t, s, "briefs/2026-01-01/keep.md", "brief", "", nil)

	dry, err := s.PruneRetention(ctx, cutoff, 100, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if dry.Applied || dry.Deleted != 0 {
		t.Fatalf("dry run = %+v, want no deletes", dry)
	}
	// Eligible including the bound row: reviewOld, bound, reviewViaTask,
	// screensOld, screensFallback.
	if dry.Matched != 5 || dry.SkippedBound != 1 || dry.Bytes == 0 {
		t.Fatalf("dry run counts = %+v, want matched 5 skipped_bound 1 bytes > 0", dry)
	}
	// Prefix totals: 6 review + 4 screens = 10, minus 5 eligible = 5 kept.
	if dry.KeptActive != 5 {
		t.Fatalf("dry run kept_active = %d, want 5", dry.KeptActive)
	}
	for _, d := range []Document{reviewOld, reviewViaTask, screensOld, screensFallback, bound} {
		if !docExists(t, s, d.Key) {
			t.Fatalf("dry run deleted %s", d.Key)
		}
	}

	applied, err := s.PruneRetention(ctx, cutoff, 100, true, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || applied.Deleted != 4 || applied.SkippedBound != 1 {
		t.Fatalf("apply = %+v, want deleted 4 skipped_bound 1", applied)
	}
	if applied.ReviewDeleted != 2 || applied.ScreensDeleted != 2 {
		t.Fatalf("apply per-prefix = %+v, want review 2 screens 2", applied)
	}
	for _, key := range []string{reviewOld.Key, reviewViaTask.Key, screensOld.Key, screensFallback.Key} {
		if docExists(t, s, key) {
			t.Fatalf("apply left eligible %s", key)
		}
	}
	for _, key := range []string{"review/mwr-102-bbb-p1-2", "review/mwr-103-ccc-p1-3", "review/mwr-999-zzz-p1-9", bound.Key, "screens/job-fresh/pane.txt", "screens/job-running/pane.txt", "briefs/2026-01-01/keep.md"} {
		if !docExists(t, s, key) {
			t.Fatalf("apply deleted kept %s", key)
		}
	}
	var logs int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_prune_log WHERE pruned_by='tester'`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if logs != 4 {
		t.Fatalf("audit log rows = %d, want 4", logs)
	}
	var loggedSHA string
	if err := s.pool.QueryRow(ctx, `SELECT sha256 FROM doc_prune_log WHERE document_key=$1`, reviewOld.Key).Scan(&loggedSHA); err != nil {
		t.Fatal(err)
	}
	if loggedSHA != reviewOld.SHA256 {
		t.Fatalf("audit sha = %s, want %s", loggedSHA, reviewOld.SHA256)
	}

	again, err := s.PruneRetention(ctx, cutoff, 100, true, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if again.Deleted != 0 || again.Matched != 1 || again.SkippedBound != 1 {
		t.Fatalf("second run = %+v, want deleted 0 matched 1 skipped_bound 1 (the bound row)", again)
	}
}

// TestPruneRetentionRefusesBadInput keeps the retention pruner bounded and
// identified: no zero or future cutoff, no out-of-range limit, no anonymous
// run.
func TestPruneRetentionRefusesBadInput(t *testing.T) {
	s := schemaStore(t, "doc_retention_refuse")
	ctx := t.Context()
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	for _, tc := range []struct {
		name       string
		cutoff     time.Time
		limit      int
		prunedBy   string
	}{
		{"zero cutoff", time.Time{}, 10, "test"},
		{"future cutoff", time.Now().UTC().Add(time.Hour), 10, "test"},
		{"zero limit", cutoff, 0, "test"},
		{"over limit", cutoff, DocRetentionMaxLimit + 1, "test"},
		{"no who", cutoff, 10, ""},
	} {
		if _, err := s.PruneRetention(ctx, tc.cutoff, tc.limit, true, tc.prunedBy); !errors.Is(err, ErrDocPruneInvalid) {
			t.Fatalf("%s: err = %v, want ErrDocPruneInvalid", tc.name, err)
		}
	}
}

// TestPruneRetentionRespectsLimit deletes at most limit documents per run.
func TestPruneRetentionRespectsLimit(t *testing.T) {
	s := schemaStore(t, "doc_retention_limit")
	ctx := t.Context()
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	for i, job := range []string{"job-l1", "job-l2", "job-l3"} {
		retentionJob(t, s, job, "completed", &old)
		retentionDoc(t, s, "screens/"+job+"/pane.txt", "evidence", job, nil)
		_ = i
	}
	res, err := s.PruneRetention(ctx, cutoff, 2, true, "test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 || res.Matched != 3 {
		t.Fatalf("limit 2 apply = %+v, want deleted 2 of matched 3", res)
	}
}

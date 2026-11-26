package store

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// endJob files a jobs row for job, ended days ago, so the hk 5057 screens/*
// job-end leg deletes its screens.
func (w mlWorld) endJob(job string, days int) {
	w.t.Helper()
	w.exec(`INSERT INTO jobs(job_id, task_id, host, state, created_by, created_at, updated_at, ended_at) VALUES($1,0,'test-host','completed','tester',$2,$3,$4)`,
		`INSERT INTO jobs(job_id, task_id, host, state, created_by, created_at, updated_at, ended_at) VALUES(?,0,'test-host','completed','tester',?,?,?)`,
		job, w.now, w.now, *w.daysAgo(days))
}

func (w mlWorld) prune(limit int) DocRetentionResult {
	w.t.Helper()
	res, err := w.s.PruneRetention(w.t.Context(), w.cutoff(), limit, true, "merge-link-test")
	if err != nil {
		w.t.Fatal(err)
	}
	return res
}

// logged returns the job and task_id of key's one doc_prune_log row.
func (w mlWorld) logged(key string) (*string, *int64) {
	w.t.Helper()
	var job *string
	var task *int64
	if err := w.s.db().QueryRow(w.t.Context(), w.s.q(`SELECT job, task_id FROM doc_prune_log WHERE document_key=$1`, `SELECT job, task_id FROM doc_prune_log WHERE document_key=?`), key).Scan(&job, &task); err != nil {
		w.t.Fatalf("doc_prune_log row for %s: %v", key, err)
	}
	return job, task
}

// TestRetentionMergeLinkWitnessSurvivesLegacyDelete: the hk 5057 screens/*
// job-end leg deletes a job's screens/mwr- document before the merge link
// runs (hk 6289).
//   - Job A: the screen is the only document carrying a second task. After
//     it is gone, A stays ambiguous through its doc_prune_log row, and its
//     review and briefs are kept.
//   - Job B: the screen is the only document carrying a task. After it is
//     gone, B stays linked through its log row, and its review and briefs
//     are deleted.
func TestRetentionMergeLinkWitnessSurvivesLegacyDelete(t *testing.T) {
	forRetentionDialects(t, "doc_retention_ml_witness", func(t *testing.T, s *Store) {
		w := newMLWorld(t, s)
		t1, t2, t3 := w.task("witness-1"), w.task("witness-2"), w.task("witness-3")
		w.change("example/app", 771, "merged", w.daysAgo(40), sha40('a'), t1)
		w.change("example/app", 772, "merged", w.daysAgo(40), sha40('b'), t3)
		jobA, jobB := "mwr-771-aaaaaaa-p1-1", "mwr-772-bbbbbbbp1x"
		reviewA := w.doc("review/"+jobA, jobA, &t1)
		briefsA := w.doc("briefs/"+jobA, jobA, &t1)
		screenA := w.doc("screens/"+jobA+"/pane.txt", jobA, &t2)
		reviewB := w.doc("review/"+jobB, jobB, nil)
		briefsB := w.doc("briefs/"+jobB, jobB, nil)
		screenB := w.doc("screens/"+jobB+"/pane.txt", jobB, &t3)
		w.endJob(jobA, 40)
		w.endJob(jobB, 40)

		// The legacy legs alone delete both screens first.
		s.setRetentionMergeLinkApply(false)
		legacy := w.run(true)
		if legacy.ScreensDeleted != 2 || legacy.MergeLinkDeleted != 0 {
			t.Fatalf("legacy apply = %+v, want the two screens only", legacy)
		}
		w.assertKeys([]string{screenA.Key, screenB.Key}, []string{reviewA.Key, briefsA.Key, reviewB.Key, briefsB.Key})

		s.setRetentionMergeLinkApply(true)
		dry := w.run(false)
		one := DocRetentionFamily{EligibleRows: 1, Ambiguous: 1, Total: 2}
		w.assertFamilies(dry, map[string]DocRetentionFamily{DocRetentionMwrPrefix: one, DocRetentionBriefsMwrPrefix: one})
		applied := w.run(true)
		if applied.MergeLinkDeleted != 2 || applied.Deleted != 2 {
			t.Fatalf("apply = %+v, want job B's review and briefs", applied)
		}
		w.assertKeys([]string{reviewB.Key, briefsB.Key}, []string{reviewA.Key, briefsA.Key})
	})
}

// TestRetentionMergeLinkApplyHonoursLimitAndBatch: with the switch on, the
// merge leg deletes whole jobs only while they fit (hk 6290). A limit below
// one job's eligible count deletes nothing and reports the skip; a limit
// that fits one job but not two deletes exactly one whole job. The pick
// reaches one job per batch of this size, so MergeLinkPickSkippedJobs counts
// only the job the pick reached while the other eligible job remains
// untouched and uncounted.
func TestRetentionMergeLinkApplyHonoursLimitAndBatch(t *testing.T) {
	forRetentionDialects(t, "doc_retention_ml_limit", func(t *testing.T, s *Store) {
		w := newMLWorld(t, s)
		s.setRetentionMergeLinkApply(true)
		task := w.task("limit")
		w.change("example/app", 791, "merged", w.daysAgo(40), sha40('1'), task)
		w.change("example/app", 792, "merged", w.daysAgo(40), sha40('2'), task)
		jobs := map[string][]string{}
		for _, job := range []string{"mwr-791-1111111-p1-1", "mwr-792-2222222-p1-2"} {
			for fam, d := range w.jobDocs(job, &task) {
				if fam != DocRetentionKeptPrefix {
					jobs[job] = append(jobs[job], d.Key)
				}
			}
		}
		// noSplit fails when a job lost some but not all of its documents,
		// and returns how many jobs are gone.
		noSplit := func() int {
			t.Helper()
			gone := 0
			for job, keys := range jobs {
				n := 0
				for _, k := range keys {
					if !w.exists(k) {
						n++
					}
				}
				if n != 0 && n != len(keys) {
					t.Fatalf("job %s split: %d of %d documents deleted", job, n, len(keys))
				}
				if n == len(keys) {
					gone++
				}
			}
			return gone
		}

		for _, limit := range []int{1, 2} {
			res := w.prune(limit)
			if res.Deleted != 0 || res.MergeLinkDeleted != 0 || res.MergeLinkPickSkippedJobs != 1 {
				t.Fatalf("limit %d: apply = %+v, want nothing deleted and one job skipped", limit, res)
			}
			if gone := noSplit(); gone != 0 {
				t.Fatalf("limit %d: %d jobs deleted, want none", limit, gone)
			}
		}

		res := w.prune(4)
		if res.Deleted != 3 || res.MergeLinkDeleted != 3 || res.Deleted > int64(res.Limit) || res.MergeLinkPickSkippedJobs != 1 {
			t.Fatalf("limit 4: apply = %+v, want one whole job (3) and the other skipped", res)
		}
		if gone := noSplit(); gone != 1 {
			t.Fatalf("limit 4: %d jobs deleted, want exactly one", gone)
		}

		res = w.prune(100)
		if res.Deleted != 3 || res.MergeLinkPickSkippedJobs != 0 {
			t.Fatalf("limit 100: apply = %+v, want the remaining job and no skip", res)
		}
		if gone := noSplit(); gone != 2 {
			t.Fatalf("limit 100: %d jobs deleted, want both", gone)
		}
	})
}

// TestDocPruneLogRecordsJobAndTask: every pruner writes the deleted
// document's job and task_id on its doc_prune_log row: hk 3639
// PruneDocuments, the hk 5057 review/mwr-* and screens/* legs, and the merge
// leg. A document without a task logs a NULL task_id.
func TestDocPruneLogRecordsJobAndTask(t *testing.T) {
	forRetentionDialects(t, "doc_prune_log_evidence", func(t *testing.T, s *Store) {
		w := newMLWorld(t, s)
		ctx := t.Context()
		tPrune, tReview, tScreen, tMerge := w.task("log-prune"), w.task("log-review"), w.task("log-screen"), w.task("log-merge")

		// hk 3639: review/mwr-* by age.
		jobPrune := "mwr-780-0000000-p1-0"
		byAge := w.doc("review/"+jobPrune, jobPrune, &tPrune)
		noTask := w.doc("review/mwr-780-0000000-p2-0", jobPrune, nil)
		// Through s.begin, so a MySQL sync trigger sees the transaction's sync id.
		tx, release, err := s.begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, s.q(`UPDATE documents SET updated_at=$1 WHERE key=$2 OR key=$3`, "UPDATE documents SET updated_at=? WHERE `key`=? OR `key`=?"), w.now.Add(-48*time.Hour), byAge.Key, noTask.Key)
		if err == nil {
			err = tx.Commit(ctx)
		}
		_ = tx.Rollback(ctx)
		release()
		if err != nil {
			t.Fatal(err)
		}
		pruned, err := s.PruneDocuments(ctx, DocPrunePrefix, w.now.Add(-24*time.Hour), 10, true, "log-test")
		if err != nil || pruned.Deleted != 2 {
			t.Fatalf("PruneDocuments = %+v err=%v, want 2 deleted", pruned, err)
		}

		// hk 5057 review leg: the reviewer's job id.
		jobReview := "mwr-781-1111111-p1-1"
		cid := w.change("example/app", 781, "merged", w.daysAgo(40), sha40('1'))
		w.exec(`INSERT INTO reviews(change_id, reviewed_head, round, pass, lens, recorded_at, reviewer_job_id) VALUES($1,$2,2,1,'correctness',$3,$4)`,
			`INSERT INTO reviews(change_id, reviewed_head, round, pass, lens, recorded_at, reviewer_job_id) VALUES(?,?,2,1,'correctness',?,?)`, cid, sha40('1'), w.now, jobReview)
		review := w.doc("review/"+jobReview, jobReview, &tReview)
		// hk 5057 screens leg: the job ended.
		screen := w.doc("screens/job-log-end/pane.txt", "job-log-end", &tScreen)
		w.endJob("job-log-end", 40)
		// The merge leg: a briefs/mwr- document no legacy leg matches.
		jobMerge := "mwr-783-3333333-p1-3"
		w.change("example/app", 783, "merged", w.daysAgo(40), sha40('3'), tMerge)
		brief := w.doc("briefs/"+jobMerge, jobMerge, &tMerge)

		s.setRetentionMergeLinkApply(true)
		res := w.run(true)
		if res.ReviewDeleted != 1 || res.ScreensDeleted != 1 || res.MergeLinkDeleted != 1 {
			t.Fatalf("retention apply = %+v, want one per leg", res)
		}
		w.assertKeys([]string{byAge.Key, noTask.Key, review.Key, screen.Key, brief.Key}, nil)

		for _, c := range []struct {
			key, job string
			task     *int64
		}{
			{byAge.Key, jobPrune, &tPrune},
			{noTask.Key, jobPrune, nil},
			{review.Key, jobReview, &tReview},
			{screen.Key, "job-log-end", &tScreen},
			{brief.Key, jobMerge, &tMerge},
		} {
			job, task := w.logged(c.key)
			if job == nil || *job != c.job {
				t.Fatalf("%s: logged job %v, want %q", c.key, job, c.job)
			}
			if (task == nil) != (c.task == nil) || (task != nil && *task != *c.task) {
				t.Fatalf("%s: logged task %v, want %v", c.key, task, c.task)
			}
		}
	})
}

// TestAdmitMergeJobsWholeJobsOnly is the limit rule without a database: whole
// jobs in order while they fit, a job that does not fit skipped whole, and a
// smaller later job still admitted.
func TestAdmitMergeJobsWholeJobsOnly(t *testing.T) {
	rows := func(job string, ids ...int64) []retentionRow {
		var out []retentionRow
		for _, id := range ids {
			out = append(out, retentionRow{id: id, job: job})
		}
		return out
	}
	all := slices.Concat(rows("a", 1, 2, 3), rows("b", 4, 5, 6, 7), rows("c", 8))
	for _, c := range []struct {
		capacity          int
		wantIDs           []int64
		wantJobs, wantSki []string
	}{
		{0, nil, nil, []string{"a", "b", "c"}},
		{1, []int64{8}, []string{"c"}, []string{"a", "b"}},
		{2, []int64{8}, []string{"c"}, []string{"a", "b"}},
		{4, []int64{1, 2, 3, 8}, []string{"a", "c"}, []string{"b"}},
		{8, []int64{1, 2, 3, 4, 5, 6, 7, 8}, []string{"a", "b", "c"}, nil},
	} {
		admitted, jobs, skipped := admitMergeJobs([]string{"a", "b"}, all, c.capacity)
		var ids []int64
		for _, r := range admitted {
			ids = append(ids, r.id)
		}
		if len(ids) > c.capacity || !slices.Equal(ids, c.wantIDs) || !slices.Equal(jobs, c.wantJobs) || !slices.Equal(skipped, c.wantSki) {
			t.Fatalf("capacity %d: ids %v jobs %v skipped %v, want %v %v %v", c.capacity, ids, jobs, skipped, c.wantIDs, c.wantJobs, c.wantSki)
		}
	}
}

// TestMergeLinkCountsSQLReadsPruneLog: the dry-run classification reads the
// doc_prune_log evidence on both dialects. Run with -v to print the rendered
// MySQL statement and its arguments.
func TestMergeLinkCountsSQLReadsPruneLog(t *testing.T) {
	cutoff := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, mysql := range []bool{false, true} {
		q, args := mergeLinkCountsSQL(mysql, cutoff)
		if !strings.Contains(q, "FROM doc_prune_log pl") || !strings.Contains(q, "pl.task_id IS NOT NULL") {
			t.Fatalf("mysql=%v: counts SQL does not read doc_prune_log:\n%s", mysql, q)
		}
		if mysql {
			if n := strings.Count(q, "?"); n != len(args) {
				t.Fatalf("mysql: %d placeholders, %d args", n, len(args))
			}
			t.Logf("MySQL dry-run SQL:\n%s\nargs: %v", q, args)
		}
	}
}

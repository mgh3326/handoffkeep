package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	testSHA1 = "1111111111111111111111111111111111111111"
	testSHA2 = "2222222222222222222222222222222222222222"
	testSHA3 = "3333333333333333333333333333333333333333"
)

// changeRepo is a repo name no other test uses: changes(repo, pr_number) is
// global in the shared schema.
func changeRepo(name string) string {
	return fmt.Sprintf("example/hk3037-%s-%d", name, time.Now().UnixNano())
}

func repoPR(repo string, n int) string { return fmt.Sprintf("https://github.com/%s/pull/%d", repo, n) }

type changeRow struct {
	ID                int64
	State             string
	Head, Base, Merge *string
	MergedAt          *time.Time
	RevertedBy        *int64
	Priority          int
	DependsOn         *int64
	DependsOnState    *string
}

func readChange(t *testing.T, s *Store, repo string, n int) (changeRow, bool) {
	t.Helper()
	var c changeRow
	rows, err := s.pool.Query(t.Context(), `SELECT id,state,head_sha,base_sha,merge_sha,merged_at,reverted_by_change_id,priority,depends_on_change_id,depends_on_state FROM changes WHERE repo=$1 AND pr_number=$2`, repo, n)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		return c, false
	}
	if err = rows.Scan(&c.ID, &c.State, &c.Head, &c.Base, &c.Merge, &c.MergedAt, &c.RevertedBy, &c.Priority, &c.DependsOn, &c.DependsOnState); err != nil {
		t.Fatal(err)
	}
	return c, true
}

func mustChange(t *testing.T, s *Store, repo string, n int) changeRow {
	t.Helper()
	c, ok := readChange(t, s, repo, n)
	if !ok {
		t.Fatalf("no change for %s#%d", repo, n)
	}
	return c
}

func changeLinks(t *testing.T, s *Store, changeID int64) map[int64]string {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), `SELECT task_id,relation FROM change_tasks WHERE change_id=$1`, changeID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var rel string
		if err = rows.Scan(&id, &rel); err != nil {
			t.Fatal(err)
		}
		out[id] = rel
	}
	return out
}

func countReviews(t *testing.T, s *Store, changeID int64) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM reviews WHERE change_id=$1`, changeID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestChangesMigration(t *testing.T) {
	s := schemaStore(t, "changes_mig")
	ctx := t.Context()
	for _, table := range []string{"changes", "change_tasks", "reviews"} {
		var found bool
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass(format('%I.%I', current_schema(), $1::text)) IS NOT NULL`, table).Scan(&found); err != nil || !found {
			t.Fatalf("table %s: found=%v err=%v", table, found, err)
		}
	}
	var version int
	if err := s.pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("max(version)=%d err=%v, want %d", version, err, SchemaVersion)
	}
	var id int64
	now := time.Now().UTC()
	if err := s.pool.QueryRow(ctx, `INSERT INTO changes(repo,pr_number,opened_at,created_at,updated_at) VALUES('example/mig',1,$1,$1,$1) RETURNING id`, now).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO changes(repo,pr_number,opened_at,created_at,updated_at) VALUES('example/mig',1,$1,$1,$1)`, now); err == nil {
		t.Fatal("(repo, pr_number) is not unique")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE changes SET depends_on_change_id=id, depends_on_state='merged' WHERE id=$1`, id); err == nil {
		t.Fatal("a self-dependency was accepted")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE changes SET depends_on_state='merged' WHERE id=$1`, id); err == nil {
		t.Fatal("a dependency state without a target was accepted")
	}
	insert := `INSERT INTO reviews(change_id,reviewed_head,round,pass,lens,recorded_at) VALUES($1,$2,1,1,'correctness',$3)`
	if _, err := s.pool.Exec(ctx, insert, id, testSHA1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, insert, id, testSHA1, now); err == nil {
		t.Fatal("(change_id, reviewed_head, lens, round, pass) is not unique")
	}
}

// TestTaskSourceEventPayload needs no database.
func TestTaskSourceEventPayload(t *testing.T) {
	pr := repoPR("example/app", 7)
	base := func(source string) TaskSourceEvent {
		in := TaskSourceEvent{Source: source, SourceRef: pr, TaskID: 1}
		if source == SourceMergewakeRevert {
			in.RevertedPR = repoPR("example/app", 6)
		}
		return in
	}
	review := TaskEventReview{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness"}

	t.Run("old payload normalizes as before", func(t *testing.T) {
		for _, tc := range []struct {
			in   TaskSourceEvent
			want TaskSourceEvent
		}{
			{base(SourceMergewakePR), TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: 1, PR: pr}},
			{base(SourceMergewakeReview), TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: pr, TaskID: 1, PR: pr}},
			{base(SourceMergewakeMerge), TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: 1, PR: pr}},
			{base(SourceMergewakeRevert), TaskSourceEvent{Source: SourceMergewakeRevert, SourceRef: pr, TaskID: 1, RevertedPR: repoPR("example/app", 6), RevertPR: pr, PR: repoPR("example/app", 6)}},
		} {
			got, err := normalizeTaskSourceEvent(tc.in)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: got %+v err=%v, want %+v", tc.in.Source, got, err, tc.want)
			}
		}
	})
	t.Run("new fields are accepted where allowed", func(t *testing.T) {
		in := base(SourceMergewakeMerge)
		in.HeadSHA, in.BaseSHA, in.MergeSHA = testSHA1, testSHA2, testSHA3
		if _, err := normalizeTaskSourceEvent(in); err != nil {
			t.Fatalf("merge with all SHAs: %v", err)
		}
		in = base(SourceMergewakeReview)
		in.HeadSHA, in.Reviews = testSHA1, []TaskEventReview{review}
		if _, err := normalizeTaskSourceEvent(in); err != nil {
			t.Fatalf("review with reviews: %v", err)
		}
		in = base(SourceMergewakeRevert)
		in.HeadSHA, in.BaseSHA = testSHA1, testSHA2
		if _, err := normalizeTaskSourceEvent(in); err != nil {
			t.Fatalf("revert with head and base: %v", err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		sixteen := make([]TaskEventReview, 16)
		seventeen := make([]TaskEventReview, 17)
		for i := range seventeen {
			seventeen[i] = review
			if i < 16 {
				sixteen[i] = review
			}
		}
		n := func(v int) *int { return &v }
		for name, mod := range map[string]func(*TaskSourceEvent){
			"bad head_sha":        func(in *TaskSourceEvent) { in.HeadSHA = "abc" },
			"upper-case head_sha": func(in *TaskSourceEvent) { in.HeadSHA = strings.Repeat("A", 40) },
			"bad base_sha":        func(in *TaskSourceEvent) { in.BaseSHA = testSHA1 + "0" },
			"merge_sha on a pr":   func(in *TaskSourceEvent) { in.MergeSHA = testSHA1 },
			"bad merge_sha":       func(in *TaskSourceEvent) { in.Source, in.MergeSHA = SourceMergewakeMerge, "xyz" },
			"reviews on a pr":     func(in *TaskSourceEvent) { in.Reviews = []TaskEventReview{review} },
			"reviews on a merge":  func(in *TaskSourceEvent) { in.Source, in.Reviews = SourceMergewakeMerge, []TaskEventReview{review} },
			"17 reviews":          func(in *TaskSourceEvent) { in.Source, in.Reviews = SourceMergewakeReview, seventeen },
			"no reviewed_head": func(in *TaskSourceEvent) {
				in.Source, in.Reviews = SourceMergewakeReview, []TaskEventReview{{Round: 1, Pass: 1, Lens: "x"}}
			},
			"round 0": func(in *TaskSourceEvent) {
				in.Source, in.Reviews = SourceMergewakeReview, []TaskEventReview{{ReviewedHead: testSHA1, Round: 0, Pass: 1, Lens: "x"}}
			},
			"pass 0": func(in *TaskSourceEvent) {
				in.Source, in.Reviews = SourceMergewakeReview, []TaskEventReview{{ReviewedHead: testSHA1, Round: 1, Pass: 0, Lens: "x"}}
			},
			"upper-case lens": func(in *TaskSourceEvent) {
				in.Source, in.Reviews = SourceMergewakeReview, []TaskEventReview{{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "Correctness"}}
			},
			"missing lens": func(in *TaskSourceEvent) {
				in.Source, in.Reviews = SourceMergewakeReview, []TaskEventReview{{ReviewedHead: testSHA1, Round: 1, Pass: 1}}
			},
			"bad verdict": func(in *TaskSourceEvent) {
				in.Source = SourceMergewakeReview
				r := review
				r.Verdict = "not ok"
				in.Reviews = []TaskEventReview{r}
			},
			"bad family": func(in *TaskSourceEvent) {
				in.Source = SourceMergewakeReview
				r := review
				r.Family = "a b"
				in.Reviews = []TaskEventReview{r}
			},
			"bad profile": func(in *TaskSourceEvent) {
				in.Source = SourceMergewakeReview
				r := review
				r.Profile = "A!"
				in.Reviews = []TaskEventReview{r}
			},
			"negative count": func(in *TaskSourceEvent) {
				in.Source = SourceMergewakeReview
				r := review
				r.High = n(-1)
				in.Reviews = []TaskEventReview{r}
			},
			"long text": func(in *TaskSourceEvent) {
				in.Source = SourceMergewakeReview
				r := review
				r.Model = strings.Repeat("m", 201)
				in.Reviews = []TaskEventReview{r}
			},
			"finished before start": func(in *TaskSourceEvent) {
				in.Source = SourceMergewakeReview
				r, t1 := review, time.Now()
				t0 := t1.Add(-time.Hour)
				r.StartedAt, r.FinishedAt = &t1, &t0
				in.Reviews = []TaskEventReview{r}
			},
		} {
			in := base(SourceMergewakePR)
			mod(&in)
			if in.Source == SourceMergewakeRevert {
				in.RevertedPR = repoPR("example/app", 6)
			}
			if _, err := normalizeTaskSourceEvent(in); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		in := base(SourceMergewakeReview)
		in.Reviews = sixteen
		if _, err := normalizeTaskSourceEvent(in); err != nil {
			t.Errorf("16 reviews refused: %v", err)
		}
	})
	t.Run("a launch profile with @ is a valid profile", func(t *testing.T) {
		in := base(SourceMergewakeReview)
		r := review
		r.Profile = "sonnet@high"
		in.Reviews = []TaskEventReview{r}
		if _, err := normalizeTaskSourceEvent(in); err != nil {
			t.Fatal(err)
		}
	})
}

func TestChangeFromEvents(t *testing.T) {
	s := schemaStore(t, "change_events")
	pool := s.pool
	task := func(t *testing.T, state string) Task { return flowTask(t, s, pool, "implement", state, false) }

	t.Run("pr", func(t *testing.T) {
		repo, x := changeRepo("pr"), task(t, StateBacklog)
		send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 1), TaskID: x.ID, HeadSHA: testSHA1, BaseSHA: testSHA2})
		c := mustChange(t, s, repo, 1)
		if c.State != ChangeOpen || c.Head == nil || *c.Head != testSHA1 || c.Base == nil || *c.Base != testSHA2 || c.Merge != nil || c.MergedAt != nil {
			t.Fatalf("change %+v", c)
		}
		if got := changeLinks(t, s, c.ID); !reflect.DeepEqual(got, map[int64]string{x.ID: ChangeImplements}) {
			t.Fatalf("links %v", got)
		}
	})
	t.Run("a second task on the same PR", func(t *testing.T) {
		repo, a, b := changeRepo("two"), task(t, StateBacklog), task(t, StateBacklog)
		send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 2), TaskID: a.ID})
		send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 2), TaskID: b.ID})
		var n int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM changes WHERE repo=$1`, repo).Scan(&n); err != nil || n != 1 {
			t.Fatalf("changes=%d err=%v", n, err)
		}
		if got := changeLinks(t, s, mustChange(t, s, repo, 2).ID); len(got) != 2 || got[a.ID] != ChangeImplements || got[b.ID] != ChangeImplements {
			t.Fatalf("links %v", got)
		}
	})
	t.Run("head and base follow the latest event while open", func(t *testing.T) {
		repo, x := changeRepo("head"), task(t, StateBacklog)
		send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 3), TaskID: x.ID, HeadSHA: testSHA1})
		send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: repoPR(repo, 3), TaskID: x.ID, HeadSHA: testSHA2})
		send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: repoPR(repo, 3), TaskID: x.ID})
		if c := mustChange(t, s, repo, 3); c.Head == nil || *c.Head != testSHA2 {
			t.Fatalf("head %v", c.Head)
		}
	})
	t.Run("merge", func(t *testing.T) {
		repo, x := changeRepo("merge"), task(t, StateBacklog)
		pr := repoPR(repo, 4)
		send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID})
		at := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
		send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: x.ID, MergeSHA: testSHA3, HeadSHA: testSHA1, At: &at})
		c := mustChange(t, s, repo, 4)
		if c.State != ChangeMerged || c.MergedAt == nil || !c.MergedAt.Equal(at) || c.Merge == nil || *c.Merge != testSHA3 || c.Head == nil || *c.Head != testSHA1 {
			t.Fatalf("change %+v", c)
		}
		// A redelivered merge keeps the first merged_at.
		later := time.Now().UTC().Add(-time.Second)
		send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: x.ID, At: &later})
		if again := mustChange(t, s, repo, 4); !again.MergedAt.Equal(at) {
			t.Fatalf("merged_at moved to %v", again.MergedAt)
		}
	})
	t.Run("a merge for a PR no pr event announced", func(t *testing.T) {
		repo, x := changeRepo("mergefirst"), task(t, StateBacklog)
		send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: repoPR(repo, 5), TaskID: x.ID})
		if c := mustChange(t, s, repo, 5); c.State != ChangeMerged || c.MergedAt == nil {
			t.Fatalf("change %+v", c)
		}
	})
	t.Run("a late pr after merge", func(t *testing.T) {
		repo, x := changeRepo("late"), task(t, StateBacklog)
		pr := repoPR(repo, 6)
		send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: x.ID, HeadSHA: testSHA1})
		send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID, HeadSHA: testSHA2, BaseSHA: testSHA2})
		send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: pr, TaskID: x.ID, HeadSHA: testSHA2})
		c := mustChange(t, s, repo, 6)
		if c.State != ChangeMerged || c.Head == nil || *c.Head != testSHA1 || c.Base != nil {
			t.Fatalf("a late event changed a merged change: %+v", c)
		}
	})
	t.Run("revert", func(t *testing.T) {
		repo, x := changeRepo("revert"), task(t, StateBacklog)
		pr, revert := repoPR(repo, 7), repoPR(repo, 8)
		send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID})
		send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: x.ID})
		at := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
		send(t, s, TaskSourceEvent{Source: SourceMergewakeRevert, SourceRef: revert, RevertedPR: pr, TaskID: x.ID, At: &at})
		reverted, rv := mustChange(t, s, repo, 7), mustChange(t, s, repo, 8)
		if reverted.RevertedBy == nil || *reverted.RevertedBy != rv.ID {
			t.Fatalf("reverted_by %v, want %d", reverted.RevertedBy, rv.ID)
		}
		if rv.State != ChangeMerged || rv.MergedAt == nil || !rv.MergedAt.Equal(at) {
			t.Fatalf("revert change %+v", rv)
		}
		if got := changeLinks(t, s, rv.ID); !reflect.DeepEqual(got, map[int64]string{x.ID: ChangeReferences}) {
			t.Fatalf("revert links %v", got)
		}
		if got := changeLinks(t, s, reverted.ID); got[x.ID] != ChangeImplements {
			t.Fatalf("reverted links %v", got)
		}
	})
	t.Run("an event for a missing task", func(t *testing.T) {
		repo := changeRepo("missing")
		_, err := s.ApplyTaskSourceEvent(t.Context(), TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 9), TaskID: 987654321})
		if !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("err %v", err)
		}
		if _, ok := readChange(t, s, repo, 9); ok {
			t.Fatal("a 404 wrote a change")
		}
	})
	t.Run("an event the task refuses still writes the change", func(t *testing.T) {
		for _, state := range []string{StateTriage, StateDropped} {
			repo, x := changeRepo("refused-"+state), task(t, state)
			res := send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 10), TaskID: x.ID})
			if res.Applied {
				t.Fatalf("%s: the event applied", state)
			}
			c := mustChange(t, s, repo, 10)
			if got := changeLinks(t, s, c.ID); got[x.ID] != ChangeImplements {
				t.Fatalf("%s: links %v", state, got)
			}
		}
	})
	t.Run("a payload without the new fields leaves the task result alone", func(t *testing.T) {
		repo, x := changeRepo("old"), task(t, StateBacklog)
		res := send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, 11), TaskID: x.ID})
		if !res.Applied || res.State != StateReviewing {
			t.Fatalf("result %+v", res)
		}
	})
}

func TestReviewsFromEvents(t *testing.T) {
	s := schemaStore(t, "change_reviews")
	repo := changeRepo("reviews")
	x := flowTask(t, s, s.pool, "implement", StateBacklog, false)
	pr := repoPR(repo, 1)
	n := func(v int) *int { return &v }
	yes := true
	first := TaskEventReview{ReviewedHead: testSHA1, ReviewedBase: testSHA2, PatchID: "abcdef0123", Round: 1, Pass: 1, Lens: "correctness",
		Family: "openai", Profile: "gpt@high", Model: "m", Executor: "codex", Fallback: &yes, Verdict: "pass", Critical: n(0), High: n(2), Medium: n(1), Low: n(0)}

	res := send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: pr, TaskID: x.ID, HeadSHA: testSHA1, Reviews: []TaskEventReview{first}})
	if !res.Applied {
		t.Fatalf("first review: %+v", res)
	}
	c := mustChange(t, s, repo, 1)
	if got := countReviews(t, s, c.ID); got != 1 {
		t.Fatalf("reviews=%d", got)
	}

	// A second review run on the same PR is redelivered for the task and still
	// records its reviews.
	second := first
	second.ReviewedHead, second.Round = testSHA3, 2
	res = send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: pr, TaskID: x.ID, HeadSHA: testSHA3, Reviews: []TaskEventReview{second}})
	if !res.Redelivered {
		t.Fatalf("second run should be redelivered: %+v", res)
	}
	if got := countReviews(t, s, c.ID); got != 2 {
		t.Fatalf("reviews after second run=%d", got)
	}

	// Resending the same key leaves one row, the first write, and no error.
	changed := first
	changed.Verdict, changed.High = "fail", n(9)
	send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: pr, TaskID: x.ID, Reviews: []TaskEventReview{changed, first}})
	if got := countReviews(t, s, c.ID); got != 2 {
		t.Fatalf("reviews after resend=%d", got)
	}
	var verdict string
	var high int
	var fallback bool
	if err := s.pool.QueryRow(t.Context(), `SELECT verdict,high,fallback FROM reviews WHERE change_id=$1 AND reviewed_head=$2`, c.ID, testSHA1).Scan(&verdict, &high, &fallback); err != nil {
		t.Fatal(err)
	}
	if verdict != "pass" || high != 2 || !fallback {
		t.Fatalf("first write did not win: verdict=%s high=%d fallback=%v", verdict, high, fallback)
	}

	// A different pass of the same head is its own row, and an unknown count
	// stays NULL.
	other := first
	other.Pass, other.Critical, other.High, other.Medium, other.Low = 2, nil, nil, nil, nil
	send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: pr, TaskID: x.ID, Reviews: []TaskEventReview{other}})
	var nulls int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM reviews WHERE change_id=$1 AND pass=2 AND critical IS NULL`, c.ID).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("unknown counts: n=%d err=%v", nulls, err)
	}
}

func TestChangeMergeFlowStore(t *testing.T) {
	s := schemaStore(t, "change_flow")
	repo := changeRepo("flow")
	x := flowTask(t, s, s.pool, "implement", StateBacklog, false)
	ids := map[int]int64{}
	for i := 1; i <= 3; i++ {
		send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(repo, i), TaskID: x.ID})
		ids[i] = mustChange(t, s, repo, i).ID
	}
	dep := func(n int) *int64 { v := ids[n]; return &v }

	if _, err := s.SetChangeMergeFlow(t.Context(), ids[1], ChangeMergeFlow{Priority: 5, HoldReason: "waiting", DependsOnChange: dep(2), DependsOnState: "merged"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetChangeMergeFlow(t.Context(), ids[2], ChangeMergeFlow{DependsOnChange: dep(1), DependsOnState: "deployed"}); !errors.Is(err, ErrChangeDependencyCycle) {
		t.Fatalf("two-step cycle: %v", err)
	}
	if _, err := s.SetChangeMergeFlow(t.Context(), ids[3], ChangeMergeFlow{DependsOnChange: dep(3), DependsOnState: "merged"}); !errors.Is(err, ErrChangeDependencyCycle) {
		t.Fatalf("self dependency: %v", err)
	}
	// 3 -> 1 -> 2 is a chain, and 2 -> 3 would close it.
	if _, err := s.SetChangeMergeFlow(t.Context(), ids[3], ChangeMergeFlow{DependsOnChange: dep(1), DependsOnState: "merged"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetChangeMergeFlow(t.Context(), ids[2], ChangeMergeFlow{DependsOnChange: dep(3), DependsOnState: "merged"}); !errors.Is(err, ErrChangeDependencyCycle) {
		t.Fatalf("three-step cycle: %v", err)
	}
	if _, err := s.SetChangeMergeFlow(t.Context(), 987654321, ChangeMergeFlow{}); !errors.Is(err, ErrChangeNotFound) {
		t.Fatalf("unknown change: %v", err)
	}
	if _, err := s.SetChangeMergeFlow(t.Context(), ids[1], ChangeMergeFlow{DependsOnChange: func() *int64 { v := int64(987654321); return &v }(), DependsOnState: "merged"}); !errors.Is(err, ErrChangeNotFound) {
		t.Fatalf("unknown target: %v", err)
	}
	for name, in := range map[string]ChangeMergeFlow{
		"negative priority": {Priority: -1},
		"long hold_reason":  {HoldReason: strings.Repeat("x", MaxHoldReason+1)},
		"state alone":       {DependsOnState: "merged"},
		"target alone":      {DependsOnChange: dep(2)},
		"bad state":         {DependsOnChange: dep(2), DependsOnState: "open"},
	} {
		if _, err := s.SetChangeMergeFlow(t.Context(), ids[1], in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A write replaces all three fields.
	if _, err := s.SetChangeMergeFlow(t.Context(), ids[1], ChangeMergeFlow{}); err != nil {
		t.Fatal(err)
	}
	if c := mustChange(t, s, repo, 1); c.Priority != 0 || c.DependsOn != nil || c.DependsOnState != nil {
		t.Fatalf("not cleared: %+v", c)
	}
}

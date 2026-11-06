package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func strPtr(v string) *string { return &v }

// listReviews reads a change's reviews through the list read, the shape the
// API returns.
func listReviews(t *testing.T, s *Store, repo string) []ChangeReview {
	t.Helper()
	cs, err := s.ListChangesByPR(t.Context(), repoPR(repo, 1), 10)
	if err != nil || len(cs) != 1 {
		t.Fatalf("ListChangesByPR: n=%d err=%v", len(cs), err)
	}
	return cs[0].Reviews
}

func reviewJSON(t *testing.T, r ChangeReview) map[string]any {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

var reviewHistoryKeys = []string{"reviewer", "same_family", "delta", "quiet", "outside_diff_blocking", "patch_digest"}

func TestReviewHistoryRoundTrip(t *testing.T) {
	s := schemaStore(t, "review_history")
	repo := "example/review-history"
	x := flowTask(t, s, s.pool, "implement", StateBacklog, false)
	pr := repoPR(repo, 1)
	digest := strings.Repeat("ab", 16)
	full := TaskEventReview{ReviewedHead: testSHA1, PatchID: "abcdef0123", Round: 1, Pass: 1, Lens: "correctness", Verdict: "pass",
		ReviewHistory: ReviewHistory{Reviewer: strPtr("sonnet@high"), SameFamily: boolPtr(false), Delta: boolPtr(true), Quiet: boolPtr(false), OutsideDiffBlocking: intPtr(0), PatchDigest: &digest}}
	plain := TaskEventReview{ReviewedHead: testSHA1, Round: 1, Pass: 2, Lens: "correctness", Verdict: "pass"}
	send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: pr, TaskID: x.ID, HeadSHA: testSHA1, Reviews: []TaskEventReview{full, plain}})

	byPass := map[int]ChangeReview{}
	for _, r := range listReviews(t, s, repo) {
		byPass[r.Pass] = r
	}
	got := byPass[1]
	if got.Reviewer == nil || *got.Reviewer != "sonnet@high" || got.SameFamily == nil || *got.SameFamily || got.Delta == nil || !*got.Delta ||
		got.Quiet == nil || *got.Quiet || got.OutsideDiffBlocking == nil || *got.OutsideDiffBlocking != 0 || got.PatchDigest == nil || *got.PatchDigest != digest {
		t.Fatalf("round trip: %+v", got)
	}
	if got.PatchID == nil || *got.PatchID != "abcdef0123" {
		t.Fatalf("patch_id must stay: %+v", got.PatchID)
	}
	// false and 0 are values, not absence: every key is present.
	m := reviewJSON(t, got)
	for _, k := range reviewHistoryKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON of a full review has no %q: %v", k, m)
		}
	}
	// A review sent without them stores NULLs and reads back without the keys.
	m = reviewJSON(t, byPass[2])
	for _, k := range reviewHistoryKeys {
		if _, ok := m[k]; ok {
			t.Errorf("JSON of a review without history carries %q: %v", k, m)
		}
	}
	var nulls int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM reviews WHERE pass=2 AND reviewer IS NULL AND same_family IS NULL AND delta IS NULL AND quiet IS NULL AND outside_diff_blocking IS NULL AND patch_digest IS NULL AND change_id=(SELECT id FROM changes WHERE repo=$1)`, repo).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("NULL columns: n=%d err=%v", nulls, err)
	}

	// A redelivery of the same key changes nothing, the history fields included.
	again := full
	again.Reviewer, again.PatchDigest = strPtr("other"), strPtr(strings.Repeat("cd", 16))
	send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: pr, TaskID: x.ID, Reviews: []TaskEventReview{again}})
	if r := listReviews(t, s, repo)[0]; r.Pass != 1 || *r.Reviewer != "sonnet@high" || *r.PatchDigest != digest {
		t.Fatalf("redelivery changed the row: %+v", r)
	}
	// And a redelivery that only now carries the fields does not fill a row an event already reported.
	late := plain
	late.Reviewer = strPtr("late")
	send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: pr, TaskID: x.ID, Reviews: []TaskEventReview{late}})
	for _, r := range listReviews(t, s, repo) {
		if r.Pass == 2 && r.Reviewer != nil {
			t.Fatalf("a reported row was filled by a redelivery: %+v", r)
		}
	}
}

func TestReviewHistoryFillsBareRow(t *testing.T) {
	f := newStateFixture(t, "rh_bare")
	digest := strings.Repeat("0a", 20)
	x := flowTask(t, f.s, f.s.pool, "implement", StateBacklog, false)
	event := func(rv TaskEventReview) {
		t.Helper()
		send(t, f.s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: repoPR(f.ref.Repo, f.ref.PR), TaskID: x.ID, Reviews: []TaskEventReview{rv}})
	}
	review := func(pass int) ChangeReview {
		t.Helper()
		for _, r := range listReviews(t, f.s, f.ref.Repo) {
			if r.Pass == pass {
				return r
			}
		}
		t.Fatalf("no review for pass %d", pass)
		return ChangeReview{}
	}

	// Pass 1: a bare row from the findings write, then an event with the fields.
	f.apply(t, ChangeBatch{Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness", ObservedAt: f.at(1)}})
	if r := review(1); r.Reviewer != nil || r.Verdict != nil {
		t.Fatalf("bare row is not bare: %+v", r)
	}
	event(TaskEventReview{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness", Verdict: "pass",
		ReviewHistory: ReviewHistory{Reviewer: strPtr("opus@high"), SameFamily: boolPtr(true), OutsideDiffBlocking: intPtr(2), PatchDigest: &digest}})
	r := review(1)
	if r.Verdict == nil || *r.Verdict != "pass" || r.Reviewer == nil || *r.Reviewer != "opus@high" || r.SameFamily == nil || !*r.SameFamily ||
		r.OutsideDiffBlocking == nil || *r.OutsideDiffBlocking != 2 || r.PatchDigest == nil || *r.PatchDigest != digest || r.Delta != nil || r.Quiet != nil {
		t.Fatalf("event did not fill the bare row: %+v", r)
	}

	// Pass 2: the findings write itself carries the fields; a later event fills
	// the rest of the row and the fields the findings write set stay.
	f.apply(t, ChangeBatch{Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: 2, Lens: "correctness", ObservedAt: f.at(2),
		ReviewHistory: ReviewHistory{Reviewer: strPtr("first"), Delta: boolPtr(true)}}})
	if r = review(2); r.Reviewer == nil || *r.Reviewer != "first" || r.Delta == nil || !*r.Delta || r.Verdict != nil {
		t.Fatalf("findings write did not create the row with its fields: %+v", r)
	}
	event(TaskEventReview{ReviewedHead: testSHA1, Round: 1, Pass: 2, Lens: "correctness", Verdict: "fail",
		ReviewHistory: ReviewHistory{Reviewer: strPtr("second"), Quiet: boolPtr(true)}})
	if r = review(2); r.Verdict == nil || *r.Verdict != "fail" || *r.Reviewer != "first" || r.Quiet == nil || !*r.Quiet || !*r.Delta {
		t.Fatalf("event after a findings write with fields: %+v", r)
	}

	// Pass 3: a findings write with fields fills a row that is still bare, and
	// leaves a row an event already reported alone.
	f.apply(t, ChangeBatch{Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: 3, Lens: "correctness", ObservedAt: f.at(3)}})
	f.apply(t, ChangeBatch{Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: 3, Lens: "correctness", ObservedAt: f.at(4),
		ReviewHistory: ReviewHistory{Reviewer: strPtr("filled"), PatchDigest: &digest}}})
	if r = review(3); r.Reviewer == nil || *r.Reviewer != "filled" || r.PatchDigest == nil {
		t.Fatalf("findings write did not fill a bare row: %+v", r)
	}
	f.apply(t, ChangeBatch{Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness", ObservedAt: f.at(5),
		ReviewHistory: ReviewHistory{Delta: boolPtr(true), Reviewer: strPtr("late")}}})
	if r = review(1); r.Delta != nil || *r.Reviewer != "opus@high" {
		t.Fatalf("findings write changed a reported row: %+v", r)
	}
	if n := f.count(t, `SELECT count(*) FROM reviews WHERE change_id=$1`, f.view(t, ChangeReadOptions{}).Change.ID); n != 3 {
		t.Fatalf("reviews=%d", n)
	}
}

func TestReviewHistoryValidation(t *testing.T) {
	good := TaskEventReview{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness"}
	for _, tc := range []struct {
		name string
		h    ReviewHistory
		want string
	}{
		{"digest upper case", ReviewHistory{PatchDigest: strPtr(strings.Repeat("AB", 16))}, "patch_digest"},
		{"digest too short", ReviewHistory{PatchDigest: strPtr("abcdef")}, "patch_digest"},
		{"digest too long", ReviewHistory{PatchDigest: strPtr(strings.Repeat("a", 129))}, "patch_digest"},
		{"digest not hex", ReviewHistory{PatchDigest: strPtr(strings.Repeat("g", 20))}, "patch_digest"},
		{"digest empty", ReviewHistory{PatchDigest: strPtr("")}, "patch_digest"},
		{"negative outside", ReviewHistory{OutsideDiffBlocking: intPtr(-1)}, "outside_diff_blocking"},
		{"huge outside", ReviewHistory{OutsideDiffBlocking: intPtr(maxReviewCount + 1)}, "outside_diff_blocking"},
		{"reviewer control", ReviewHistory{Reviewer: strPtr("a\nb")}, "reviewer"},
		{"reviewer empty", ReviewHistory{Reviewer: strPtr("")}, "reviewer"},
		{"reviewer long", ReviewHistory{Reviewer: strPtr(strings.Repeat("r", maxReviewText+1))}, "reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rv := good
			rv.ReviewHistory = tc.h
			if err := checkEventReview(rv); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("event: %v", err)
			}
			now := time.Now().UTC()
			w := ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness", ObservedAt: now, ReviewHistory: tc.h}
			err := checkChangeFindings(w, now)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), ErrInvalidChangeWrite.Error()+": ") {
				t.Fatalf("findings: %v", err)
			}
		})
	}
	ok := good
	ok.ReviewHistory = ReviewHistory{Reviewer: strPtr("sonnet@high"), OutsideDiffBlocking: intPtr(0), PatchDigest: strPtr(strings.Repeat("0", 16)), SameFamily: boolPtr(false)}
	if err := checkEventReview(ok); err != nil {
		t.Fatalf("valid history refused: %v", err)
	}
	ok.PatchDigest = strPtr(strings.Repeat("f", 128))
	if err := checkEventReview(ok); err != nil {
		t.Fatalf("128-digit digest refused: %v", err)
	}
}

func TestChangeCheckDeliveryID(t *testing.T) {
	f := newStateFixture(t, "delivery")
	checks := func(min int, delivery string, cs ...ChangeCheckIn) {
		t.Helper()
		f.apply(t, ChangeBatch{Checks: &ChangeChecksWrite{HeadSHA: testSHA1, Checks: cs, ObservedAt: f.at(min), Source: "webhook", DeliveryID: delivery}})
	}
	delivery := func() map[string]*string {
		out := map[string]*string{}
		for _, c := range *f.view(t, ChangeReadOptions{}).Checks {
			out[c.Name] = c.DeliveryID
		}
		return out
	}
	build := ChangeCheckIn{Name: "build", State: "completed", Conclusion: "success"}
	lint := ChangeCheckIn{Name: "lint", State: "queued"}
	checks(1, "d-1", build, lint)
	if got := delivery(); got["build"] == nil || *got["build"] != "d-1" || got["lint"] == nil || *got["lint"] != "d-1" {
		t.Fatalf("after first write: %v", got)
	}
	// A later write names only lint: build is not touched and keeps d-1.
	lint.State = "completed"
	checks(2, "d-2", lint)
	if got := delivery(); *got["build"] != "d-1" || *got["lint"] != "d-2" {
		t.Fatalf("untouched row lost or changed its delivery: %v %v", *got["build"], *got["lint"])
	}
	// A write without a delivery id that changes a row leaves that row with none.
	lint.Conclusion = "failure"
	checks(3, "", lint)
	if got := delivery(); *got["build"] != "d-1" || got["lint"] != nil {
		t.Fatalf("write without delivery_id: build=%v lint=%v", *got["build"], got["lint"])
	}
	m, err := json.Marshal((*f.view(t, ChangeReadOptions{}).Checks)[1])
	if err != nil || strings.Contains(string(m), "delivery_id") {
		t.Fatalf("a check without delivery carries the key: %s %v", m, err)
	}
	// Validation: same as the state write's delivery_id.
	now := time.Now().UTC()
	w := ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: now, Source: "webhook", DeliveryID: strings.Repeat("d", 129)}
	if err := checkChangeChecks(w, now); err == nil || !strings.Contains(err.Error(), "delivery_id") {
		t.Fatalf("long delivery_id: %v", err)
	}
	w.DeliveryID = strings.Repeat("d", 128)
	if err := checkChangeChecks(w, now); err != nil {
		t.Fatalf("128-byte delivery_id refused: %v", err)
	}
}

func TestReviewHistoryMigration(t *testing.T) {
	s := schemaStore(t, "review_history_mig")
	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND ((table_name='reviews' AND column_name IN ('reviewer','same_family','delta','quiet','outside_diff_blocking','patch_digest')) OR (table_name='change_checks' AND column_name='delivery_id'))`).Scan(&n); err != nil || n != 7 {
		t.Fatalf("new columns: n=%d err=%v", n, err)
	}
	var id int64
	now := time.Now().UTC()
	if err := s.pool.QueryRow(t.Context(), `INSERT INTO changes(repo,pr_number,opened_at,created_at,updated_at) VALUES('example/rhmig',1,$1,$1,$1) RETURNING id`, now).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO reviews(change_id,reviewed_head,round,pass,lens,recorded_at,outside_diff_blocking) VALUES($1,$2,1,1,'x',$3,-1)`, id, testSHA1, now); err == nil {
		t.Fatal("a negative outside_diff_blocking was accepted")
	}
}

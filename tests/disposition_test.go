package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// Disposition contract tests (#493). Design:
// hk:doc design/2026-09-21/task493-disposition-contract; ruling:
// hk:doc decision/2026-09-21/task493-phase1-review.

var dispositionSeq int64

func dispositionPR(t *testing.T) string {
	t.Helper()
	dispositionSeq++
	return fmt.Sprintf("https://github.com/example/repo%d/pull/%d", time.Now().UnixNano()%1_000_000, dispositionSeq)
}

func dispositionInput(lane, originPR string, residuals int, recommended string) store.DispositionInput {
	merged := time.Now().UTC().Add(-time.Hour)
	in := store.DispositionInput{Lane: lane, Title: "[처분] " + originPR, OriginPR: originPR, MergeSHA: strings.Repeat("a", 40), MergedAt: &merged,
		Install: store.DispositionInstall{State: "unknown"}, ResidualN: residuals, Recommended: recommended, CreatedBy: "director-node"}
	if residuals > 0 {
		in.ResidualDoc = "disposition/2026-09-21/fixture"
	}
	return in
}

func mustCreateDisposition(t *testing.T, s *store.Store, in store.DispositionInput) store.Task {
	t.Helper()
	x, created, err := s.CreateDisposition(t.Context(), in)
	if err != nil || !created {
		t.Fatalf("create disposition created=%v err=%v", created, err)
	}
	return x
}

// drainOpenDispositions closes every open item through the operator path
// (answer E, apply) so list- and count-sensitive tests start from zero. It
// uses no privileged SQL: task_events is append-only.
func drainOpenDispositions(t *testing.T, s *store.Store) {
	t.Helper()
	// Background, not t.Context(): this also runs from t.Cleanup.
	ctx := context.Background()
	open, err := s.ListOpenDispositions(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range open {
		eventID := fmt.Sprintf("drain-%d-g%d", item.Task.ID, item.Gen)
		if _, err := s.AnswerDisposition(ctx, store.DispositionAnswerInput{ID: item.Task.ID, Gen: item.Gen, Key: "E", OperatorEmail: "drain@example.com", EventID: eventID}); err != nil {
			t.Fatal(err)
		}
		// Mark the notification delivered so drained items never linger in the
		// console's pending-notice list.
		if _, _, err := s.AppendRelayEvent(ctx, store.RelayEvent{Kind: "lane.event", OwnerLane: item.Task.Lane, EventID: eventID, Text: "[decision] drain"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyDisposition(ctx, item.Task.ID, "test-drain", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func openGen(t *testing.T, s *store.Store, id int64) int64 {
	t.Helper()
	open, err := s.ListOpenDispositions(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range open {
		if item.Task.ID == id {
			return item.Gen
		}
	}
	t.Fatalf("disposition #%d is not open", id)
	return 0
}

type dispositionCounts struct {
	Tasks, TaskEvents, Relay, Backlog, ItemEvents int64
	ItemState                                     string
}

func dispositionRowCounts(t *testing.T, lane string, id int64) dispositionCounts {
	t.Helper()
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(t.Context())
	var c dispositionCounts
	if err := db.QueryRow(t.Context(), `SELECT (SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM task_events),(SELECT COUNT(*) FROM relay_events),
		(SELECT COUNT(*) FROM tasks WHERE lane=$1 AND state='backlog'),(SELECT COUNT(*) FROM task_events WHERE task_id=$2),(SELECT state FROM tasks WHERE id=$2)`, lane, id).
		Scan(&c.Tasks, &c.TaskEvents, &c.Relay, &c.Backlog, &c.ItemEvents, &c.ItemState); err != nil {
		t.Fatal(err)
	}
	return c
}

// AC ① / M11: one open item per origin; #324~#327 (four residuals of one PR)
// are one item with residual_n=4.
func TestDispositionCreateIsOnePerOrigin(t *testing.T) {
	s := uiStore(t)
	lane := uiLane(t, "director")
	pr := dispositionPR(t)
	x := mustCreateDisposition(t, s, dispositionInput(lane, pr, 4, "C"))
	if x.State != "needs_decision" || x.Kind != "decide" || x.Refs.OriginPR != pr || x.Refs.Disposition.Facts.ResidualN != 4 || x.Refs.Disposition.Facts.FactsSource != "gh-pr-view" {
		t.Fatalf("created item = %+v", x)
	}
	again, created, err := s.CreateDisposition(t.Context(), dispositionInput(lane, pr, 1, "E"))
	if err != nil || created || again.ID != x.ID {
		t.Fatalf("second create for the same origin: id=%d created=%v err=%v, want existing #%d", again.ID, created, err, x.ID)
	}
	other := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "E"))
	if other.ID == x.ID {
		t.Fatal("a different origin must create its own item")
	}
	got, _, err := s.GetTask(t.Context(), x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 2 || got.Events[0].From != "backlog" || got.Events[0].To != "claimed" || got.Events[1].From != "claimed" || got.Events[1].To != "needs_decision" {
		t.Fatalf("creation must record only legal edges, got %+v", got.Events)
	}
	if !strings.Contains(got.Events[1].Note, "잔여 4") || !strings.Contains(got.Events[1].Note, "권고 C 잔여 수용") || !strings.Contains(got.Events[1].Note, "설치 unknown") {
		t.Fatalf("question must carry the recorded facts, got %q", got.Events[1].Note)
	}
}

func TestDispositionCreateRejectsUnsourcedFacts(t *testing.T) {
	s := uiStore(t)
	lane := uiLane(t, "director")
	parent := createUITask(t, s, lane, "open parent")
	cases := map[string]func(*store.DispositionInput){
		"no origin":          func(in *store.DispositionInput) { in.OriginPR = "" },
		"two origins":        func(in *store.DispositionInput) { in.OriginTask = parent.ID },
		"short sha":          func(in *store.DispositionInput) { in.MergeSHA = "abc1234" },
		"no merged_at":       func(in *store.DispositionInput) { in.MergedAt = nil },
		"not a PR url":       func(in *store.DispositionInput) { in.OriginPR = "https://example.com/x/y/pull/1" },
		"bad recommendation": func(in *store.DispositionInput) { in.Recommended = "F" },
		"installed without witness": func(in *store.DispositionInput) {
			in.Install = store.DispositionInstall{State: "installed", TargetsPass: 1, TargetsTotal: 1}
		},
		"partial that is complete": func(in *store.DispositionInput) {
			in.Install = store.DispositionInstall{State: "partial", TargetsPass: 2, TargetsTotal: 2, Witness: "witness/x"}
		},
		"residuals without doc": func(in *store.DispositionInput) { in.ResidualN, in.ResidualDoc = 2, "" },
		"negative residuals":    func(in *store.DispositionInput) { in.ResidualN = -1 },
	}
	for name, mutate := range cases {
		in := dispositionInput(lane, dispositionPR(t), 0, "A")
		mutate(&in)
		if _, _, err := s.CreateDisposition(t.Context(), in); err == nil {
			t.Errorf("%s: create succeeded", name)
		}
	}
	// A parent-task origin must be terminal; hk reads that fact itself.
	in := store.DispositionInput{Lane: lane, Title: "[처분] #parent", OriginTask: parent.ID, Install: store.DispositionInstall{State: "unknown"}, Recommended: "E", CreatedBy: "director-node"}
	if _, _, err := s.CreateDisposition(t.Context(), in); err == nil {
		t.Fatal("non-terminal parent accepted")
	}
	claimAndTransition(t, s, parent, "dropped", "done")
	x, created, err := s.CreateDisposition(t.Context(), in)
	if err != nil || !created || x.Refs.Disposition.Facts.FactsSource != "hk-task" || x.Refs.OriginTask != parent.ID {
		t.Fatalf("terminal parent origin: %+v created=%v err=%v", x, created, err)
	}
	// Disposition refs cannot be forged through the generic create path.
	forged := store.Task{Lane: lane, Title: "forged", Kind: "decide", CreatedBy: "n", Refs: x.Refs}
	if _, err := s.CreateTask(t.Context(), forged); err == nil {
		t.Fatal("CreateTask accepted disposition refs")
	}
}

// M4 + M3: the item is never observable in backlog, so NextTask cannot claim it.
func TestDispositionIsNeverClaimable(t *testing.T) {
	s := uiStore(t)
	lane := uiLane(t, "director")
	mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A"))
	if _, err := s.NextTask(t.Context(), lane, "director"); !errors.Is(err, store.ErrQueueEmpty) {
		t.Fatalf("NextTask on a lane holding only a disposition item: err=%v, want queue_empty", err)
	}
}

// AC ② / M1 M2 M3: operator silence writes nothing, however much time passes.
func TestDispositionSilenceIsInert(t *testing.T) {
	s := uiStore(t)
	lane := uiLane(t, "director")
	x := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 2, "A"))
	before := dispositionRowCounts(t, lane, x.ID)
	ctx := t.Context()
	for range 3 {
		if _, err := s.NextTask(ctx, lane, "director"); !errors.Is(err, store.ErrQueueEmpty) {
			t.Fatalf("NextTask err=%v", err)
		}
	}
	if _, err := s.ListTasks(ctx, lane, "", "", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListOpenTaskDecisions(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListOpenDispositions(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListUnnotifiedDispositions(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EventWatermarks(ctx); err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC().Add(30 * 24 * time.Hour)
	summary, err := s.DispositionSummary(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DispositionSummary(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	after := dispositionRowCounts(t, lane, x.ID)
	if after.ItemState != "needs_decision" || after.ItemEvents != before.ItemEvents || after.Backlog != before.Backlog {
		t.Fatalf("silence changed the item: before=%+v after=%+v", before, after)
	}
	if after.Tasks != before.Tasks || after.Relay != before.Relay || after.TaskEvents != before.TaskEvents {
		t.Fatalf("silence wrote rows: before=%+v after=%+v", before, after)
	}
	if summary.Open < 1 || summary.OldestOpenDays < 30 {
		t.Fatalf("30 days of silence must leave the item open and aged: %+v", summary)
	}
}

// AC+1 / M5 M6: every non-operator path is refused while the item is open.
func TestDispositionOnlyOperatorLeavesNeedsDecision(t *testing.T) {
	s := uiStore(t)
	lane := uiLane(t, "director")
	x := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A"))
	before := dispositionRowCounts(t, lane, x.ID)
	for _, to := range []string{"claimed", "backlog", "hold", "dropped"} {
		if _, err := s.TransitionTask(t.Context(), x.ID, to, "director-node", "self-dispose", nil); !errors.Is(err, store.ErrDispositionOperatorOnly) {
			t.Errorf("TransitionTask(%s) err=%v, want disposition_operator_only", to, err)
		}
	}
	svc := api.Service{Store: s}
	if _, err := svc.ResolveDecision(t.Context(), api.DecisionResolveInput{Type: "task", ID: x.ID, By: "operator", Answer: "A: 배포"}); !errors.Is(err, store.ErrDispositionOperatorOnly) {
		t.Errorf("decisions resolve err=%v, want disposition_operator_only", err)
	}
	after := dispositionRowCounts(t, lane, x.ID)
	// Relay rows too: an answer event reaching the director's lane without a
	// recorded answer is exactly the fail-open this guard exists to stop.
	if after.ItemState != "needs_decision" || after.ItemEvents != 2 || after.Relay != before.Relay {
		t.Fatalf("refused paths wrote: before=%+v after=%+v", before, after)
	}
	// The disposition object is not patchable through a transition either.
	answered, err := s.AnswerDisposition(t.Context(), store.DispositionAnswerInput{ID: x.ID, Gen: openGen(t, s, x.ID), Key: "D", OperatorEmail: "admin@example.com", EventID: "web-disposition-test"})
	if err != nil {
		t.Fatal(err)
	}
	forged := answered.Refs
	forged.Disposition.Answer.Key = "A"
	if _, err := s.TransitionTask(t.Context(), x.ID, "hold", "director-node", "", &forged); err == nil {
		t.Fatal("transition patched the disposition answer")
	}
	if _, err := s.ApplyDisposition(t.Context(), x.ID, "director-node", ""); err != nil {
		t.Fatal(err)
	}
	// D -> hold; a disposition item never returns to backlog (NextTask would claim it).
	if _, err := s.TransitionTask(t.Context(), x.ID, "backlog", "director-node", "", nil); !errors.Is(err, store.ErrTaskConflict) {
		t.Fatalf("hold->backlog err=%v, want task_conflict", err)
	}
	// Origins are not re-pointable through a transition patch.
	if _, err := s.TransitionTask(t.Context(), x.ID, "hold", "director-node", "", &store.TaskRefs{OriginTask: x.ID}); err == nil {
		t.Fatal("transition gave a disposition item a second origin")
	}
	// Re-asking (into needs_decision) is allowed and opens a new generation.
	if _, err := s.TransitionTask(t.Context(), x.ID, "needs_decision", "director-node", "re-ask", nil); err != nil {
		t.Fatal(err)
	}
}

func TestDispositionAnswerBindsGenerationAndKey(t *testing.T) {
	s := uiStore(t)
	lane := uiLane(t, "director")
	x := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A"))
	gen := openGen(t, s, x.ID)
	bad := []store.DispositionAnswerInput{
		{ID: x.ID, Gen: gen, Key: "Z", OperatorEmail: "admin@example.com", EventID: "e"},
		{ID: x.ID, Gen: gen, Key: "A", OperatorEmail: "", EventID: "e"},
		{ID: x.ID, Gen: gen, Key: "A", OperatorEmail: "service-name", EventID: "e"},
		{ID: x.ID, Gen: gen, Key: "A", OperatorEmail: "admin@example.com", EventID: ""},
	}
	for _, in := range bad {
		if _, err := s.AnswerDisposition(t.Context(), in); err == nil {
			t.Errorf("answer %+v accepted", in)
		}
	}
	if _, err := s.AnswerDisposition(t.Context(), store.DispositionAnswerInput{ID: x.ID, Gen: gen + 1000, Key: "A", OperatorEmail: "admin@example.com", EventID: "e"}); !errors.Is(err, store.ErrDispositionStale) {
		t.Fatalf("stale generation err=%v", err)
	}
	got, err := s.AnswerDisposition(t.Context(), store.DispositionAnswerInput{ID: x.ID, Gen: gen, Key: "B", OperatorEmail: "admin@example.com", EventID: "web-disposition-x"})
	if err != nil {
		t.Fatal(err)
	}
	a := got.Refs.Disposition.Answer
	if got.State != "claimed" || a == nil || a.Key != "B" || a.By != "operator:admin@example.com" || a.Gen != gen || a.EventID != "web-disposition-x" {
		t.Fatalf("answer = %+v state=%s", a, got.State)
	}
	if _, err := s.AnswerDisposition(t.Context(), store.DispositionAnswerInput{ID: x.ID, Gen: gen, Key: "A", OperatorEmail: "admin@example.com", EventID: "e2"}); !errors.Is(err, store.ErrTaskConflict) {
		t.Fatalf("second answer err=%v, want task_conflict", err)
	}
	full, _, _ := s.GetTask(t.Context(), x.ID)
	last := full.Events[len(full.Events)-1]
	if last.By != "operator:admin@example.com" || last.From != "needs_decision" || last.To != "claimed" {
		t.Fatalf("answer event = %+v", last)
	}
}

// AC+2 / M8 M9 M10: a batch answers exactly its snapshot, in one call.
func TestDispositionBatchIsScopedToSnapshot(t *testing.T) {
	s := uiStore(t)
	lane := uiLane(t, "director")
	a := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A"))
	c := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 1, "C"))
	stale := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "E"))
	snapshot := []store.DispositionRef{{ID: a.ID, Gen: openGen(t, s, a.ID)}, {ID: c.ID, Gen: openGen(t, s, c.ID)}, {ID: stale.ID, Gen: openGen(t, s, stale.ID)}}
	// After the snapshot: the stale item is answered D, applied to hold, and
	// re-asked (new generation); a new item appears.
	if _, err := s.AnswerDisposition(t.Context(), store.DispositionAnswerInput{ID: stale.ID, Gen: snapshot[2].Gen, Key: "D", OperatorEmail: "admin@example.com", EventID: "e-stale"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDisposition(t.Context(), stale.ID, "director-node", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), stale.ID, "needs_decision", "director-node", "re-ask", nil); err != nil {
		t.Fatal(err)
	}
	late := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A"))
	answered, skipped, err := s.AnswerDispositionBatch(t.Context(), snapshot, "admin@example.com", "batch0001", func(l string) string { return "web-disposition-batch-batch0001-" + l })
	if err != nil {
		t.Fatal(err)
	}
	if len(answered) != 2 || len(skipped) != 1 || skipped[0].ID != stale.ID || skipped[0].Reason != "stale" {
		t.Fatalf("answered=%d skipped=%+v", len(answered), skipped)
	}
	for _, x := range answered {
		ans := x.Refs.Disposition.Answer
		if ans.Key != x.Refs.Disposition.Recommended || ans.BatchID != "batch0001" || x.State != "claimed" {
			t.Fatalf("batch answer = %+v", ans)
		}
	}
	for _, id := range []int64{late.ID, stale.ID} {
		if got := dispositionRowCounts(t, lane, id); got.ItemState != "needs_decision" {
			t.Fatalf("#%d outside the snapshot changed to %s", id, got.ItemState)
		}
	}
	// Replay: nothing left to answer.
	again, skippedAgain, err := s.AnswerDispositionBatch(t.Context(), snapshot[:2], "admin@example.com", "batch0001", func(l string) string { return "x" })
	if err != nil || len(again) != 0 || len(skippedAgain) != 2 {
		t.Fatalf("replay answered=%d skipped=%d err=%v", len(again), len(skippedAgain), err)
	}
	if _, _, err := s.AnswerDispositionBatch(t.Context(), make([]store.DispositionRef, store.DispositionBatchLimit+1), "admin@example.com", "b", func(string) string { return "x" }); err == nil {
		t.Fatal("batch beyond the limit accepted")
	}
}

func TestDispositionApplyUsesLegalEdges(t *testing.T) {
	s := uiStore(t)
	lane := uiLane(t, "director")
	want := map[string]string{"A": "merged", "B": "merged", "C": "merged", "D": "hold", "E": "dropped"}
	for key, final := range want {
		x := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "E"))
		if _, err := s.ApplyDisposition(t.Context(), x.ID, "director-node", ""); !errors.Is(err, store.ErrTaskConflict) {
			t.Fatalf("apply before answer err=%v", err)
		}
		if _, err := s.AnswerDisposition(t.Context(), store.DispositionAnswerInput{ID: x.ID, Gen: openGen(t, s, x.ID), Key: key, OperatorEmail: "admin@example.com", EventID: "e"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.ApplyDisposition(t.Context(), x.ID, "director-node", "applied")
		if err != nil || got.State != final {
			t.Fatalf("apply %s: state=%s err=%v", key, got.State, err)
		}
		full, _, _ := s.GetTask(t.Context(), x.ID)
		for _, e := range full.Events {
			if !store.TaskTransitions[e.From][e.To] {
				t.Fatalf("illegal recorded edge %s->%s", e.From, e.To)
			}
		}
	}
}

// AC ③ / AC+3 / AC+4 / M12: one definition, lane-independent, replayable.
func TestDispositionSummaryDefinition(t *testing.T) {
	s := uiStore(t)
	ctx := t.Context()
	laneA, laneB := uiLane(t, "director"), uiLane(t, "other-director")
	drainOpenDispositions(t, s)
	t.Cleanup(func() { drainOpenDispositions(t, s) })
	// The oldest open item is one the director re-asked: its age still counts
	// from creation, not from the re-ask event.
	reasked := mustCreateDisposition(t, s, dispositionInput(laneB, dispositionPR(t), 0, "D"))
	if _, err := s.AnswerDisposition(ctx, store.DispositionAnswerInput{ID: reasked.ID, Gen: openGen(t, s, reasked.ID), Key: "D", OperatorEmail: "admin@example.com", EventID: "e-r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDisposition(ctx, reasked.ID, "director-node", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := s.TransitionTask(ctx, reasked.ID, "needs_decision", "director-node", "re-ask with new facts", nil); err != nil {
		t.Fatal(err)
	}
	base, err := s.DispositionSummary(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if base.Open != 1 || base.OldestOpenID != reasked.ID || base.OldestOpenSince == nil || !base.OldestOpenSince.Equal(reasked.CreatedAt) {
		t.Fatalf("oldest open must be the re-asked item aged from creation: %+v created=%s", base, reasked.CreatedAt)
	}
	openA := mustCreateDisposition(t, s, dispositionInput(laneA, dispositionPR(t), 0, "A"))
	mustCreateDisposition(t, s, dispositionInput(laneB, dispositionPR(t), 0, "C"))
	pending := mustCreateDisposition(t, s, dispositionInput(laneA, dispositionPR(t), 0, "B"))
	held := mustCreateDisposition(t, s, dispositionInput(laneB, dispositionPR(t), 0, "D"))
	beforeAnswers := time.Now().UTC()
	time.Sleep(10 * time.Millisecond)
	if _, err := s.AnswerDisposition(ctx, store.DispositionAnswerInput{ID: pending.ID, Gen: openGen(t, s, pending.ID), Key: "B", OperatorEmail: "admin@example.com", EventID: "e-p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerDisposition(ctx, store.DispositionAnswerInput{ID: held.ID, Gen: openGen(t, s, held.ID), Key: "D", OperatorEmail: "admin@example.com", EventID: "e-h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDisposition(ctx, held.ID, "director-node", ""); err != nil {
		t.Fatal(err)
	}
	// A merged PR without an item is a coverage candidate.
	merged := createUITask(t, s, laneA, "merged work")
	if _, err := s.ClaimTask(ctx, merged.ID, "b"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"in_progress", "join"} {
		if _, err := s.TransitionTask(ctx, merged.ID, to, "b", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.TransitionTask(ctx, merged.ID, "merged", "b", "", &store.TaskRefs{PR: dispositionPR(t)}); err != nil {
		t.Fatal(err)
	}
	now, err := s.DispositionSummary(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if now.Open-base.Open != 2 || now.PendingApply-base.PendingApply != 1 || now.Held-base.Held != 1 {
		t.Fatalf("counts across two lanes: base=%+v now=%+v", base, now)
	}
	if !now.CoverageSupported || now.MergedWithoutItem-base.MergedWithoutItem != 1 {
		t.Fatalf("coverage candidates: base=%d now=%d supported=%v", base.MergedWithoutItem, now.MergedWithoutItem, now.CoverageSupported)
	}
	if now.SingleAnswers24h-base.SingleAnswers24h != 2 {
		t.Fatalf("single answers: base=%d now=%d", base.SingleAnswers24h, now.SingleAnswers24h)
	}
	if !strings.HasPrefix(now.Line, fmt.Sprintf("미처분 %d · 최고령 ", now.Open)) {
		t.Fatalf("line=%q", now.Line)
	}
	// Replay at an earlier instant: both answered items were still open then.
	past, err := s.DispositionSummary(ctx, beforeAnswers)
	if err != nil {
		t.Fatal(err)
	}
	if past.Open-now.Open != 2 || past.PendingApply != now.PendingApply-1 || past.Held != now.Held-1 {
		t.Fatalf("as-of replay: past=%+v now=%+v", past, now)
	}
	// Age is measured from creation, not from updated_at.
	later, err := s.DispositionSummary(ctx, openA.CreatedAt.Add(5*24*time.Hour+time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if later.OldestOpenDays < 5 {
		t.Fatalf("oldest open days=%d, want >=5", later.OldestOpenDays)
	}
}

// ESC-2 / C3: beyond the batch limit the summary names the next batch.
func TestDispositionSummaryNamesNextBatch(t *testing.T) {
	s := uiStore(t)
	lane := uiLane(t, "director")
	t.Cleanup(func() { drainOpenDispositions(t, s) })
	base, err := s.DispositionSummary(t.Context(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	need := store.DispositionBatchLimit + 2 - base.Open
	for i := 0; i < need; i++ {
		mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "E"))
	}
	got, err := s.DispositionSummary(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.NextBatch != got.Open-store.DispositionBatchLimit || got.NextBatch < 2 || !strings.Contains(got.Line, fmt.Sprintf("다음 묶음 %d건", got.NextBatch)) {
		t.Fatalf("next batch: %+v", got)
	}
}

// The bearer API creates items and reads the summary; it cannot answer them.
func TestDispositionBearerAPIRoutes(t *testing.T) {
	s := uiStore(t)
	server := httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"director-node": "node-token"}}.Handler())
	defer server.Close()
	call := func(method, path string, payload any) (int, map[string]any) {
		t.Helper()
		var reader io.Reader
		if payload != nil {
			raw, _ := json.Marshal(payload)
			reader = bytes.NewReader(raw)
		}
		req, _ := http.NewRequestWithContext(t.Context(), method, server.URL+path, reader)
		req.Header.Set("Authorization", "Bearer node-token")
		req.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(response.Body).Decode(&out)
		return response.StatusCode, out
	}
	lane := uiLane(t, "director")
	in := dispositionInput(lane, dispositionPR(t), 0, "E")
	status, out := call(http.MethodPost, "/v1/tasks/dispositions", in)
	if status != http.StatusCreated || out["created"] != true {
		t.Fatalf("create status=%d out=%v", status, out)
	}
	task := out["task"].(map[string]any)
	id := int64(task["id"].(float64))
	if task["created_by"] != "director-node" || task["state"] != "needs_decision" {
		t.Fatalf("created task=%v", task)
	}
	if status, out = call(http.MethodPost, "/v1/tasks/dispositions", in); status != http.StatusOK || out["created"] != false {
		t.Fatalf("idempotent create status=%d out=%v", status, out)
	}
	if status, out = call(http.MethodGet, "/v1/tasks/dispositions/summary?as_of="+url.QueryEscape(time.Now().UTC().Format(time.RFC3339)), nil); status != http.StatusOK || !strings.HasPrefix(out["line"].(string), "미처분 ") {
		t.Fatalf("summary status=%d out=%v", status, out)
	}
	if status, _ = call(http.MethodGet, "/v1/tasks/dispositions/summary?as_of=yesterday", nil); status != http.StatusBadRequest {
		t.Fatalf("bad as_of status=%d", status)
	}
	if status, out = call(http.MethodPost, fmt.Sprintf("/v1/tasks/dispositions/%d/apply", id), map[string]string{}); status != http.StatusConflict {
		t.Fatalf("apply before answer status=%d out=%v", status, out)
	}
	drainOpenDispositions(t, s)
}

// D2 (operator 2026-09-21): the guard covers disposition items only. Builder
// questions keep every existing path: generic transition, decisions resolve,
// and the generic decision inbox.
func TestDispositionGuardLeavesOtherDecisionsUnchanged(t *testing.T) {
	s := uiStore(t)
	lane := uiLane(t, "builder")
	mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A"))
	t.Cleanup(func() { drainOpenDispositions(t, s) })
	ask := func(title string) store.Task {
		t.Helper()
		return claimAndTransition(t, s, createUITask(t, s, lane, title), "needs_decision", "which interface?")
	}
	viaTransition := ask("builder question via transition")
	viaResolve := ask("builder question via resolve")
	open, err := s.ListOpenTaskDecisions(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, d := range open {
		if d.Task.Refs.Disposition != nil {
			t.Fatalf("disposition item #%d listed as a generic decision", d.Task.ID)
		}
		seen[d.Task.ID] = true
	}
	if !seen[viaTransition.ID] || !seen[viaResolve.ID] {
		t.Fatalf("builder questions missing from the generic inbox: %v", seen)
	}
	if got, err := s.TransitionTask(t.Context(), viaTransition.ID, "claimed", "director-node", "answered in chat", nil); err != nil || got.State != "claimed" {
		t.Fatalf("generic transition of a builder question: state=%s err=%v", got.State, err)
	}
	svc := api.Service{Store: s}
	if _, err := svc.ResolveDecision(t.Context(), api.DecisionResolveInput{Type: "task", ID: viaResolve.ID, By: "operator-desk", Answer: "A", NoInject: true}); err != nil {
		t.Fatalf("decisions resolve of a builder question: %v", err)
	}
	if got, _, _ := s.GetTask(t.Context(), viaResolve.ID); got.State != "claimed" {
		t.Fatalf("resolved builder question state=%s", got.State)
	}
	// A task that merely carries origin refs is not a disposition item.
	child, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: "ordered follow-up", Kind: "implement", CreatedBy: "n", Refs: store.TaskRefs{OriginTask: viaResolve.ID}})
	if err != nil || child.Refs.OriginTask != viaResolve.ID {
		t.Fatalf("follow-up with origin_task: %+v err=%v", child, err)
	}
	child = claimAndTransition(t, s, child, "needs_decision", "q")
	if _, err := s.TransitionTask(t.Context(), child.ID, "backlog", "director-node", "", nil); err != nil {
		t.Fatalf("origin refs must not trigger the disposition guard: %v", err)
	}
}

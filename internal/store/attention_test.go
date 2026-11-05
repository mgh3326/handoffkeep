package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Every test reads whole tables, so each runs in a throwaway schema.
func attentionStore(t *testing.T) *Store { return schemaStore(t, "attention") }

var attentionNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

type attentionRequest struct {
	ref                  string
	created              time.Time
	due, snooze, expires *time.Time
	status               string
}

func at(d time.Duration) *time.Time {
	x := attentionNow.Add(d)
	return &x
}

func insertAttentionRequest(t *testing.T, s *Store, r attentionRequest) int64 {
	t.Helper()
	if r.status == "" {
		r.status = DecisionStatusOpen
	}
	if r.created.IsZero() {
		r.created = attentionNow.Add(-time.Hour)
	}
	var id int64
	if err := s.pool.QueryRow(t.Context(), `INSERT INTO decision_requests(source,source_ref,lane,question,due_at,snoozed_until,expires_at,status,created_at,updated_at)
		VALUES('chat',$1,'lane-'||$1,'question '||$1,$2,$3,$4,$5,$6,$6) RETURNING id`,
		r.ref, r.due, r.snooze, r.expires, r.status, r.created).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type attentionTaskSeed struct {
	title    string
	state    string
	awaiting bool
	refs     string
	created  time.Time
}

// insertAttentionTask writes a task row with no events, so its activity is
// its created_at.
func insertAttentionTask(t *testing.T, s *Store, x attentionTaskSeed) int64 {
	t.Helper()
	if x.refs == "" {
		x.refs = "{}"
	}
	if x.state == "" {
		x.state = "developing"
	}
	if x.created.IsZero() {
		x.created = attentionNow.Add(-time.Hour)
	}
	var id int64
	if err := s.pool.QueryRow(t.Context(), `INSERT INTO tasks(lane,title,kind,state,awaiting_decision,refs,created_by,created_at,updated_at)
		VALUES('attn-lane',$1,'implement',$2,$3,$4::jsonb,'test',$5,$5) RETURNING id`,
		x.title, x.state, x.awaiting, x.refs, x.created).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func attentionOf(t *testing.T, s *Store) Attention {
	t.Helper()
	got, err := s.Attention(t.Context(), attentionNow)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func decisionIDs(items []AttentionDecision) []string {
	out := []string{}
	for _, x := range items {
		out = append(out, fmt.Sprintf("%s:%d", x.Source, x.ID))
	}
	return out
}

func TestAttentionEmpty(t *testing.T) {
	got := attentionOf(t, attentionStore(t))
	if got.Decisions.Count != 0 || got.Tasks.Count != 0 || got.Decisions.Items == nil || got.Tasks.Items == nil || len(got.Decisions.Items)+len(got.Tasks.Items) != 0 {
		t.Fatalf("empty store: %+v", got)
	}
}

func TestAttentionRequestsCountedAndBounded(t *testing.T) {
	s := attentionStore(t)
	ids := []int64{}
	for i := 0; i < 12; i++ {
		ids = append(ids, insertAttentionRequest(t, s, attentionRequest{ref: fmt.Sprintf("r%02d", i), created: attentionNow.Add(-time.Duration(100-i) * time.Minute)}))
	}
	// The newest request is the only overdue one, so it ranks ahead of ten
	// older requests that have no due date.
	overdue := insertAttentionRequest(t, s, attentionRequest{ref: "overdue", created: attentionNow.Add(-time.Minute), due: at(-time.Minute)})
	got := attentionOf(t, s).Decisions
	if got.Count != 13 || len(got.Items) != AttentionItems {
		t.Fatalf("count=%d items=%d, want 13 and %d", got.Count, len(got.Items), AttentionItems)
	}
	first := got.Items[0]
	if first.ID != overdue || !first.Overdue || first.Source != AttentionSourceRequest || first.DueAt == nil {
		t.Fatalf("first=%+v, want the overdue request %d", first, overdue)
	}
	// The rest are the oldest, in creation order.
	for i, x := range got.Items[1:] {
		if x.ID != ids[i] || x.Overdue {
			t.Fatalf("item %d = %+v, want request %d", i+1, x, ids[i])
		}
	}
}

func TestAttentionRequestOrder(t *testing.T) {
	s := attentionStore(t)
	noDue := insertAttentionRequest(t, s, attentionRequest{ref: "nodue", created: attentionNow.Add(-3 * time.Hour)})
	soon := insertAttentionRequest(t, s, attentionRequest{ref: "soon", created: attentionNow.Add(-time.Hour), due: at(time.Hour)})
	later := insertAttentionRequest(t, s, attentionRequest{ref: "later", created: attentionNow.Add(-2 * time.Hour), due: at(5 * time.Hour)})
	lateOverdue := insertAttentionRequest(t, s, attentionRequest{ref: "late", created: attentionNow.Add(-time.Minute), due: at(-time.Minute)})
	earlyOverdue := insertAttentionRequest(t, s, attentionRequest{ref: "early", created: attentionNow.Add(-time.Minute), due: at(-time.Hour)})
	got := attentionOf(t, s).Decisions
	want := []int64{earlyOverdue, lateOverdue, soon, later, noDue}
	if len(got.Items) != len(want) {
		t.Fatalf("items=%v", decisionIDs(got.Items))
	}
	for i, id := range want {
		if got.Items[i].ID != id {
			t.Fatalf("order=%v, want ids %v", decisionIDs(got.Items), want)
		}
	}
}

func TestAttentionEqualKeysComeBackInIDOrder(t *testing.T) {
	s := attentionStore(t)
	created := attentionNow.Add(-time.Hour)
	var want []int64
	for i := 0; i < 4; i++ {
		want = append(want, insertAttentionRequest(t, s, attentionRequest{ref: fmt.Sprintf("same%d", i), created: created, due: at(time.Hour)}))
	}
	got := attentionOf(t, s).Decisions.Items
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("order=%v, want ids %v", decisionIDs(got), want)
		}
	}
	// Two tasks idle since the same moment: id order again.
	a := insertAttentionTask(t, s, attentionTaskSeed{title: "a", awaiting: true, created: created})
	b := insertAttentionTask(t, s, attentionTaskSeed{title: "b", awaiting: true, created: created})
	tasks := attentionOf(t, s).Tasks.Items
	if len(tasks) != 2 || tasks[0].ID != a || tasks[1].ID != b {
		t.Fatalf("tasks=%+v, want %d then %d", tasks, a, b)
	}
}

func TestAttentionSnoozedAndExpiredRequestsExcluded(t *testing.T) {
	s := attentionStore(t)
	open := insertAttentionRequest(t, s, attentionRequest{ref: "open"})
	insertAttentionRequest(t, s, attentionRequest{ref: "snoozed", snooze: at(time.Hour)})
	insertAttentionRequest(t, s, attentionRequest{ref: "expired", expires: at(-time.Minute)})
	insertAttentionRequest(t, s, attentionRequest{ref: "answered", status: DecisionStatusAnswered})
	// A snooze that has ended and an expiry still ahead leave the request open.
	woke := insertAttentionRequest(t, s, attentionRequest{ref: "woke", snooze: at(-time.Minute), expires: at(time.Hour)})
	got := attentionOf(t, s).Decisions
	if got.Count != 2 || len(got.Items) != 2 {
		t.Fatalf("got %v (count %d), want only %d and %d", decisionIDs(got.Items), got.Count, open, woke)
	}
	if ids := decisionIDs(got.Items); ids[0] != fmt.Sprintf("request:%d", open) || ids[1] != fmt.Sprintf("request:%d", woke) {
		t.Fatalf("items=%v", ids)
	}
}

func TestAttentionLaneEventDualWriteCountedOnce(t *testing.T) {
	s := attentionStore(t)
	// Recording a [decision-needed] event also opens its request.
	dual, _ := decisionRelay(t, s, RelayEvent{Kind: "lane.event", OwnerLane: "dual-lane", EventID: "dual-1", Text: "[decision-needed] ship it?"})
	got := attentionOf(t, s).Decisions
	if got.Count != 1 || len(got.Items) != 1 || got.Items[0].Source != AttentionSourceRequest {
		t.Fatalf("dual write: count=%d items=%v, want one request", got.Count, decisionIDs(got.Items))
	}
	if got.Items[0].Lane != "dual-lane" || !strings.Contains(got.Items[0].Question, "ship it?") {
		t.Fatalf("item=%+v", got.Items[0])
	}

	// A snoozed request keeps hiding its event.
	if _, err := s.pool.Exec(t.Context(), `UPDATE decision_requests SET snoozed_until=$1 WHERE source='lane_event' AND source_ref=$2`, attentionNow.Add(time.Hour), fmt.Sprint(dual.ID)); err != nil {
		t.Fatal(err)
	}
	if got = attentionOf(t, s).Decisions; got.Count != 0 {
		t.Fatalf("snoozed dual write: count=%d items=%v", got.Count, decisionIDs(got.Items))
	}
}

func TestAttentionLaneEventWithoutRequestCounted(t *testing.T) {
	s := attentionStore(t)
	lone, _ := decisionRelay(t, s, RelayEvent{Kind: "lane.event", OwnerLane: "lone-lane", EventID: "lone-1", Text: "[decision-needed]   which db?"})
	if _, err := s.pool.Exec(t.Context(), `DELETE FROM decision_requests WHERE source='lane_event' AND source_ref=$1`, fmt.Sprint(lone.ID)); err != nil {
		t.Fatal(err)
	}
	// An answered event is not waiting.
	answered, _ := decisionRelay(t, s, RelayEvent{Kind: "lane.event", OwnerLane: "lone-lane", EventID: "lone-2", Text: "[decision-needed] and this?"})
	decisionRelay(t, s, RelayEvent{Kind: "lane.event", OwnerLane: "lone-lane", EventID: "lone-3", Text: fmt.Sprintf("[decision-answered] #%d: yes", answered.ID)})
	got := attentionOf(t, s).Decisions
	if got.Count != 1 || len(got.Items) != 1 {
		t.Fatalf("count=%d items=%v, want the one unanswered event", got.Count, decisionIDs(got.Items))
	}
	x := got.Items[0]
	if x.Source != AttentionSourceLaneEvent || x.ID != lone.ID || x.Lane != "lone-lane" || x.Question != "which db?" || x.DueAt != nil || x.Overdue {
		t.Fatalf("item=%+v", x)
	}
	// Open lane events and requests are one list.
	req := insertAttentionRequest(t, s, attentionRequest{ref: "later-request", created: time.Now().UTC().Add(time.Hour)})
	got = attentionOf(t, s).Decisions
	if got.Count != 2 || got.Items[0].ID != lone.ID || got.Items[1].ID != req {
		t.Fatalf("merged: count=%d items=%v", got.Count, decisionIDs(got.Items))
	}
}

func TestAttentionTasksWaiting(t *testing.T) {
	s := attentionStore(t)
	old := attentionNow.Add(-72 * time.Hour)
	awaiting := insertAttentionTask(t, s, attentionTaskSeed{title: "awaiting", state: "developing", awaiting: true, created: attentionNow.Add(-2 * time.Hour)})
	// Awaiting with an open structured request: that request is the decision.
	insertAttentionTask(t, s, attentionTaskSeed{title: "has request", awaiting: true, refs: `{"decision_request":{"status":"open"}}`})
	// A disposition is answered elsewhere.
	insertAttentionTask(t, s, attentionTaskSeed{title: "disposition", awaiting: true, refs: `{"disposition":{}}`})
	// A request that is no longer open does not hide the task.
	answeredReq := insertAttentionTask(t, s, attentionTaskSeed{title: "answered request", awaiting: true, refs: `{"decision_request":{"status":"answered"}}`, created: attentionNow.Add(-3 * time.Hour)})
	stalled := insertAttentionTask(t, s, attentionTaskSeed{title: "stalled", state: "reviewing", created: old})
	insertAttentionTask(t, s, attentionTaskSeed{title: "fresh", state: "developing", created: attentionNow.Add(-time.Hour)})
	// Idle but not started: backlog work is not stalled.
	insertAttentionTask(t, s, attentionTaskSeed{title: "backlog", state: "backlog", created: old})

	got := attentionOf(t, s).Tasks
	if got.AwaitingDecision != 2 || got.Stalled != 1 || got.Count != 3 {
		t.Fatalf("counts=%+v, want awaiting 2, stalled 1, count 3", got)
	}
	// Awaiting first, oldest activity first within each reason.
	want := []struct {
		id     int64
		reason string
	}{{answeredReq, AttentionReasonAwaitingDecision}, {awaiting, AttentionReasonAwaitingDecision}, {stalled, AttentionReasonStalled}}
	if len(got.Items) != len(want) {
		t.Fatalf("items=%+v", got.Items)
	}
	for i, w := range want {
		if got.Items[i].ID != w.id || got.Items[i].Reason != w.reason {
			t.Fatalf("item %d = %+v, want %d %s", i, got.Items[i], w.id, w.reason)
		}
	}
	if x := got.Items[2]; x.Title != "stalled" || x.Lane != "attn-lane" || x.State != "reviewing" || !x.Since.Equal(old) {
		t.Fatalf("stalled item=%+v", x)
	}
}

// The stalled count follows the store's started_stalled_hours setting and its
// activity rule: a comment is activity.
func TestAttentionStalledFollowsSettingAndActivity(t *testing.T) {
	s := attentionStore(t)
	idle := insertAttentionTask(t, s, attentionTaskSeed{title: "idle", created: attentionNow.Add(-30 * time.Hour)})
	commented := insertAttentionTask(t, s, attentionTaskSeed{title: "commented", created: attentionNow.Add(-30 * time.Hour)})
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO task_comments(task_id,author,body,created_at) VALUES($1,'test','hi',$2)`, commented, attentionNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := attentionOf(t, s).Tasks
	if got.Stalled != 1 || len(got.Items) != 1 || got.Items[0].ID != idle {
		t.Fatalf("24h: %+v", got)
	}
	if err := s.SetStartedStalledHours(48); err != nil {
		t.Fatal(err)
	}
	if got = attentionOf(t, s).Tasks; got.Stalled != 0 || got.Count != 0 {
		t.Fatalf("48h: %+v", got)
	}
}

// ListOpenTaskDecisions is the inbox's own list of the same tasks: the
// awaiting count here must equal it.
func TestAttentionAwaitingMatchesInbox(t *testing.T) {
	s := attentionStore(t)
	insertAttentionTask(t, s, attentionTaskSeed{title: "one", awaiting: true})
	insertAttentionTask(t, s, attentionTaskSeed{title: "two", awaiting: true, refs: `{"decision_request":{"status":"open"}}`})
	insertAttentionTask(t, s, attentionTaskSeed{title: "three", awaiting: true, refs: `{"disposition":{}}`})
	inbox, err := s.ListOpenTaskDecisions(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := attentionOf(t, s).Tasks.AwaitingDecision; got != len(inbox) || got != 1 {
		t.Fatalf("awaiting=%d inbox=%d, want 1 and 1", got, len(inbox))
	}
}

func TestAttentionTaskItemsBounded(t *testing.T) {
	s := attentionStore(t)
	for i := 0; i < 13; i++ {
		insertAttentionTask(t, s, attentionTaskSeed{title: fmt.Sprintf("t%d", i), awaiting: true, created: attentionNow.Add(-time.Duration(i+1) * time.Hour)})
	}
	got := attentionOf(t, s).Tasks
	if got.Count != 13 || len(got.Items) != AttentionItems {
		t.Fatalf("count=%d items=%d", got.Count, len(got.Items))
	}
}

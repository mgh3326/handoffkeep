package tests

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// #618: one decision-request record, three console readers. These tests seed
// requests in every display state and check that the queue list refs, the
// drawer detail and the Decisions page report the same open requests and the
// same pending/uncleaned split.

func drInput(question string) store.DecisionRequestInput {
	return store.DecisionRequestInput{
		Question: question,
		Options: store.DecisionOptions{AllowFree: true, Options: []store.DecisionOption{
			{Key: "A", Label: "자문 먼저", Recommended: true},
			{Key: "B", Label: "#79 먼저"},
			{Key: "C", Label: "중단"},
		}},
		DefaultAction: "보류하고 다음 태스크",
	}
}

func drTask(t *testing.T, s *store.Store, lane string, path ...string) store.Task {
	t.Helper()
	x := newTask(t, s, lane, "decision view task", 0)
	var err error
	for _, to := range path {
		if to == "claimed" {
			x, err = s.ClaimTask(t.Context(), x.ID, "dr-test", "job-1", "")
		} else {
			x, err = s.TransitionTask(t.Context(), x.ID, to, "dr-test", "step", nil, "")
		}
		if err != nil {
			t.Fatalf("to %s: %v", to, err)
		}
	}
	return x
}

func drRecord(t *testing.T, s *store.Store, id int64, in store.DecisionRequestInput) store.DecisionRequest {
	t.Helper()
	got, err := s.RecordDecisionRequest(t.Context(), id, "director-1", in)
	if err != nil {
		t.Fatal(err)
	}
	return got.Request
}

// allBoardTasks pages the complete board list, as the queue does.
func allBoardTasks(t *testing.T, client *http.Client, base, assertion string) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	after := ""
	for {
		var page struct {
			Tasks       []map[string]any `json:"tasks"`
			NextAfterID int64            `json:"next_after_id"`
			Truncated   bool             `json:"truncated"`
		}
		url := base + "/ui/api/board/tasks?limit=500"
		if after != "" {
			url += "&after_id=" + after
		}
		if status := boardJSON(t, client, url, assertion, &page); status != http.StatusOK {
			t.Fatalf("board list status=%d", status)
		}
		out = append(out, page.Tasks...)
		if !page.Truncated {
			return out
		}
		after = strconv.FormatInt(page.NextAfterID, 10)
	}
}

// listMark applies the queue's rule (adapter.ts decisionMark) to a list row.
func listMark(task map[string]any) string {
	refs, _ := task["refs"].(map[string]any)
	request, _ := refs["decision_request"].(map[string]any)
	if request == nil || request["status"] != "open" {
		return ""
	}
	if task["state"] == "merged" || task["state"] == "dropped" {
		return "uncleaned"
	}
	return "pending"
}

func TestUIDecisionRequestViewsAgree(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "b618")

	backlog := drTask(t, s, lane)
	drRecord(t, s, backlog.ID, drInput("backlog 요청"))
	overdue := drTask(t, s, lane, "claimed", "in_progress")
	late := drInput("기한 지난 요청")
	past := time.Now().Add(-2 * time.Hour)
	late.DueAt = &past
	drRecord(t, s, overdue.ID, late)
	merged := drTask(t, s, lane, "claimed", "in_progress")
	drRecord(t, s, merged.ID, drInput("머지 전 요청"))
	for _, to := range []string{"verifying", "merged"} {
		if _, err := s.TransitionTask(t.Context(), merged.ID, to, "dr-test", "step", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	answered := drTask(t, s, lane, "claimed", "in_progress")
	answeredReq := drRecord(t, s, answered.ID, drInput("답한 요청"))
	if _, err := s.ResolveDecisionRequest(t.Context(), answered.ID, "director-1", store.DecisionResolveInput{RequestID: answeredReq.ID, Kind: store.DecisionRequestAnswered, Option: "A", Responder: "operator"}); err != nil {
		t.Fatal(err)
	}
	blocked := drTask(t, s, lane, "claimed", "in_progress")
	block := drInput("막는 요청")
	block.Block = true
	drRecord(t, s, blocked.ID, block)
	superseded := drTask(t, s, lane, "claimed")
	first := drRecord(t, s, superseded.ID, drInput("첫 요청"))
	second := drInput("바뀐 요청")
	second.Supersedes = first.ID
	second.Options.Options = []store.DecisionOption{{Key: "A", Label: "새 A"}, {Key: "B", Label: "새 B", Recommended: true}}
	secondReq := drRecord(t, s, superseded.ID, second)

	wantState := map[int64]string{
		backlog.ID: "open", overdue.ID: "overdue", merged.ID: "uncleaned",
		answered.ID: "answered", blocked.ID: "open", superseded.ID: "open",
	}

	// Queue: list refs, the whole dataset and this lane.
	tasks := allBoardTasks(t, h.Client(), h.URL, assertion)
	listPending, listUncleaned := 0, 0
	laneMarks := map[int64]string{}
	for _, task := range tasks {
		mark := listMark(task)
		switch mark {
		case "pending":
			listPending++
		case "uncleaned":
			listUncleaned++
		}
		if task["lane"] == lane {
			laneMarks[int64(task["id"].(float64))] = mark
		}
	}
	wantMarks := map[int64]string{backlog.ID: "pending", overdue.ID: "pending", merged.ID: "uncleaned", answered.ID: "", blocked.ID: "pending", superseded.ID: "pending"}
	for id, want := range wantMarks {
		if laneMarks[id] != want {
			t.Fatalf("queue mark #%d=%q want %q", id, laneMarks[id], want)
		}
	}

	// Drawer: the detail's current request state for every seeded task, and
	// the lane's open split equals the queue marks.
	drawerPending, drawerUncleaned := 0, 0
	for id, want := range wantState {
		var detail struct {
			DecisionRequests []struct {
				ID           string `json:"id"`
				Current      bool   `json:"current"`
				State        string `json:"state"`
				SupersededBy string `json:"superseded_by"`
				Resolution   *struct {
					Option string `json:"option"`
				} `json:"resolution"`
				Options []store.DecisionOption `json:"options"`
			} `json:"decision_requests"`
		}
		if status := boardJSON(t, h.Client(), h.URL+"/ui/api/board/tasks/"+strconv.FormatInt(id, 10), assertion, &detail); status != http.StatusOK {
			t.Fatalf("detail #%d status=%d", id, status)
		}
		if len(detail.DecisionRequests) == 0 || !detail.DecisionRequests[0].Current || detail.DecisionRequests[0].State != want {
			t.Fatalf("drawer #%d=%+v want %s", id, detail.DecisionRequests, want)
		}
		switch detail.DecisionRequests[0].State {
		case "open", "overdue":
			drawerPending++
		case "uncleaned":
			drawerUncleaned++
		}
		if id == superseded.ID {
			if len(detail.DecisionRequests) != 2 || detail.DecisionRequests[1].ID != first.ID || detail.DecisionRequests[1].State != "superseded" ||
				detail.DecisionRequests[1].SupersededBy != secondReq.ID || detail.DecisionRequests[0].Resolution != nil || detail.DecisionRequests[1].Options[0].Label != "자문 먼저" {
				t.Fatalf("superseded history=%+v", detail.DecisionRequests)
			}
		}
	}
	lanePending, laneUncleaned := 0, 0
	for _, mark := range laneMarks {
		if mark == "pending" {
			lanePending++
		} else if mark == "uncleaned" {
			laneUncleaned++
		}
	}
	if drawerPending != lanePending || drawerUncleaned != laneUncleaned || lanePending != 4 || laneUncleaned != 1 {
		t.Fatalf("drawer %d/%d, queue %d/%d, want 4/1", drawerPending, drawerUncleaned, lanePending, laneUncleaned)
	}

	// Detail of a task that never had a request says so explicitly (empty
	// list, not absent), and a needs_decision task without one is 미기록.
	plain := drTask(t, s, lane, "claimed", "in_progress")
	if _, err := s.TransitionTask(t.Context(), plain.ID, "needs_decision", "dr-test", "기록 없는 질문", nil, ""); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/board/tasks/"+strconv.FormatInt(plain.ID, 10), assertion, &raw); status != http.StatusOK {
		t.Fatalf("detail status=%d", status)
	}
	if string(raw["decision_requests"]) != "[]" || !strings.Contains(string(raw["decision_legacy"]), `"state":"unrecorded"`) || !strings.Contains(string(raw["decision_legacy"]), "기록 없는 질문") {
		t.Fatalf("legacy detail=%s / %s", raw["decision_requests"], raw["decision_legacy"])
	}
}

// The legacy answer paths refuse a task whose structured request is open:
// answering there would unblock the task and leave the request open.
func TestDecisionRequestLegacyAnswerPathsRefuseOpenRequest(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	x := drTask(t, s, lane, "claimed", "in_progress")
	in := drInput("막는 요청")
	in.Block = true
	request := drRecord(t, s, x.ID, in)
	client := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}
	if _, err := client.ResolveDecision(t.Context(), "task", x.ID, "director-1", "A", "", true, ""); err == nil || err.Error() != "decision_request_open" {
		t.Fatalf("decisions resolve err=%v", err)
	}
	got, _, _ := s.GetTask(t.Context(), x.ID)
	if got.State != "developing" || !got.AwaitingDecision || got.Refs.DecisionRequest.ID != request.ID || got.Refs.DecisionRequest.Status != "open" {
		t.Fatalf("task changed: %s awaiting=%v %+v", got.State, got.AwaitingDecision, got.Refs.DecisionRequest)
	}
}

// HTTP: 201 on record, 200 with the same request_id on a re-send, the
// documented conflicts, and a validation reason the CLI can print.
func TestDecisionRequestAPI(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	x := drTask(t, s, taskLane(t), "claimed")
	url := h.URL + "/v1/tasks/" + strconv.FormatInt(x.ID, 10) + "/decision-request"
	post := func(body any, token string) (int, map[string]any) {
		response := request(t, h.Client(), http.MethodPost, url, token, body)
		defer response.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(response.Body).Decode(&out)
		return response.StatusCode, out
	}
	status, created := post(drInput("API 요청"), "node-token")
	if status != http.StatusCreated || created["request"].(map[string]any)["id"] != "dr-"+strconv.FormatInt(x.ID, 10)+"-1" || created["request"].(map[string]any)["requested_by"] != "node" {
		t.Fatalf("create status=%d body=%v", status, created)
	}
	status, dup := post(drInput("API 요청"), "node2-token")
	if status != http.StatusOK || dup["duplicate"] != true || dup["request"].(map[string]any)["id"] != created["request"].(map[string]any)["id"] {
		t.Fatalf("resend status=%d body=%v", status, dup)
	}
	status, conflict := post(drInput("다른 요청"), "node-token")
	if status != http.StatusConflict || conflict["error"] != "decision_request_open" {
		t.Fatalf("second open status=%d body=%v", status, conflict)
	}
	zero := drInput("선택지 없음")
	zero.Options.Options = nil
	status, invalid := post(zero, "node-token")
	if status != http.StatusBadRequest || !strings.Contains(invalid["error"].(string), "at least one option") {
		t.Fatalf("zero options status=%d body=%v", status, invalid)
	}
	status, _ = post(drInput("x"), "")
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", status)
	}
	missing := request(t, h.Client(), http.MethodPost, h.URL+"/v1/tasks/987654321/decision-request", "node-token", drInput("x"))
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown task status=%d", missing.StatusCode)
	}
	resolve := request(t, h.Client(), http.MethodPost, url+"/resolve", "node-token", store.DecisionResolveInput{RequestID: "dr-" + strconv.FormatInt(x.ID, 10) + "-9", Kind: "answered", Option: "A"})
	resolve.Body.Close()
	if resolve.StatusCode != http.StatusConflict {
		t.Fatalf("stale resolve status=%d", resolve.StatusCode)
	}
}

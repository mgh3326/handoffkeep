package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// Invariant (operator decision 359, hk 4987): the request's requester lane
// cancels its started, dots-claimed request with a reason in one call. The
// reason is recorded on the task, dots' later reply is refused as on any
// settled task, the request leaves dots_inbox, and another lane is refused
// with the store's rule, never with its own input echoed.
func TestDotsCancelAPI(t *testing.T) {
	_, url := testdb.Schema(t, "dots_cancel_api")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Tokens: Tokens{"cli": "test-token"}, Service: Service{Store: st}}.Handler())
	defer h.Close()
	ctx := t.Context()

	task, err := st.CreateDotsRequest(ctx, "cli", store.DotsRequestInput{Title: "Vehicles", Body: "Make six vehicles", RequesterLane: "requesting-lane"})
	if err != nil {
		t.Fatal(err)
	}
	revision := task.UpdatedAt
	start, err := st.ReplyDots(ctx, "operator:test@example.com", "chatgpt", store.DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "Starting", IdempotencyKey: "start", ExpectedUpdatedAt: &revision})
	if err != nil || start.RunToken == "" {
		t.Fatalf("start=%+v err=%v", start, err)
	}
	claimed, found, err := st.GetTask(ctx, task.ID)
	if err != nil || !found || claimed.State != store.StateResearching || claimed.ClaimedBy != "dots" {
		t.Fatalf("claimed=%+v found=%v err=%v", claimed, found, err)
	}
	if inbox, err := st.DotsInbox(ctx, 0, 100); err != nil || len(inbox) != 1 || inbox[0].ID != task.ID {
		t.Fatalf("inbox before cancel=%+v err=%v", inbox, err)
	}

	path := "/v1/dots/requests/" + strconv.FormatInt(task.ID, 10) + "/cancel"

	// Another lane is refused with the rule; the refusal carries no echoed lane.
	code, out := projectRequest(t, h.URL, http.MethodPost, path, "test-token", map[string]any{"requester_lane": "intruder-lane", "reason": "stop"})
	if code != http.StatusConflict || out["error"] != "task_conflict" {
		t.Fatalf("intruder status=%d body=%v", code, out)
	}
	if reason, _ := out["reason"].(string); reason != "only the request's requester lane may cancel it" || strings.Contains(reason, "intruder-lane") {
		t.Fatalf("intruder reason=%q (body=%v)", reason, out)
	}

	// The requester cancels with a reason.
	const why = "operator allowed real-model resemblance; the vehicles were made with Meshy"
	code, out = projectRequest(t, h.URL, http.MethodPost, path, "test-token", map[string]any{"requester_lane": "requesting-lane", "reason": why})
	if code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%v", code, out)
	}
	if got, _ := out["task"].(map[string]any); got["state"] != "dropped" || got["category"] != "canceled" {
		t.Fatalf("cancelled task=%v", got)
	}

	got, found, err := st.GetTask(ctx, task.ID)
	if err != nil || !found || got.State != store.StateDropped || got.CanceledAt == nil {
		t.Fatalf("stored task=%+v found=%v err=%v", got, found, err)
	}
	if got.Refs.Closure == nil || got.Refs.Closure.Type != "cancelled" || got.Refs.Closure.Evidence != why {
		t.Fatalf("reason not recorded on the task: %+v", got.Refs.Closure)
	}

	// dots' later reply is refused as on any settled task.
	_, err = st.ReplyDots(ctx, "operator:test@example.com", "chatgpt", store.DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "Done", IdempotencyKey: "result", RunToken: start.RunToken})
	if !errors.Is(err, store.ErrTaskConflict) {
		t.Fatalf("dots reply after cancel: %v", err)
	}

	// The request left dots_inbox.
	if inbox, err := st.DotsInbox(ctx, 0, 100); err != nil || len(inbox) != 0 {
		t.Fatalf("inbox after cancel=%+v err=%v", inbox, err)
	}

	// A cancel without the bearer token is refused before any write.
	code, _ = projectRequest(t, h.URL, http.MethodPost, path, "", map[string]any{"requester_lane": "requesting-lane", "reason": why})
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", code)
	}
}

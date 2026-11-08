package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// The refusals below are decided before any store state is read, so they
// hold on a store with no database.
func TestDotsReplyRunTokenRefusalCodes(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", "on")
	o := operatorTools{service: api.Service{Store: &store.Store{}}, principal: principal{Actor: "operator:test@example.com", Channel: "chatgpt"}}
	ctx := context.Background()
	in := store.DotsReplyInput{TaskID: 1, Kind: "PROGRESS", Body: "Found a lead", IdempotencyKey: "progress"}
	if _, _, err := o.dotsReply(ctx, nil, in); err == nil || err.Error() != "dots_run_token_required" {
		t.Fatalf("missing token: err=%v", err)
	}
	in.RunToken = "not-a-token"
	if _, _, err := o.dotsReply(ctx, nil, in); err == nil || err.Error() != "dots_run_token_mismatch" {
		t.Fatalf("malformed token: err=%v", err)
	}
}

func TestDotsInboxShowsStartedWithoutToken(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", "off")
	st := newOperatorStore(t)
	ctx := context.Background()
	task, err := st.CreateDotsRequest(ctx, "caller", store.DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	current, found, err := st.GetTask(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("task found=%v err=%v", found, err)
	}
	started, err := st.ReplyDots(ctx, "operator:test@example.com", "chatgpt", store.DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "Starting", IdempotencyKey: "start", ExpectedUpdatedAt: &current.UpdatedAt})
	if err != nil || started.RunToken == "" {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	text, failed := toolText(t, session, "dots_inbox", map[string]any{})
	if failed || !strings.Contains(text, `"started":true`) || !strings.Contains(text, `"run_started_at"`) {
		t.Fatalf("inbox failed=%v text=%s", failed, text)
	}
	if strings.Contains(text, started.RunToken) {
		t.Fatal("dots_inbox exposed the run token")
	}
}

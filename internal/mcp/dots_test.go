package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestDotsToolsRegistered(t *testing.T) {
	srv := httptest.NewServer(HTTPHandler(api.Service{}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]bool{}
	for _, tool := range list.Tools {
		if tool.Name == "dots_inbox" || tool.Name == "dots_reply" {
			tools[tool.Name] = true
		}
	}
	if !tools["dots_inbox"] || !tools["dots_reply"] {
		t.Fatalf("dots tools: %v", tools)
	}
}

func TestDotsReplyWritesSwitch(t *testing.T) {
	o := operatorTools{service: api.Service{Store: &store.Store{}}}
	for _, value := range []string{"", "off", "invalid"} {
		t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", value)
		_, _, err := o.dotsReply(context.Background(), nil, store.DotsReplyInput{})
		if err == nil || err.Error() != "dots_reply_writes_disabled" {
			t.Fatalf("setting %q: err=%v", value, err)
		}
	}
	t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", "on")
	_, _, err := o.dotsReply(context.Background(), nil, store.DotsReplyInput{})
	if err == nil || !strings.Contains(err.Error(), "invalid dots reply") {
		t.Fatalf("on: err=%v", err)
	}
}

func TestDotsInboxAvailableWhenWritesOff(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", "off")
	st := newOperatorStore(t)
	if _, err := st.CreateDotsRequest(context.Background(), "caller", store.DotsRequestInput{Title: "Research", Body: "Investigate"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	text, failed := toolText(t, session, "dots_inbox", map[string]any{})
	if failed || !strings.Contains(text, "Investigate") {
		t.Fatalf("inbox failed=%v text=%s", failed, text)
	}
	text, failed = toolText(t, session, "dots_reply", map[string]any{"task_id": 1, "kind": "RESULT", "body": "done", "idempotency_key": "key"})
	if !failed || !strings.Contains(text, "dots_reply_writes_disabled") {
		t.Fatalf("reply failed=%v text=%s", failed, text)
	}
}

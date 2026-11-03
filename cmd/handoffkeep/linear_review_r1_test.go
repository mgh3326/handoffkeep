package main

import (
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestR1DisabledMetadataReturns503(t *testing.T) {
	clearLinearConfig(t)
	cfg := linearFromEnv(&store.Store{})
	handler := api.Server{Tokens: api.Tokens{"operator": "fake-token"}, LinearMetadata: cfg.metadata}.Handler()
	defer func() {
		if p := recover(); p != nil {
			t.Fatal("disabled metadata panicked", p)
		}
	}()
	req := httptest.NewRequest(http.MethodPost, "/v1/linear/metadata/refresh", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer fake-token")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != http.StatusServiceUnavailable || !strings.Contains(out.Body.String(), "linear_metadata_disabled") {
		t.Fatal(out.Code, out.Body.String())
	}
}
func TestR1BoardRequiresIssueWebhook(t *testing.T) {
	clearLinearConfig(t)
	t.Setenv("HANDOFFKEEP_LINEAR_WORK_BOARD", "on")
	t.Setenv("HANDOFFKEEP_LINEAR_API_KEY", "fake-key")
	st := &store.Store{}
	if cfg := linearFromEnv(st); cfg.boardWorker != nil || st.LinearBoardEnabled() {
		t.Fatal("board enabled without issue webhook")
	}
	t.Setenv("HANDOFFKEEP_LINEAR_WEBHOOK_SECRET", "fake-signing-secret")
	if cfg := linearFromEnv(st); cfg.webhook == nil || cfg.boardWorker == nil || !st.LinearBoardEnabled() {
		t.Fatal("complete configuration not enabled")
	}
}

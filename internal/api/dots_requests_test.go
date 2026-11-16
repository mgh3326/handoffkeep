package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestDotsRequestAPI(t *testing.T) {
	_, url := testdb.Schema(t, "dots_request_api")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Tokens: Tokens{"caller": "test-token"}, Service: Service{Store: st}}.Handler())
	defer h.Close()
	input := map[string]any{"title": "Research", "body": "Investigate"}
	code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/dots/requests", "test-token", input)
	task, _ := out["task"].(map[string]any)
	if code != http.StatusCreated || task["lane"] != "dots" || task["parent_lane"] != "caller" || task["source"] != "dots" {
		t.Fatalf("status=%d body=%v", code, out)
	}
	code, _ = projectRequest(t, h.URL, http.MethodPost, "/v1/dots/requests", "", input)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", code)
	}
}

// Invariant (operator decision 346): a dots request the caller can fix answers
// 400 invalid_context with a store-authored reason that names the failed rule,
// never echoing the refused input, and a store failure stays 500.
func TestDotsRequestRefusalCarriesReason(t *testing.T) {
	_, url := testdb.Schema(t, "dots_request_refusal")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	srv := Server{Tokens: Tokens{"caller": "test-token"}, Service: Service{Store: st}}
	h := httptest.NewServer(srv.Handler())
	defer h.Close()
	template := "[목적] 홈 화면\n[유지할 것] 색상\n[범위] 1화면\n[제약] 없음\n[산출물] PNG\n[완료 기준] 채택\n[입력] 커밋 9f14c82, 캡처 환경: 로컬 빌드"
	for _, tc := range []struct {
		name        string
		in          map[string]any
		wantReason  string
		notContains string
	}{
		{"unknown type", map[string]any{"title": "x", "body": template, "type": "NO_SUCH_TYPE"}, "invalid dots request type: CREATE_MOCKUP, CREATE_ASSET or REVIEW_IMPLEMENTATION", "NO_SUCH_TYPE"},
		{"missing template section", map[string]any{"title": "x", "body": "just prose", "type": "CREATE_MOCKUP"}, "invalid dots request: the body lacks a [목적] section with content", ""},
		{"bad notify lane", map[string]any{"title": "x", "body": "Investigate\n[알릴 레인] Not A Lane"}, "the [알릴 레인] line names a lane that is not [A-Za-z0-9._-]{1,128}", "Not A Lane"},
		{"bad options", map[string]any{"title": "x", "body": "Investigate", "options": []map[string]any{{"key": "Z", "label": "one"}}}, "option keys are single letters A-F", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/dots/requests", "test-token", tc.in)
			if code != http.StatusBadRequest || out["error"] != "invalid_context" {
				t.Fatalf("status=%d body=%v", code, out)
			}
			reason, _ := out["reason"].(string)
			if reason != tc.wantReason {
				t.Fatalf("reason=%q, want %q (body=%v)", reason, tc.wantReason, out)
			}
			if tc.notContains != "" && strings.Contains(reason, tc.notContains) {
				t.Fatalf("reason echoes the refused input: %q", reason)
			}
		})
	}
	t.Run("store failure stays 500", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r := httptest.NewRequest(http.MethodPost, "/v1/dots/requests", strings.NewReader(`{"title":"x","body":"Investigate"}`)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		srv.dotsRequestCreate(w, r)
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"internal_error"`) {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	})
}

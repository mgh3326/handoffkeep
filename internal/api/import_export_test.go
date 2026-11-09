package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportReadGrantIsOptInAndScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth")
	if err := os.WriteFile(path, []byte("HANDOFFKEEP_TOKEN_youtrack=fixture-secret\nHANDOFFKEEP_IMPORT_READONLY_youtrack=true\nHANDOFFKEEP_IMPORT_READONLY_peer=true\nHANDOFFKEEP_IMPORT_READONLY_bad=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tokens, _, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := LoadImportReadGrants(path, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if !grants["youtrack"] || grants["peer"] || grants["bad"] {
		t.Fatalf("grants=%v", grants)
	}
	h := Server{Tokens: Tokens{"youtrack": "fixture-secret", "peer": "peer-secret"}, ImportReadGrants: grants}.Handler()
	for _, tc := range []struct {
		name, token, method, path string
		want                      int
		code                      string
	}{
		{"missing grant", "peer-secret", http.MethodGet, "/v1/import/not-a-resource", http.StatusForbidden, "import_read_forbidden"},
		{"bad token", "wrong", http.MethodGet, "/v1/import/not-a-resource", http.StatusUnauthorized, "unauthorized"},
		{"unknown resource with grant", "fixture-secret", http.MethodGet, "/v1/import/not-a-resource", http.StatusNotFound, "not_found"},
		{"other read route blocked", "fixture-secret", http.MethodGet, "/v1/tasks", http.StatusForbidden, "import_read_only"},
		{"write verb blocked", "fixture-secret", http.MethodPost, "/v1/import/tasks", http.StatusForbidden, "import_read_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.code != "" && !containsJSONError(w.Body.String(), tc.code) {
				t.Fatalf("body=%s", w.Body.String())
			}
		})
	}
}

func TestImportExportRejectsMalformedPagingBeforeStoreRead(t *testing.T) {
	h := Server{Tokens: Tokens{"yt": "fixture-secret"}, ImportReadGrants: map[string]bool{"yt": true}}.Handler()
	r := httptest.NewRequest(http.MethodGet, "/v1/import/tasks?after_id=-1", nil)
	r.Header.Set("Authorization", "Bearer fixture-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !containsJSONError(w.Body.String(), "invalid_context") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestImportUpdatedRoutesRejectInvalidPagingCursors(t *testing.T) {
	h := Server{Tokens: Tokens{"yt": "fixture-secret"}, ImportReadGrants: map[string]bool{"yt": true}}.Handler()
	for _, path := range []string{
		"/v1/import/tasks?after_at=nope",
		"/v1/import/comments?after_at=nope",
		"/v1/import/documents?project=demo&after_at=nope",
		"/v1/import/task-documents?task_ids=",
		"/v1/import/decisions?after_at=nope",
	} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", "Bearer fixture-secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || !containsJSONError(w.Body.String(), "invalid_context") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestImportExportRejectsEmptyProjectFilters(t *testing.T) {
	h := Server{Tokens: Tokens{"yt": "fixture-secret"}, ImportReadGrants: map[string]bool{"yt": true}}.Handler()
	for _, path := range []string{
		"/v1/import/tasks?project=",
		"/v1/import/documents?project=",
		"/v1/import/document-authors?project=",
	} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", "Bearer fixture-secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || !containsJSONError(w.Body.String(), "invalid_context") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestImportTaskSharedProjectIsEmpty(t *testing.T) {
	h := Server{Tokens: Tokens{"yt": "fixture-secret"}, ImportReadGrants: map[string]bool{"yt": true}}.Handler()
	r := httptest.NewRequest(http.MethodGet, "/v1/import/tasks?project=/shared", nil)
	r.Header.Set("Authorization", "Bearer fixture-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"items":[]`) || !strings.Contains(w.Body.String(), `"next_cursor":null`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func containsJSONError(body, want string) bool { return strings.Contains(body, `"error":"`+want+`"`) }

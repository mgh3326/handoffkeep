package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

func TestAppErrMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		body   map[string]string
	}{
		{"sentinel", store.ErrTaskNotFound, http.StatusNotFound, map[string]string{"error": "not_found"}},
		{"wrapped sentinel", fmt.Errorf("claim: %w", store.ErrQueueEmpty), http.StatusNotFound, map[string]string{"error": "queue_empty"}},
		{"sentinel with reason", fmt.Errorf("%w: claimed by another job", store.ErrTaskConflict), http.StatusConflict, map[string]string{"error": "task_conflict", "reason": "claimed by another job"}},
		{"store validation", errors.New("invalid bench rep"), http.StatusBadRequest, map[string]string{"error": "invalid_context"}},
		{"secret pattern", errors.New("secret_like_content:aws_key"), http.StatusBadRequest, map[string]string{"error": "secret_like_content", "pattern": "aws_key"}},
		{"integrity violation", &pgconn.PgError{Code: "23505"}, http.StatusBadRequest, map[string]string{"error": "invalid_context"}},
		{"data exception", &pgconn.PgError{Code: "22P02"}, http.StatusBadRequest, map[string]string{"error": "invalid_context"}},
		{"undefined column", &pgconn.PgError{Code: "42703"}, http.StatusInternalServerError, map[string]string{"error": "internal_error"}},
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), http.StatusInternalServerError, map[string]string{"error": "internal_error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/v1/tasks/1", nil)
			appErr(w, r, tc.err)
			if w.Code != tc.status {
				t.Fatalf("status=%d want %d", w.Code, tc.status)
			}
			var got map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.body) {
				t.Fatalf("body=%v want %v", got, tc.body)
			}
		})
	}
}

// A 500 carries the request's id in the body and in the one log line, next
// to the route pattern and the authenticated client, so an operator can go
// from a client's error to the server log and the Sentry event.
func TestInternalErrorCarriesRequestID(t *testing.T) {
	t.Setenv("HANDOFFKEEP_SENTRY_DSN", "")
	monitor, err := telemetry.FromEnv("test-revision")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		*r = *r.WithContext(store.WithWrkPrincipal(r.Context(), "node-a", nil))
		appErr(w, r, fmt.Errorf("query: %w", context.DeadlineExceeded))
	})
	w := httptest.NewRecorder()
	monitor.Handler(mux).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/tasks/7", nil))
	id := w.Header().Get("X-Request-ID")
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusInternalServerError || id == "" || !reflect.DeepEqual(body, map[string]string{"error": "internal_error", "request_id": id}) {
		t.Fatalf("status=%d header=%q body=%v", w.Code, id, body)
	}
	line := logs.String()
	for _, want := range []string{"api.internal_error ", "request_id=" + id + " ", `route="GET /v1/tasks/{id}"`, `client="node-a"`, `err="query: context deadline exceeded"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q lacks %q", line, want)
		}
	}
}

// Every rule must name a distinct sentinel: a duplicate would make the second
// row dead and hide which status is meant.
func TestErrorRulesDistinct(t *testing.T) {
	seen := map[error]string{}
	for _, rule := range errorRules {
		if prev, ok := seen[rule.err]; ok {
			t.Errorf("%v mapped twice (%s, %s)", rule.err, prev, rule.code)
		}
		seen[rule.err] = rule.code
	}
}

func TestDecodeRejectsTrailingDataAndOversize(t *testing.T) {
	var v struct{ A int }
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"A":1} {"A":2}`))
	if err := decode(r, &v, 64); err == nil {
		t.Fatal("trailing value accepted")
	}
	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{\"A\":1}\n"))
	if err := decode(r, &v, 64); err != nil || v.A != 1 {
		t.Fatalf("v=%+v err=%v", v, err)
	}
	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"A":`+strings.Repeat(" ", 8192)+`1}`))
	var tooLarge *http.MaxBytesError
	if err := decode(r, &v, 64); !errors.As(err, &tooLarge) {
		t.Fatalf("oversized body err=%v, want *http.MaxBytesError", err)
	}
}

package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// noDB drives the PR-state handlers directly over a store with no database:
// everything they can answer (auth, decoding, validation, bad query values) is
// answered before the store touches a pool, so these tests run without
// Postgres. The routes themselves are exercised through Handler by the
// database tests below.
type noDB struct{ server Server }

func newNoDB() noDB {
	return noDB{Server{Service: Service{Store: &store.Store{}}, Tokens: Tokens{"peer": "peer-token"}}}
}

func (n noDB) call(t *testing.T, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handlers := map[string]http.HandlerFunc{
		"/v1/changes/state": n.server.changeStatePut, "/v1/changes/checks": n.server.changeChecksPut, "/v1/changes/threads": n.server.changeThreadsPut,
		"/v1/changes/findings": n.server.changeFindingsPut, "/v1/changes/gate": n.server.changeGatePut, "/v1/changes/batch": n.server.changeBatch,
		"/v1/changes/one": n.server.changeOne,
	}
	rec := httptest.NewRecorder()
	handlers[req.URL.Path](rec, req)
	out := map[string]any{}
	_ = json.NewDecoder(rec.Body).Decode(&out)
	return rec.Code, out
}

func stateAt(min int) string {
	return time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Add(time.Duration(min) * time.Minute).Format(time.RFC3339)
}

var changeStateRoutes = []struct{ method, path string }{
	{http.MethodPut, "/v1/changes/state"},
	{http.MethodPut, "/v1/changes/checks"},
	{http.MethodPut, "/v1/changes/threads"},
	{http.MethodPut, "/v1/changes/findings"},
	{http.MethodPut, "/v1/changes/gate"},
	{http.MethodPost, "/v1/changes/batch"},
	{http.MethodGet, "/v1/changes/one?repo=example/app&pr=1"},
}

func TestChangeStateRoutesRequireAuth(t *testing.T) {
	n := newNoDB()
	for _, rt := range changeStateRoutes {
		for _, token := range []string{"", "wrong-token"} {
			if status, body := n.call(t, rt.method, rt.path, token, map[string]any{"repo": "example/app", "pr": 1}); status != http.StatusUnauthorized || body["error"] != "unauthorized" {
				t.Errorf("%s %s with token %q: %d %v", rt.method, rt.path, token, status, body)
			}
		}
	}
}

// A refused write explains itself: 400 invalid_context with the store's text
// as the reason.
func TestChangeStateWriteRefusalCarriesReason(t *testing.T) {
	n := newNoDB()
	cases := []struct {
		name, method, path string
		body               map[string]any
		reason             string
	}{
		{"state head", http.MethodPut, "/v1/changes/state", map[string]any{"repo": "example/app", "pr": 1, "head_sha": "abc", "state": "open", "observed_at": stateAt(1), "source": "webhook"}, "head_sha must be 40 lowercase hex"},
		{"checks state", http.MethodPut, "/v1/changes/checks", map[string]any{"repo": "example/app", "pr": 1, "head_sha": changeTestSHA, "observed_at": stateAt(1), "source": "poll", "checks": []any{map[string]any{"name": "ci", "state": "Queued"}}}, "check state and conclusion"},
		{"threads kind", http.MethodPut, "/v1/changes/threads", map[string]any{"repo": "example/app", "pr": 1, "observed_at": stateAt(1), "source": "poll", "threads": []any{map[string]any{"thread_id": "T", "author_kind": "nobody"}}}, "author_kind"},
		{"findings lens", http.MethodPut, "/v1/changes/findings", map[string]any{"repo": "example/app", "pr": 1, "reviewed_head": changeTestSHA, "lens": "a b", "observed_at": stateAt(1)}, "lens must match"},
		{"gate state", http.MethodPut, "/v1/changes/gate", map[string]any{"repo": "example/app", "pr": 1, "head_sha": changeTestSHA, "state": "maybe", "decided_at": stateAt(1)}, "state must be ready, blocked or pending"},
		{"bad repo", http.MethodPut, "/v1/changes/gate", map[string]any{"repo": "nope", "pr": 1, "head_sha": changeTestSHA, "state": "ready", "decided_at": stateAt(1)}, "repo must be owner/name"},
		{"empty batch", http.MethodPost, "/v1/changes/batch", map[string]any{"repo": "example/app", "pr": 1}, "send at least one"},
		{"batch section", http.MethodPost, "/v1/changes/batch", map[string]any{"repo": "example/app", "pr": 1, "gate": map[string]any{"head_sha": changeTestSHA, "state": "maybe", "decided_at": stateAt(1)}}, "gate: state must be"},
		{"future time", http.MethodPut, "/v1/changes/state", map[string]any{"repo": "example/app", "pr": 1, "head_sha": changeTestSHA, "state": "open", "observed_at": time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339), "source": "poll"}, "in the future"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := n.call(t, tc.method, tc.path, "peer-token", tc.body)
			reason, _ := body["reason"].(string)
			if status != http.StatusBadRequest || body["error"] != "invalid_context" || !strings.Contains(reason, tc.reason) {
				t.Fatalf("%d %v, want 400 invalid_context with a reason containing %q", status, body, tc.reason)
			}
		})
	}
	// A body that is not the shape (unknown field) is a plain 400.
	status, body := n.call(t, http.MethodPut, "/v1/changes/gate", "peer-token", map[string]any{"repo": "example/app", "pr": 1, "surprise": true})
	if status != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("unknown field: %d %v", status, body)
	}
}

func TestChangeOneRejectsBadInput(t *testing.T) {
	n := newNoDB()
	for _, q := range []string{"", "?repo=example/app", "?pr=1", "?repo=nope&pr=1", "?repo=example/app&pr=0", "?repo=example/app&pr=x",
		"?repo=example/app&pr=99999999999", "?repo=example/app&pr=1&all_heads=yes"} {
		if status, body := n.call(t, http.MethodGet, "/v1/changes/one"+q, "peer-token", nil); status != http.StatusBadRequest || body["error"] != "invalid_context" {
			t.Errorf("%q: %d %v", q, status, body)
		}
	}
	status, body := n.call(t, http.MethodGet, "/v1/changes/one?repo=example/app&pr=1&include=checks,diff", "peer-token", nil)
	if reason, _ := body["reason"].(string); status != http.StatusBadRequest || !strings.Contains(reason, "include must list") {
		t.Fatalf("bad include: %d %v", status, body)
	}
}

func stateBody(repo string, extra map[string]any) map[string]any {
	b := map[string]any{"repo": repo, "pr": 1}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func section(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	sections, _ := body["sections"].(map[string]any)
	s, ok := sections[name].(map[string]any)
	if !ok {
		t.Fatalf("no %s section in %v", name, body)
	}
	return s
}

func TestChangeStateWriteAndRead(t *testing.T) {
	h, _ := changesServer(t)
	repo := changeRepoName("state")
	get := func(query string) (int, map[string]any) {
		return changesCall(t, h, http.MethodGet, "/v1/changes/one?repo="+repo+"&pr=1"+query, "peer-token", nil)
	}
	// A PR hk has never seen is 404.
	if status, body := get(""); status != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("unseen: %d %v", status, body)
	}
	status, body := changesCall(t, h, http.MethodPut, "/v1/changes/state", "peer-token", stateBody(repo, map[string]any{
		"head_sha": changeTestSHA, "base_sha": changeTestSHA2, "base_ref": "main", "state": "open", "draft": false, "title": "A PR", "author": "octo", "observed_at": stateAt(10), "source": "webhook", "delivery_id": "d-1"}))
	if status != http.StatusOK || section(t, body, "state")["applied"] != true {
		t.Fatalf("state: %d %v", status, body)
	}
	// A stale write answers applied:false and leaves the row unchanged.
	status, body = changesCall(t, h, http.MethodPut, "/v1/changes/state", "peer-token", stateBody(repo, map[string]any{
		"head_sha": changeTestSHA2, "state": "closed", "title": "Older", "observed_at": stateAt(5), "source": "poll"}))
	if s := section(t, body, "state"); status != http.StatusOK || s["applied"] != false || s["reason"] != "stale" {
		t.Fatalf("stale state: %d %v", status, body)
	}
	_, one := get("")
	change := one["change"].(map[string]any)
	if change["title"] != "A PR" || change["state"] != "open" || change["head_sha"] != changeTestSHA || change["source"] != "webhook" {
		t.Fatalf("a stale write changed the row: %v", change)
	}
	// The other sections through their own routes.
	status, body = changesCall(t, h, http.MethodPut, "/v1/changes/checks", "peer-token", stateBody(repo, map[string]any{
		"head_sha": changeTestSHA, "observed_at": stateAt(11), "source": "poll", "complete": true,
		"checks": []any{map[string]any{"name": "build", "state": "completed", "conclusion": "success", "required": true}, map[string]any{"name": "lint", "state": "queued"}}}))
	if status != http.StatusOK || section(t, body, "checks")["applied"] != true {
		t.Fatalf("checks: %d %v", status, body)
	}
	status, body = changesCall(t, h, http.MethodPut, "/v1/changes/checks", "peer-token", stateBody(repo, map[string]any{
		"head_sha": changeTestSHA, "observed_at": stateAt(12), "source": "poll", "complete": true,
		"checks": []any{map[string]any{"name": "build", "state": "completed", "conclusion": "success"}}}))
	if status != http.StatusOK || section(t, body, "checks")["applied"] != true {
		t.Fatalf("complete checks: %d %v", status, body)
	}
	_, one = get("&include=checks")
	checks := one["checks"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["name"] != "build" {
		t.Fatalf("a complete write must drop the missing check from the read: %v", checks)
	}
	if _, has := one["threads"]; has {
		t.Fatalf("include=checks returned threads: %v", one)
	}
	// A batch carries several sections; the stale one is reported, the rest apply.
	status, body = changesCall(t, h, http.MethodPost, "/v1/changes/batch", "peer-token", stateBody(repo, map[string]any{
		"state":    map[string]any{"head_sha": changeTestSHA, "state": "open", "title": "stale", "observed_at": stateAt(1), "source": "poll"},
		"threads":  map[string]any{"observed_at": stateAt(13), "source": "poll", "complete": true, "threads": []any{map[string]any{"thread_id": "T1", "author_kind": "mergewake", "severity": "high", "excerpt": "fix this"}}},
		"findings": map[string]any{"reviewed_head": changeTestSHA, "round": 1, "pass": 1, "lens": "correctness", "observed_at": stateAt(13), "findings": []any{map[string]any{"severity": "high", "title": "bug", "thread_id": "T1"}}},
		"gate":     map[string]any{"head_sha": changeTestSHA, "state": "blocked", "reasons": []string{"threads_open"}, "policy_revision": "abcdef1234567", "decided_at": stateAt(13)},
	}))
	if status != http.StatusOK || section(t, body, "state")["applied"] != false || section(t, body, "threads")["applied"] != true ||
		section(t, body, "findings")["applied"] != true || section(t, body, "gate")["applied"] != true {
		t.Fatalf("batch: %d %v", status, body)
	}
	status, one = get("")
	if status != http.StatusOK {
		t.Fatalf("read: %d %v", status, one)
	}
	for _, key := range []string{"change", "gate", "checks", "threads", "findings", "freshness"} {
		if _, ok := one[key]; !ok {
			t.Fatalf("read has no %q: %v", key, one)
		}
	}
	if g := one["gate"].(map[string]any); g["state"] != "blocked" || g["current"] != true || g["policy_revision"] != "abcdef1234567" {
		t.Fatalf("gate: %v", g)
	}
	if f := one["findings"].([]any)[0].(map[string]any); f["status"] != "open" || f["thread_id"] != "T1" {
		t.Fatalf("finding: %v", f)
	}
	fresh := one["freshness"].(map[string]any)
	for _, key := range []string{"state", "checks", "threads", "findings", "gate"} {
		if fresh[key] == nil {
			t.Fatalf("freshness.%s missing: %v", key, fresh)
		}
	}
	// The list carries the gate, draft flag, freshness and open counts.
	status, list := changesCall(t, h, http.MethodGet, "/v1/changes?pr=https://github.com/"+repo+"/pull/1", "peer-token", nil)
	rows, _ := list["changes"].([]any)
	if status != http.StatusOK || len(rows) != 1 {
		t.Fatalf("list: %d %v", status, list)
	}
	row := rows[0].(map[string]any)
	if row["gate_state"] != "blocked" || row["open_threads"] != float64(1) || row["open_findings"] != float64(1) || row["freshness_min"] == nil {
		t.Fatalf("list row: %v", row)
	}
	if d, ok := row["draft"]; ok && d != false {
		t.Fatalf("draft: %v", d)
	}
}

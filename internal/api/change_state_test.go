package api

import (
	"bytes"
	"encoding/json"
	"fmt"
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
		{"finding id", http.MethodPut, "/v1/changes/findings", map[string]any{"repo": "example/app", "pr": 1, "reviewed_head": changeTestSHA, "lens": "correctness", "observed_at": stateAt(1), "findings": []any{map[string]any{"severity": "high"}}}, "a finding needs an id"},
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
		"findings": map[string]any{"reviewed_head": changeTestSHA, "round": 1, "pass": 1, "lens": "correctness", "observed_at": stateAt(13), "findings": []any{map[string]any{"id": "T1", "location": "internal/a.go:3", "severity": "high", "blocking": true, "summary": "bug", "url": "https://github.com/x/y/pull/1#r1", "author_replied": true}}},
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
	// A finding comes back under mergewake's prstatus.Finding field names, so
	// panewire can pass it through unchanged.
	f := one["findings"].([]any)[0].(map[string]any)
	want := map[string]any{"id": "T1", "location": "internal/a.go:3", "severity": "high", "blocking": true, "summary": "bug", "url": "https://github.com/x/y/pull/1#r1", "author_replied": true, "status": "open", "thread_id": "T1"}
	for k, v := range want {
		if f[k] != v {
			t.Fatalf("finding[%q]=%v, want %v (finding: %v)", k, f[k], v, f)
		}
	}
	for _, gone := range []string{"title", "path", "line", "ext_id", "replied"} {
		if _, has := f[gone]; has {
			t.Fatalf("finding still carries %q: %v", gone, f)
		}
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

// TestChangeReviewHistoryAndCheckDeliveryJSON pins the wire names of the
// review-history fields (on the list read and on the one-PR read's reviews
// section) and of a check's delivery_id (on GET /v1/changes/one), and that a request without
// them is unchanged (hk 3208).
func TestChangeReviewHistoryAndCheckDeliveryJSON(t *testing.T) {
	h, st := changesServer(t)
	x := relaneAPITask(t, st, relaneLane(t, "history"), "history target")
	repo := changeRepoName("history")
	pr := "https://github.com/" + repo + "/pull/1"
	digest := strings.Repeat("ab", 16)
	review := func(pass int, extra map[string]any) map[string]any {
		r := map[string]any{"reviewed_head": changeTestSHA, "round": 1, "pass": pass, "lens": "correctness", "patch_id": "abcdef0123"}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	event := func(r map[string]any) (int, map[string]any) {
		return changesCall(t, h, http.MethodPost, "/v1/task-events", "peer-token", map[string]any{"source": "mergewake.review", "source_ref": pr, "task_id": x.ID, "reviews": []any{r}})
	}
	if status, body := event(review(1, map[string]any{"reviewer": "sonnet@high", "same_family": false, "delta": true, "quiet": false, "outside_diff_blocking": 0, "patch_digest": digest})); status != http.StatusOK {
		t.Fatalf("review with history: %d %v", status, body)
	}
	if status, body := event(review(2, nil)); status != http.StatusOK {
		t.Fatalf("review without history (an old mergewake): %d %v", status, body)
	}
	status, body := changesCall(t, h, http.MethodGet, "/v1/changes?pr="+pr, "peer-token", nil)
	list, _ := body["changes"].([]any)
	if status != http.StatusOK || len(list) != 1 {
		t.Fatalf("list: %d %v", status, body)
	}
	reviews := list[0].(map[string]any)["reviews"].([]any)
	if len(reviews) != 2 {
		t.Fatalf("reviews: %v", reviews)
	}
	full, bare := reviews[0].(map[string]any), reviews[1].(map[string]any)
	want := map[string]any{"reviewer": "sonnet@high", "same_family": false, "delta": true, "quiet": false, "outside_diff_blocking": float64(0), "patch_digest": digest, "patch_id": "abcdef0123"}
	for k, v := range want {
		if full[k] != v {
			t.Errorf("review[%q]=%v (present=%v), want %v", k, full[k], full[k] != nil, v)
		}
	}
	for _, k := range []string{"reviewer", "same_family", "delta", "quiet", "outside_diff_blocking", "patch_digest"} {
		if _, has := bare[k]; has {
			t.Errorf("a review sent without %q reads back with it: %v", k, bare)
		}
	}

	// GET /v1/changes/one carries the same rows in its reviews section: in the
	// default response and with include=reviews, and not with another include.
	for _, q := range []string{"", "&include=reviews"} {
		status, one := changesCall(t, h, http.MethodGet, "/v1/changes/one?repo="+repo+"&pr=1"+q, "peer-token", nil)
		oneReviews, _ := one["reviews"].([]any)
		if status != http.StatusOK || len(oneReviews) != 2 {
			t.Fatalf("/one%s reviews: %d %v", q, status, one)
		}
		oneFull, oneBare := oneReviews[0].(map[string]any), oneReviews[1].(map[string]any)
		for k, v := range want {
			if oneFull[k] != v {
				t.Errorf("/one%s review[%q]=%v, want %v", q, k, oneFull[k], v)
			}
		}
		for _, k := range []string{"reviewer", "same_family", "delta", "quiet", "outside_diff_blocking", "patch_digest"} {
			if _, has := oneBare[k]; has {
				t.Errorf("/one%s: a review sent without %q reads back with it: %v", q, k, oneBare)
			}
		}
		if oneFull["id"] != full["id"] || oneBare["id"] != bare["id"] {
			t.Errorf("/one%s reviews are not the list's rows in id order: %v", q, oneReviews)
		}
		if q != "" {
			if _, has := one["checks"]; has {
				t.Errorf("include=reviews returned checks: %v", one)
			}
		}
	}
	if _, one := changesCall(t, h, http.MethodGet, "/v1/changes/one?repo="+repo+"&pr=1&include=checks", "peer-token", nil); one["reviews"] != nil {
		t.Errorf("include=checks returned reviews: %v", one["reviews"])
	}

	// A refused value: a plain 400 invalid_context on the event route.
	for name, extra := range map[string]map[string]any{
		"bad digest": {"patch_digest": "XYZ"}, "negative count": {"outside_diff_blocking": -1},
	} {
		if status, body := event(review(3, extra)); status != http.StatusBadRequest || body["error"] != "invalid_context" {
			t.Errorf("%s on an event: %d %v", name, status, body)
		}
	}
	// The findings route answers with the reason.
	findings := func(extra map[string]any) (int, map[string]any) {
		b := stateBody(repo, map[string]any{"reviewed_head": changeTestSHA, "round": 1, "pass": 4, "lens": "correctness", "observed_at": stateAt(1), "findings": []any{}})
		for k, v := range extra {
			b[k] = v
		}
		return changesCall(t, h, http.MethodPut, "/v1/changes/findings", "peer-token", b)
	}
	if status, body := findings(map[string]any{"patch_digest": "XYZ"}); status != http.StatusBadRequest || body["error"] != "invalid_context" || !strings.Contains(fmt.Sprint(body["reason"]), "patch_digest") {
		t.Errorf("bad digest on findings: %d %v", status, body)
	}
	if status, body := findings(map[string]any{"outside_diff_blocking": -3}); status != http.StatusBadRequest || body["error"] != "invalid_context" || !strings.Contains(fmt.Sprint(body["reason"]), "outside_diff_blocking") {
		t.Errorf("negative count on findings: %d %v", status, body)
	}
	if status, body := findings(map[string]any{"reviewer": "opus@high", "patch_digest": digest}); status != http.StatusOK {
		t.Fatalf("findings with history: %d %v", status, body)
	}

	// A check carries delivery_id after a write that sent one, none after one that did not.
	put := func(minute int, delivery string, checks ...any) {
		t.Helper()
		b := stateBody(repo, map[string]any{"head_sha": changeTestSHA, "observed_at": stateAt(minute), "source": "webhook", "checks": checks})
		if delivery != "" {
			b["delivery_id"] = delivery
		}
		if status, body := changesCall(t, h, http.MethodPut, "/v1/changes/checks", "peer-token", b); status != http.StatusOK || section(t, body, "checks")["applied"] != true {
			t.Fatalf("checks: %d %v", status, body)
		}
	}
	checksOf := func() map[string]map[string]any {
		t.Helper()
		_, one := changesCall(t, h, http.MethodGet, "/v1/changes/one?repo="+repo+"&pr=1&include=checks&all_heads=1", "peer-token", nil)
		out := map[string]map[string]any{}
		for _, c := range one["checks"].([]any) {
			out[c.(map[string]any)["name"].(string)] = c.(map[string]any)
		}
		return out
	}
	put(10, "delivery-1", map[string]any{"name": "build", "state": "queued"}, map[string]any{"name": "lint", "state": "queued"})
	got := checksOf()
	if got["build"]["delivery_id"] != "delivery-1" || got["lint"]["delivery_id"] != "delivery-1" {
		t.Fatalf("checks after a write with delivery_id: %v", got)
	}
	put(11, "", map[string]any{"name": "lint", "state": "completed", "conclusion": "success"})
	got = checksOf()
	if _, has := got["lint"]["delivery_id"]; has {
		t.Fatalf("a check changed by a write without delivery_id carries one: %v", got["lint"])
	}
	if got["build"]["delivery_id"] != "delivery-1" {
		t.Fatalf("a check the write did not touch lost its delivery_id: %v", got["build"])
	}
	status, body = changesCall(t, h, http.MethodPut, "/v1/changes/checks", "peer-token", stateBody(repo, map[string]any{
		"head_sha": changeTestSHA, "observed_at": stateAt(12), "source": "webhook", "delivery_id": strings.Repeat("d", 129), "checks": []any{}}))
	if status != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("long delivery_id: %d %v", status, body)
	}
}

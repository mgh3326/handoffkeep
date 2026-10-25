package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (s *scenario) raw(actor, method, path string, header map[string]string, body []byte) *httptest.ResponseRecorder {
	s.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	if actor != "" {
		req.Header.Set("Authorization", "Bearer "+s.tokens[actor])
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func wantErr(t *testing.T, rec *httptest.ResponseRecorder, status int, code string, what string) {
	t.Helper()
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != status || body["error"] != code || len(body) != 1 || rec.Header().Get("Location") != "" || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("%s: %d %s (Location %q), want %d %s", what, rec.Code, rec.Body.String(), rec.Header().Get("Location"), status, code)
	}
}

func TestWrkHTTPAuthenticatesFirst(t *testing.T) {
	s := newScenario(t)
	routes := []struct{ method, path string }{
		{"GET", "/v1/wrk/capabilities"}, {"GET", "/v1/wrk/jobs/job-a"}, {"GET", "/v1/wrk/jobs/job-a/records"},
		{"POST", "/v1/wrk/jobs/job-a/records"}, {"GET", "/v1/wrk/jobs/job-a/records/1/content"},
		{"GET", "/v1/wrk/nope"}, {"DELETE", "/v1/wrk/jobs/job-a"}, {"GET", "/v1/wrk"}, {"GET", "/v1/wrk//capabilities"},
	}
	for _, r := range routes {
		wantErr(t, s.raw("", r.method, r.path, nil, nil), 401, "unauthorized", "no bearer "+r.method+" "+r.path)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(r.method, r.path, nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		s.handler.ServeHTTP(rec, req)
		wantErr(t, rec, 401, "unauthorized", "bad bearer "+r.path)
		wantErr(t, s.raw("unknown-host", r.method, r.path, nil, nil), 403, "scope_denied", "unknown id "+r.path)
	}
}

func TestWrkHTTPRoutesAndWire(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-w", "b2400-w", 2400, "T1")
	s.ok(s.post("desktop", "job-w", "round_open", openPayload(head('1'), nil, nil, nil)), "open")
	v := s.ok(s.post("desktop", "job-w", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/w/v.md", nil, verdictBytes("PASS", head('1'), "w"))), "verdict")
	wantErr(t, s.raw("desktop", "GET", "/v1/wrk/nope", nil, nil), 404, "not_found", "unknown route")
	wantErr(t, s.raw("desktop", "GET", "/v1/wrk//capabilities", nil, nil), 404, "not_found", "double slash, no redirect")
	wantErr(t, s.raw("desktop", "GET", "/v1/wrk/jobs/job-w/../job-x", nil, nil), 404, "not_found", "dot segment, no redirect")
	wantErr(t, s.raw("desktop", "GET", "/v1/wrk/jobs/job%2Dw", nil, nil), 404, "not_found", "percent alias")
	wantErr(t, s.raw("desktop", "DELETE", "/v1/wrk/jobs/job-w", nil, nil), 405, "method_not_allowed", "DELETE")
	wantErr(t, s.raw("desktop", "PATCH", "/v1/wrk/jobs/job-w/records", nil, nil), 405, "method_not_allowed", "PATCH")
	wantErr(t, s.raw("desktop", "POST", "/v1/wrk/capabilities", nil, nil), 405, "method_not_allowed", "POST capabilities")
	wantErr(t, s.raw("desktop", "GET", "/v1/wrk/jobs/job-none", nil, nil), 404, "not_found", "missing job")
	key := map[string]string{"Idempotency-Key": s.key(), "Content-Type": "text/plain"}
	wantErr(t, s.raw("desktop", "POST", "/v1/wrk/jobs/job-w/records", key, []byte(`{}`)), 415, "unsupported_media_type", "text/plain")
	big := map[string]string{"Idempotency-Key": s.key(), "Content-Type": "application/json"}
	wantErr(t, s.raw("desktop", "POST", "/v1/wrk/jobs/job-w/records", big, []byte(strings.Repeat(" ", 1048577))), 413, "request_too_large", "body over 1 MiB")
	for name, h := range map[string]map[string]string{
		"no key":        {"Content-Type": "application/json"},
		"uppercase key": {"Content-Type": "application/json", "Idempotency-Key": strings.ToUpper(s.key())},
	} {
		wantErr(t, s.raw("desktop", "POST", "/v1/wrk/jobs/job-w/records", h, []byte(`{}`)), 400, "invalid_request", name)
	}
	wantErr(t, s.raw("desktop", "GET", "/v1/wrk/jobs/job-w", nil, []byte(`{"x":1}`)), 400, "invalid_request", "read with a body")
	wantErr(t, s.raw("desktop", "GET", "/v1/wrk/capabilities?x=1", nil, nil), 400, "invalid_request", "capabilities query")
	for q, code := range map[string]string{
		"after_seq=6": "invalid_cursor", "snapshot_seq=99": "invalid_cursor", "limit=0": "invalid_cursor", "limit=501": "invalid_cursor",
		"after_seq=-1": "invalid_cursor", "after_seq=x": "invalid_cursor", "snapshot_seq=0": "invalid_cursor",
		"offset=1": "invalid_request", "limit=1&limit=2": "invalid_cursor",
	} {
		status := 400
		wantErr(t, s.raw("desktop", "GET", "/v1/wrk/jobs/job-w/records?"+q, nil, nil), status, code, "records "+q)
	}
	page := s.raw("pa-integrator", "GET", "/v1/wrk/jobs/job-w/records?limit=2", nil, nil)
	if page.Code != 200 || page.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("reader page: %d %s", page.Code, page.Header())
	}
	var rr map[string]any
	_ = json.Unmarshal(page.Body.Bytes(), &rr)
	if rr["has_more"] != true || num(rr["next_after_seq"]) != 2 || num(rr["snapshot_seq"]) != 4 {
		t.Fatalf("page %v", rr)
	}
	vseq := num(v["record"].(map[string]any)["seq"])
	content := s.raw("at-integrator", "GET", "/v1/wrk/jobs/job-w/records/"+jsonNum(vseq)+"/content", nil, nil)
	var cr map[string]any
	_ = json.Unmarshal(content.Body.Bytes(), &cr)
	if content.Code != 200 || cr["content_b64"] == nil || num(cr["size_bytes"]) != int64(len(verdictBytes("PASS", head('1'), "w"))) {
		t.Fatalf("content %d %s", content.Code, content.Body.String())
	}
	wantErr(t, s.raw("desktop", "GET", "/v1/wrk/jobs/job-w/records/1/content", nil, nil), 404, "not_found", "claim has no content")
	wantErr(t, s.raw("desktop", "GET", "/v1/wrk/jobs/job-w/records/01/content", nil, nil), 404, "not_found", "non-canonical seq")
	wantErr(t, s.raw("pa-integrator", "POST", "/v1/wrk/jobs/job-w/records", map[string]string{"Idempotency-Key": s.key(), "Content-Type": "application/json"}, []byte(`{"kind":"end","claim_seq":1,"expected_seq":4,"payload":{"end_type":"revoked","reason_code":"x"}}`)), 403, "scope_denied", "reader write")
	caps := s.raw("tt-integrator", "GET", "/v1/wrk/capabilities", nil, nil)
	var cp map[string]any
	_ = json.Unmarshal(caps.Body.Bytes(), &cp)
	if cp["client_id"] != "tt-integrator" || cp["policy_sha256"] != s.policy.SHA256() || cp["writes_available"] != true {
		t.Fatalf("capabilities %v", cp)
	}
}

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestWrkHTTPOffMode(t *testing.T) {
	s := newScenario(t)
	s.server.Wrk.Mode = WrkOff
	s.handler = s.server.Handler()
	wantErr(t, s.raw("desktop", "GET", "/v1/wrk/capabilities", nil, nil), 503, "wrk_unavailable", "off capabilities")
	wantErr(t, s.raw("", "GET", "/v1/wrk/capabilities", nil, nil), 401, "unauthorized", "off still authenticates first")
	nilWrk := Server{Tokens: s.server.Tokens}.Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/wrk/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+s.tokens["desktop"])
	nilWrk.ServeHTTP(rec, req)
	wantErr(t, rec, 503, "wrk_unavailable", "no wrk configuration")
	// Activation needs enforce; shadow accepts other writes.
	sh := newScenario(t)
	sh.server.Wrk.Mode = WrkShadow
	sh.server.Wrk.Engine.Enforce = false
	sh.ok(sh.post("director-1", "job-sh", "claim", claimPayload("job-sh", "b2400-sh", "director-1", 2400, "desktop", "builder", "T1")), "shadow claim")
	sh.ok(sh.post("desktop", "job-sh", "round_open", openPayload(head('1'), nil, nil, nil)), "shadow open")
	sh.refused(sh.post("director-1", "job-sh", "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "wrk_unavailable", "activate in shadow")
}

func TestTokensDistinctAndDigestLookup(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := randomToken(t), randomToken(t)
	if err := CheckTokensDistinct(write("ok", "HANDOFFKEEP_TOKEN_desktop="+a+"\nHANDOFFKEEP_TOKEN_rpi="+b+"\n")); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"duplicate id":   "HANDOFFKEEP_TOKEN_desktop=" + a + "\nHANDOFFKEEP_TOKEN_desktop=" + b + "\n",
		"equal token":    "HANDOFFKEEP_TOKEN_desktop=" + a + "\nHANDOFFKEEP_TOKEN_rpi=" + a + "\n",
		"repeat in kind": "# c\nHANDOFFKEEP_TOKEN_rpi=" + b + "\n\nHANDOFFKEEP_TOKEN_rpi=" + b + "\n",
	} {
		err := CheckTokensDistinct(write(name, body))
		if err == nil || strings.Contains(err.Error(), a) || strings.Contains(err.Error(), b) {
			t.Errorf("%s: %v", name, err)
		}
	}
	tokens := Tokens{"desktop": a, "rpi": b}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+b)
	if id, ok := tokens.Client(req); !ok || id != "rpi" {
		t.Fatalf("Client = %q %v", id, ok)
	}
	req.Header.Set("Authorization", "Bearer "+b[:len(b)-1])
	if _, ok := tokens.Client(req); ok {
		t.Fatal("a token prefix authenticated")
	}
	req.Header.Del("Authorization")
	if _, ok := tokens.Client(req); ok {
		t.Fatal("no bearer authenticated")
	}
	_ = http.StatusOK
}

// Refused writes leave an attempt row with only the allowed metadata, the
// unauthenticated ones with no client id (design §2 wrk_attempts).
func TestWrkHTTPRefusedWritesAreAudited(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-au", "b2400-au", 2400, "T1")
	before := len(s.mem.Attempts())
	s.raw("", "POST", "/v1/wrk/jobs/job-au/records", map[string]string{"Content-Type": "application/json"}, []byte(`{"secret":"x"}`))
	s.raw("desktop", "POST", "/v1/wrk/jobs/job-au/records", map[string]string{"Content-Type": "text/plain", "Idempotency-Key": s.key()}, []byte(`x`))
	s.raw("desktop", "GET", "/v1/wrk/jobs/job-au", nil, []byte(`x`))
	atts := s.mem.Attempts()[before:]
	if len(atts) != 2 {
		t.Fatalf("attempts %+v", atts)
	}
	if atts[0].ClientID != "" || atts[0].Code != "unauthorized" || atts[0].JobID != "job-au" {
		t.Fatalf("unauthenticated attempt %+v", atts[0])
	}
	if atts[1].ClientID != "desktop" || atts[1].Code != "unsupported_media_type" {
		t.Fatalf("media attempt %+v", atts[1])
	}
}

// Tester round 1 finding 5: route-level POST refusals are audited too.
func TestWrkHTTPRouteRefusalsAudited(t *testing.T) {
	s := newScenario(t)
	before := len(s.mem.Attempts())
	s.raw("desktop", "POST", "/v1/wrk/unknown-route", map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
	s.raw("desktop", "POST", "/v1/wrk/capabilities", map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
	s.raw("desktop", "POST", "/v1/wrk/jobs/job-x", map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
	atts := s.mem.Attempts()[before:]
	if len(atts) != 3 || atts[0].Code != "not_found" || atts[1].Code != "method_not_allowed" || atts[2].Code != "method_not_allowed" || atts[2].JobID != "job-x" {
		t.Fatalf("route refusal attempts %+v", atts)
	}
}

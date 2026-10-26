package wrkrecords_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

func TestWrkHTTPStrictErrorsGuardAndAudit(t *testing.T) {
	db := contractStore(t)
	db.seed(t, activeFixture(t), contentCatalog(t, loadVectors(t)))
	appendDB(t, db, "desktop", "job-a", "round_open", opening(h1, "", nil, nil))
	token := NewUUID()
	handler := api.Server{Service: api.Service{Store: db.runtime}, Tokens: api.Tokens{"desktop": token, "unknown": NewUUID()}}.Handler()
	raw := func(p Object) []byte {
		b, _ := json.Marshal(Request{Kind: "verdict", ClaimSeq: 1, ExpectedSeq: 3, Payload: p})
		return b
	}
	valid := raw(evidence(1, h1, "/strict/verdict", "clean", nil))
	cases := []struct {
		name, method, path, media, code string
		body                            []byte
		auth                            bool
	}{
		{"auth-first", "POST", "/v1/wrk/jobs/job-a/records", "application/json", "unauthorized", []byte("bad JSON"), false},
		{"method", "PUT", "/v1/wrk/jobs/job-a/records", "application/json", "method_not_allowed", nil, true},
		{"media", "POST", "/v1/wrk/jobs/job-a/records", "text/plain", "unsupported_media_type", valid, true},
		{"duplicate-json", "POST", "/v1/wrk/jobs/job-a/records", "application/json", "invalid_request", []byte(`{"kind":"verdict","kind":"end"}`), true},
		{"unknown-field", "POST", "/v1/wrk/jobs/job-a/records", "application/json", "invalid_request", []byte(`{"kind":"end","claim_seq":1,"expected_seq":3,"payload":{"end_type":"reaped","reason_code":"test","extra":true}}`), true},
		{"request-limit", "POST", "/v1/wrk/jobs/job-a/records", "application/json", "request_too_large", bytes.Repeat([]byte(" "), MaxRequest+1), true},
		{"read-body", "GET", "/v1/wrk/jobs/job-a", "", "invalid_request", []byte("x"), true},
		{"read-query", "GET", "/v1/wrk/jobs/job-a?secret=ignored", "", "invalid_request", nil, true},
		{"cursor", "GET", "/v1/wrk/jobs/job-a/records?after_seq=-1", "", "invalid_cursor", nil, true},
		{"slash-route", "GET", "/v1/wrk//jobs/job-a", "", "not_found", nil, true},
		{"dot-route", "GET", "/v1/wrk/jobs/../job-a", "", "not_found", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.media)
			request.Header.Set("Idempotency-Key", NewUUID())
			if tc.auth {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			if tc.name == "request-limit" {
				request.ContentLength = -1
			}
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, request)
			if out.Code != Status(Error(tc.code)) || strings.TrimSpace(out.Body.String()) != `{"error":"`+tc.code+`"}` || out.Header().Get("Location") != "" {
				t.Fatalf("unsafe/wrong error: status=%d code=%s", out.Code, tc.code)
			}
		})
	}
	t.Run("guard-no-store-no-echo-one-audit", func(t *testing.T) {
		content := evidence(1, h1, "/strict/generated", "sk-"+strings.Repeat("X", 32), nil)
		var before int
		if e := db.owner.QueryRow(t.Context(), `SELECT count(*) FROM wrk_attempts`).Scan(&before); e != nil {
			t.Fatal(e)
		}
		request := httptest.NewRequest("POST", "/v1/wrk/jobs/job-a/records", bytes.NewReader(raw(content)))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", NewUUID())
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, request)
		if out.Code != 400 || strings.TrimSpace(out.Body.String()) != `{"error":"invalid_request"}` {
			t.Fatal("guard response/echo")
		}
		var records, contents, after int
		if e := db.owner.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM wrk_records),(SELECT count(*) FROM wrk_contents),(SELECT count(*) FROM wrk_attempts)`).Scan(&records, &contents, &after); e != nil || records != 3 || contents != 0 || after != before+1 {
			t.Fatalf("guard persistence: records=%d contents=%d attempts_delta=%d", records, contents, after-before)
		}
	})
	t.Run("decoded-content-limit", func(t *testing.T) {
		content := bytes.Repeat([]byte("x"), MaxContent+1)
		p := evidence(1, h1, "/strict/oversize", "clean", nil)
		p["content_b64"], p["content_sha256"] = base64.StdEncoding.EncodeToString(content), SHA(content)
		request := httptest.NewRequest("POST", "/v1/wrk/jobs/job-a/records", bytes.NewReader(raw(p)))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", NewUUID())
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, request)
		if out.Code != 413 || strings.TrimSpace(out.Body.String()) != `{"error":"request_too_large"}` {
			t.Fatalf("decoded size code=%d", out.Code)
		}
	})
}

func TestWrkReadPermissionsAreIndependentAndRecovery(t *testing.T) {
	db := contractStore(t)
	v := activeFixture(t)
	db.seed(t, v, contentCatalog(t, loadVectors(t)))
	var policy Object
	_ = json.Unmarshal(v.Given.Policy, &policy)
	desktop := Obj(Obj(policy, "clients"), "desktop")
	desktop["permissions"] = []string{"job.read"}
	raw, _ := json.Marshal(policy)
	p, e := ParsePolicy(raw)
	if e != nil {
		t.Fatal(e)
	}
	engine := db.runtime.WrkEngine()
	engine.Policy = p
	if e = db.runtime.ConfigureWrkEngine(t.Context(), engine); e != nil {
		t.Fatal(e)
	}
	token := NewUUID()
	tokens := api.Tokens{"desktop": token, "director-1": NewUUID()}
	handler := api.Server{Service: api.Service{Store: db.runtime}, Tokens: tokens}.Handler()
	get := httptest.NewRequest("GET", "/v1/wrk/jobs/job-a", nil)
	get.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, get)
	if out.Code != 200 {
		t.Fatal("job.read incorrectly depends on capabilities.read")
	}
	engine.Recovery = true
	if e = db.runtime.ConfigureWrkEngine(t.Context(), engine); e != nil {
		t.Fatal(e)
	}
	for _, actor := range []string{"desktop", "director-1"} {
		request := httptest.NewRequest("GET", "/v1/wrk/jobs/job-a", nil)
		request.Header.Set("Authorization", "Bearer "+tokens[actor])
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, request)
		want := 503
		if actor == "director-1" {
			want = 200
		}
		if out.Code != want {
			t.Fatalf("recovery %s status=%d", actor, out.Code)
		}
	}
}

func TestWrkCommittedHTTPReplyLossReplaysExactly(t *testing.T) {
	db := contractStore(t)
	db.seed(t, activeFixture(t), contentCatalog(t, loadVectors(t)))
	token := NewUUID()
	handler := api.Server{Service: api.Service{Store: db.runtime}, Tokens: api.Tokens{"desktop": token}}.Handler()
	var first atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !first.Swap(true) {
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, r)
			if recorded.Code != 201 {
				t.Errorf("pre-loss commit status=%d", recorded.Code)
			}
			conn, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error("test hijack")
			}
			_ = conn.Close()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	body, _ := json.Marshal(Request{Kind: "round_open", ClaimSeq: 1, ExpectedSeq: 2, Payload: opening(h1, "", nil, nil)})
	key := NewUUID()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	send := func() (*http.Response, error) {
		r, e := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/wrk/jobs/job-a/records", bytes.NewReader(body))
		if e != nil {
			return nil, e
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		return client.Do(r)
	}
	if response, e := send(); e == nil {
		response.Body.Close()
		t.Fatal("lost reply unexpectedly acknowledged")
	}
	committed, e := db.runtime.WrkJob(t.Context(), "desktop", "job-a")
	if e != nil || committed.Summary.Used != 1 {
		t.Fatal("commit lost with reply")
	}
	response, e := send()
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	b, e := io.ReadAll(response.Body)
	if e != nil {
		t.Fatal(e)
	}
	var replay WriteResponse
	if json.Unmarshal(b, &replay) != nil || response.StatusCode != 200 || replay.Created || replay.Record.RecordHash != committed.Summary.TailHash || replay.Summary.Used != 1 {
		t.Fatal("HTTP replay differs or charged twice")
	}
}

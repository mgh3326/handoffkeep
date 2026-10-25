package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The area-exceptions PUT enforces its read cap and rejects trailing data:
// an oversized body must not decode a truncated prefix, and a valid object
// followed by another value is malformed. These refusals happen before the
// service, so a nil store is safe.
func TestBenchAreaExceptionsPutDecodeGuards(t *testing.T) {
	h := httptest.NewServer(Server{Tokens: Tokens{"operator": "op-token"}}.Handler())
	defer h.Close()
	put := func(body []byte) *http.Response {
		req, err := http.NewRequest(http.MethodPut, h.URL+"/v1/bench/area-exceptions", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer op-token")
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	code := func(resp *http.Response) (string, string) {
		defer resp.Body.Close()
		var out struct {
			Error, Detail string
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Error, out.Detail
	}

	// Valid object plus padding beyond the cap is refused, not truncated
	// into an accepted prefix.
	pad := `{"exceptions":[]}` + strings.Repeat(" ", benchRequestMaxBytes+8192)
	resp := put([]byte(pad))
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("oversized status=%d", resp.StatusCode)
	}
	errCode, detail := code(resp)
	if errCode != "invalid_bench_area_exceptions_json" || !strings.Contains(detail, "limit") {
		t.Fatalf("oversized body error=%q detail=%q", errCode, detail)
	}

	// A valid object followed by trailing data is malformed.
	resp = put([]byte(`{"exceptions":[]} {"trailing":1}`))
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("trailing status=%d", resp.StatusCode)
	}
	errCode, detail = code(resp)
	if errCode != "invalid_bench_area_exceptions_json" || !strings.Contains(detail, "trailing") {
		t.Fatalf("trailing body error=%q detail=%q", errCode, detail)
	}
	resp = put([]byte(`{"exceptions":[]} garbage`))
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("trailing garbage status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

// The cap boundary is exact: a body of precisely the cap passes the size
// check (and is then refused later as an empty batch), one byte more is
// refused as too large. Trailing whitespace is not trailing data. The
// non-operator refusal happens before any body handling or store call.
func TestBenchAreaExceptionsPutBoundariesAndWriter(t *testing.T) {
	h := httptest.NewServer(Server{Tokens: Tokens{"operator": "op-token", "peer": "peer-token"}}.Handler())
	defer h.Close()
	put := func(token string, body string) (int, string, string) {
		req, err := http.NewRequest(http.MethodPut, h.URL+"/v1/bench/area-exceptions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Error, Detail string
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Error, out.Detail
	}
	const base = `{"exceptions":[]}`
	limit := benchRequestMaxBytes + 4096

	status, errCode, detail := put("op-token", base+strings.Repeat(" ", limit-len(base)))
	if status != http.StatusBadRequest || strings.Contains(detail, "limit") || strings.Contains(errCode, "bench_area_exceptions_json") {
		t.Fatalf("cap-sized body: status=%d error=%q detail=%q, want it past the size check", status, errCode, detail)
	}
	status, errCode, detail = put("op-token", base+strings.Repeat(" ", limit-len(base)+1))
	if status != http.StatusBadRequest || errCode != "invalid_bench_area_exceptions_json" || !strings.Contains(detail, "limit") {
		t.Fatalf("cap+1 body: status=%d error=%q detail=%q", status, errCode, detail)
	}

	// Whitespace after the object is fine; whitespace then a value is not.
	_, errCode, detail = put("op-token", base+" \n\t ")
	if strings.Contains(detail, "trailing") || errCode == "invalid_bench_area_exceptions_json" {
		t.Fatalf("trailing whitespace refused: error=%q detail=%q", errCode, detail)
	}
	_, errCode, detail = put("op-token", base+" \n 7")
	if errCode != "invalid_bench_area_exceptions_json" || !strings.Contains(detail, "trailing") {
		t.Fatalf("whitespace then value: error=%q detail=%q", errCode, detail)
	}

	// A non-operator client is refused before the body is looked at.
	status, errCode, _ = put("peer-token", base)
	if status != http.StatusForbidden || errCode != "operator_required" {
		t.Fatalf("non-operator PUT: status=%d error=%q", status, errCode)
	}
	status, errCode, _ = put("peer-token", `{"nope":1}`)
	if status != http.StatusForbidden || errCode != "operator_required" {
		t.Fatalf("non-operator PUT with bad body: status=%d error=%q", status, errCode)
	}
}

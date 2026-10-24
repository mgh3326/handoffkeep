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

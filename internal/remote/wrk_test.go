package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestWrkNeverFollowsRedirect(t *testing.T) {
	var forwarded atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capture" {
			forwarded.Add(1)
			w.WriteHeader(500)
			return
		}
		http.Redirect(w, r, "/capture", 307)
	}))
	defer server.Close()
	c := Client{URL: server.URL, Token: "synthetic"}
	_, code := c.WrkCall(context.Background(), "GET", "/v1/wrk/capabilities", nil, "", "Capabilities")
	if code != "protocol_invalid" {
		t.Fatalf("code %s", code)
	}
	if forwarded.Load() != 0 {
		t.Fatal("wrk client followed redirect")
	}
}

package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestWrkNeverFollowsRedirect(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, method := range []string{"GET", "POST"} {
			t.Run(fmt.Sprintf("%d/%s", status, method), func(t *testing.T) {
				var forwarded atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/capture" {
						forwarded.Add(1)
						w.WriteHeader(500)
						return
					}
					http.Redirect(w, r, "/capture", status)
				}))
				defer server.Close()
				c := Client{URL: server.URL, Token: "synthetic", HTTP: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}}
				_, code := c.WrkCall(context.Background(), method, "/v1/wrk/capabilities", []byte(`{}`), "", "Capabilities")
				if code != "protocol_invalid" {
					t.Fatalf("code %s", code)
				}
				if forwarded.Load() != 0 {
					t.Fatal("wrk client followed redirect")
				}
			})
		}
	}
}

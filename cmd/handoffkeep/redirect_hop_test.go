package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	hkmcp "github.com/mgh3326/handoffkeep/internal/mcp"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A same-origin 307 back to the exact request URL followed by a response that
// is not HTTP keeps Go's transport diagnostic through the default CLI and the
// stdio MCP server built from configuredClient — as on main — and is never
// relabelled redirect_location_invalid. No credential reaches the output.
func TestExactURLHopTransportFailureDefaultCLIAndStdio(t *testing.T) {
	for _, mode := range []string{"cli", "stdio"} {
		t.Run(mode, func(t *testing.T) {
			isolateConfig(t)
			var hits atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if hits.Add(1) == 1 {
					w.Header().Set("Location", r.URL.RequestURI())
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				conn, buf, e := w.(http.Hijacker).Hijack()
				if e != nil {
					t.Error("fixture hijack failed")
					return
				}
				_, _ = buf.WriteString("invalid fixture response\r\n\r\n")
				_ = buf.Flush()
				_ = conn.Close()
			}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.Start()
			defer server.Close()
			t.Setenv("HANDOFFKEEP_URL", server.URL)
			t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
			t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID", cfFixtureID)
			t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET", cfFixtureSecret)

			var combined string
			if mode == "cli" {
				var out bytes.Buffer
				err := run([]string{"doc", "get", "--id", "5"}, &out, &out)
				if err == nil {
					t.Fatal("exact-URL hop failure unexpectedly succeeded")
				}
				combined = err.Error() + out.String()
			} else {
				srv := hkmcp.NewStdio(configuredClient(), "stdio")
				a, b := gmcp.NewInMemoryTransports()
				ss, e := srv.Connect(context.Background(), a, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer ss.Close()
				cs, e := gmcp.NewClient(&gmcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(context.Background(), b, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer cs.Close()
				res, e := cs.CallTool(context.Background(), &gmcp.CallToolParams{Name: "get_document", Arguments: map[string]any{"key": "fixture"}})
				if e != nil || res == nil || !res.IsError {
					t.Fatal("stdio did not return a tool error")
				}
				raw, _ := json.Marshal(res)
				combined = string(raw)
			}
			if strings.Contains(combined, "redirect_location_invalid") || !strings.Contains(combined, "malformed HTTP") {
				t.Errorf("%s: exact-URL hop transport failure was relabelled instead of keeping Go's diagnostic", mode)
			}
			if hits.Load() != 2 {
				t.Errorf("%s: server saw %d requests; want the 307 and the exact-URL hop", mode, hits.Load())
			}
			for _, leak := range []string{cfFixtureID, cfFixtureSecret, "fixture-token"} {
				if strings.Contains(combined, leak) {
					t.Errorf("%s: output carries a credential value", mode)
				}
			}
		})
	}
}

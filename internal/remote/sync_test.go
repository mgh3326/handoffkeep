package remote

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestSyncDeltaProtocol(t *testing.T) {
	change := `{"sync_id":1,"model":"document","model_id":"42","action":"D","at":"2026-01-01T00:00:00Z"}` + "\n"
	trailer := `{"_metadata_":{"last_sync_id":1,"max_sync_id":2,"schema_version":30}}` + "\n"
	for _, tc := range []struct {
		name, body string
		status     int
		ok, reset  bool
	}{
		{"valid", change + trailer, 200, true, false},
		{"missing trailer", change, 200, false, false},
		{"unordered", change + change + trailer, 200, false, false},
		{"after trailer", change + trailer + change, 200, false, false},
		{"invalid model", strings.Replace(change, "document", "unknown", 1) + trailer, 200, false, false},
		{"reset", `{"reset":true,"min_sync_id":12}`, 410, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Query().Get("after") != "0" {
					t.Error("missing auth or cursor")
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer h.Close()
			changes, meta, err := (Client{URL: h.URL, Token: "secret"}).SyncDelta(t.Context(), 0, 100)
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v", err)
			}
			if tc.ok && (len(changes) != 1 || meta.MaxSyncID != 2) {
				t.Fatalf("changes=%v meta=%v", changes, meta)
			}
			if tc.reset {
				var reset *store.SyncReset
				if !errors.As(err, &reset) || reset.MinSyncID != 12 {
					t.Fatalf("reset=%v", err)
				}
			}
		})
	}
}

func TestSyncDeltaResponseSizeLimit(t *testing.T) {
	trailer := `{"_metadata_":{"last_sync_id":0,"max_sync_id":0,"schema_version":30}}` + "\n"
	atLimit := trailer + strings.Repeat(" ", syncDeltaMaxBytes-len(trailer))
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"at limit", atLimit, true},
		{"one extra whitespace byte", atLimit + " ", false},
		{"record beyond limit", atLimit + `{"sync_id":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer h.Close()
			changes, _, err := (Client{URL: h.URL, Token: "secret"}).SyncDelta(t.Context(), 0, 100)
			if tc.ok {
				if err != nil || len(changes) != 0 {
					t.Fatalf("changes=%v err=%v", changes, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("oversized response err=%v", err)
			}
		})
	}
}

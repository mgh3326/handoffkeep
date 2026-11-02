package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSyncCLI(t *testing.T) {
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sync/delta" || r.URL.Query().Get("after") != "7" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request=%v", r)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"_metadata_":{"last_sync_id":7,"max_sync_id":7,"schema_version":29}}`+"\n")
	}))
	defer h.Close()
	var out bytes.Buffer
	if err := run([]string{"sync", "delta", "--after", "7", "--url", h.URL, "--token", "secret"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"last_sync_id":7`)) {
		t.Fatal(out.String())
	}
	for _, args := range [][]string{{"sync"}, {"sync", "unknown"}, {"sync", "delta", "--after", "-1"}, {"sync", "delta", "--limit", "1001"}, {"sync", "delta", "extra"}} {
		if err := run(args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
func TestSyncRetentionConfig(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
		ok   bool
	}{{"", 720 * time.Hour, true}, {"48h", 48 * time.Hour, true}, {"0", 0, false}, {"-1h", 0, false}, {"invalid", 0, false}} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("HANDOFFKEEP_SYNC_RETENTION", tc.raw)
			got, err := syncRetentionFromEnv()
			if (err == nil) != tc.ok || got != tc.want {
				t.Fatalf("got=%v err=%v", got, err)
			}
		})
	}
}

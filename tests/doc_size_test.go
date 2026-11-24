package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// PUT /v1/documents bounds the body the store keeps, not its JSON envelope:
// a body under store.DocumentMaxBytes is stored and read back unchanged even
// when its escaped JSON is larger than the old 512 KiB decode cap, and a
// longer body is refused by the store with document_too_large.
func TestDocumentBodyLimitIsTheStores(t *testing.T) {
	s, err := store.Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"node": "node-token"}}.Handler())
	defer h.Close()

	// Quotes and newlines escape to two bytes each on the wire, so ~900 KB of
	// text is well past 512 KiB of JSON.
	var quoted strings.Builder
	for quoted.Len() < 900<<10 {
		quoted.WriteString("He said \"hi\"\tand left\n")
	}
	cases := []struct {
		name string
		body string
	}{
		{"quotes and newlines", quoted.String()},
		// Every byte is a control character: six wire bytes each, the worst case
		// the decode cap is sized for.
		{"control characters at the limit", strings.Repeat("\x01", store.DocumentMaxBytes)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := fmt.Sprintf("k/docsize-%d-%s", time.Now().UnixNano(), strings.ReplaceAll(c.name, " ", "-"))
			put := request(t, h.Client(), http.MethodPut, h.URL+"/v1/documents/"+key, "node-token", map[string]any{"kind": "note", "body": c.body})
			if put.StatusCode != http.StatusOK {
				t.Fatalf("put status=%d", put.StatusCode)
			}
			put.Body.Close()

			get := request(t, h.Client(), http.MethodGet, h.URL+"/v1/documents/"+key, "node-token", nil)
			defer get.Body.Close()
			var got store.Document
			if err := json.NewDecoder(get.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Body != c.body {
				t.Fatalf("read back %d bytes, want %d unchanged", len(got.Body), len(c.body))
			}
		})
	}

	// One byte over the limit: refused by the store, the reason names both sizes,
	// and nothing is stored under the key.
	key := fmt.Sprintf("k/docsize-over-%d", time.Now().UnixNano())
	over := request(t, h.Client(), http.MethodPut, h.URL+"/v1/documents/"+key, "node-token", map[string]any{"kind": "note", "body": strings.Repeat("a", store.DocumentMaxBytes+1)})
	defer over.Body.Close()
	var refusal struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(over.Body).Decode(&refusal); err != nil {
		t.Fatal(err)
	}
	wantReason := fmt.Sprintf("body is %d bytes; the limit is %d", store.DocumentMaxBytes+1, store.DocumentMaxBytes)
	if over.StatusCode != http.StatusRequestEntityTooLarge || refusal.Error != "document_too_large" || refusal.Reason != wantReason {
		t.Fatalf("over-limit put status=%d body=%+v", over.StatusCode, refusal)
	}
	missing := request(t, h.Client(), http.MethodGet, h.URL+"/v1/documents/"+key, "node-token", nil)
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("refused document was stored: get status=%d", missing.StatusCode)
	}
}

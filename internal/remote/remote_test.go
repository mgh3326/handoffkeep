package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// GetDocumentByID must never report an empty success against a server that
// ignores ?id= (#584): the list envelope decodes to a zero Document, and any
// id that does not match the request is a failure, not a find.
func TestGetDocumentByIDSkew(t *testing.T) {
	doc := store.Document{ID: 5, Key: "k/five", Kind: "note", Body: "five"}
	other := store.Document{ID: 77, Key: "k/other", Kind: "note", Body: "other"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("id") {
		case "5": // normal: same id
			_ = json.NewEncoder(w).Encode(doc)
		case "9": // missing: not_found
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not_found"})
		case "20": // skew: server answered a different document
			_ = json.NewEncoder(w).Encode(other)
		default: // pre-?id= server: ignores the param, returns the list envelope
			_ = json.NewEncoder(w).Encode(map[string]any{"documents": []store.Document{doc, other}})
		}
	}))
	defer server.Close()
	c := Client{URL: server.URL, Token: "t"}
	ctx := context.Background()

	if got, found, err := c.GetDocumentByID(ctx, 5); err != nil || !found || got.ID != 5 {
		t.Fatalf("same id: doc=%+v found=%v err=%v", got, found, err)
	}
	if _, found, err := c.GetDocumentByID(ctx, 9); err != nil || found {
		t.Fatalf("not_found: found=%v err=%v", found, err)
	}
	for _, id := range []int64{1, 20} {
		got, found, err := c.GetDocumentByID(ctx, id)
		if err == nil || found {
			t.Fatalf("id %d: found=%v err=%v doc=%+v", id, found, err, got)
		}
		if !strings.Contains(err.Error(), "?id=") {
			t.Fatalf("id %d error should name the unsupported lookup: %v", id, err)
		}
	}
}

// Search asks for one extra row so a truncated page is still marked when the
// server predates per-row truncated flags (#584).
func TestSearchExtraRowProbe(t *testing.T) {
	var wireLimit string
	total := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		wireLimit = r.URL.Query().Get("limit")
		n, _ := strconv.Atoi(wireLimit)
		// Same contract as the real queryLimit: limit > 100 is a 400.
		if n > 100 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_context"})
			return
		}
		if n > total {
			n = total
		}
		xs := make([]store.SearchResult, 0, n)
		for i := 0; i < n; i++ {
			xs = append(xs, store.SearchResult{Scope: "docs", Key: "k/" + strconv.Itoa(i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": xs})
	}))
	defer server.Close()
	c := Client{URL: server.URL, Token: "t"}

	// More rows exist than the page: the extra row proves the cut.
	total = 25
	xs, err := c.Search(context.Background(), "q", "docs", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if wireLimit != "21" {
		t.Fatalf("wire limit=%s want 21", wireLimit)
	}
	if len(xs) != 20 {
		t.Fatalf("rows=%d want 20", len(xs))
	}
	for i, x := range xs {
		if !x.Truncated {
			t.Fatalf("row %d not marked truncated", i)
		}
	}

	// Exact fit: the page is complete and no row is marked.
	total = 20
	xs, err = c.Search(context.Background(), "q", "docs", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 20 {
		t.Fatalf("rows=%d want 20", len(xs))
	}
	for i, x := range xs {
		if x.Truncated {
			t.Fatalf("row %d wrongly marked truncated", i)
		}
	}

	// At the server cap the probe must not push the wire limit past 100 —
	// the API rejects it. Above the cap the request still fails loudly.
	total = 150
	if _, err = c.Search(context.Background(), "q", "docs", "", 100); err != nil {
		t.Fatalf("limit=100 must stay valid: %v", err)
	}
	if wireLimit != "100" {
		t.Fatalf("wire limit=%s want 100", wireLimit)
	}
	if _, err = c.Search(context.Background(), "q", "docs", "", 101); err == nil {
		t.Fatal("limit=101 must fail")
	}
	if wireLimit != "101" {
		t.Fatalf("wire limit=%s want 101", wireLimit)
	}
}

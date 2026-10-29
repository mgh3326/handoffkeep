package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The document browser lists key/title/kind/session/updated_at/size (never
// bodies) with a key cursor, and groups top-level prefixes with whole-key-space
// counts. Both routes are operator-only, like /ui/api/board/doc.
func TestUIDocsListAndPrefixes(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	base := fmt.Sprintf("ui-docs-%d", time.Now().UnixNano())
	seedPolicyDoc(t, s, base+"/a", "# Alpha\n\nfirst body\n")
	seedPolicyDoc(t, s, base+"/b", "---\ntask: 1\n---\n# Beta\n\nsecond body\n")
	seedPolicyDoc(t, s, base+"/c", "no heading here\n")

	var page struct {
		Prefix    string `json:"prefix"`
		Limit     int    `json:"limit"`
		Documents []struct {
			Key       string          `json:"key"`
			Title     string          `json:"title"`
			Kind      string          `json:"kind"`
			Session   string          `json:"session"`
			Size      int64           `json:"size"`
			UpdatedAt string          `json:"updated_at"`
			Body      json.RawMessage `json:"body"`
			SHA256    json.RawMessage `json:"sha256"`
		} `json:"documents"`
		HasMore    bool   `json:"has_more"`
		NextBefore string `json:"next_before"`
	}
	listURL := func(query string) string { return h.URL + "/ui/api/docs?" + query }
	if status := boardJSON(t, h.Client(), listURL("prefix="+url.QueryEscape(base+"/")+"&limit=2"), assertion, &page); status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	if page.Prefix != base+"/" || page.Limit != 2 || len(page.Documents) != 2 || !page.HasMore {
		t.Fatalf("first page=%+v", page)
	}
	// Newest key first: c, b. Titles come from the first heading, skipping a
	// YAML front-matter block; c has none and keeps an empty title.
	if page.Documents[0].Key != base+"/c" || page.Documents[0].Title != "" {
		t.Fatalf("first row=%+v", page.Documents[0])
	}
	if page.Documents[1].Key != base+"/b" || page.Documents[1].Title != "Beta" {
		t.Fatalf("second row=%+v", page.Documents[1])
	}
	if page.NextBefore != base+"/b" {
		t.Fatalf("next_before=%q", page.NextBefore)
	}
	for _, doc := range page.Documents {
		if doc.Body != nil || doc.SHA256 != nil {
			t.Fatalf("listing row carries body material: %+v", doc)
		}
		if doc.Kind != "note" || doc.Session != "" || doc.Size == 0 || doc.UpdatedAt == "" {
			t.Fatalf("row metadata=%+v", doc)
		}
	}
	page.Documents = nil
	if status := boardJSON(t, h.Client(), listURL("prefix="+url.QueryEscape(base+"/")+"&limit=2&before="+url.QueryEscape(base+"/b")), assertion, &page); status != http.StatusOK {
		t.Fatalf("second page status=%d", status)
	}
	if len(page.Documents) != 1 || page.Documents[0].Key != base+"/a" || page.Documents[0].Title != "Alpha" || page.HasMore || page.NextBefore != "" {
		t.Fatalf("second page=%+v", page)
	}

	var prefixes struct {
		Prefixes []struct {
			Prefix string `json:"prefix"`
			Count  int64  `json:"count"`
		} `json:"prefixes"`
	}
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/docs/prefixes", assertion, &prefixes); status != http.StatusOK {
		t.Fatalf("prefixes status=%d", status)
	}
	found := false
	for _, entry := range prefixes.Prefixes {
		if entry.Prefix == base+"/" {
			found = true
			if entry.Count < 3 {
				t.Fatalf("prefix count=%d", entry.Count)
			}
		}
	}
	if !found {
		t.Fatalf("prefix %q missing from %+v", base+"/", prefixes.Prefixes)
	}

	// An empty prefix lists every document; an unknown one is an empty array,
	// never null.
	page.Documents = nil
	if status := boardJSON(t, h.Client(), listURL("limit=1"), assertion, &page); status != http.StatusOK || len(page.Documents) != 1 {
		t.Fatalf("all-documents status=%d rows=%d", status, len(page.Documents))
	}
	response := uiRequest(t, h.Client(), http.MethodGet, listURL("prefix="+url.QueryEscape(base+"/missing")), assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `"documents":[]`) {
		t.Fatalf("empty prefix status=%d body=%s", response.StatusCode, body)
	}

	for _, bad := range []string{"limit=0", "limit=201", "limit=x", "prefix=%00"} {
		if status := boardJSON(t, h.Client(), listURL(bad), assertion, &page); status != http.StatusBadRequest {
			t.Fatalf("%s status=%d, want 400", bad, status)
		}
	}
	response = uiRequest(t, h.Client(), http.MethodPost, listURL("limit=1"), assertion, "")
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = uiRequest(t, h.Client(), http.MethodGet, listURL("limit=1"), "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	}
	response.Body.Close()

	// The page itself is operator-only like every other console page.
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/docs", assertion, "")
	if body := responseText(t, response); response.StatusCode != http.StatusOK || !strings.Contains(body, "/ui/static/console/docs.js") {
		t.Fatalf("page status=%d body=%s", response.StatusCode, body)
	}

	// A service principal may read the board JSON but has no /ui/doc access;
	// the browse routes must not widen that.
	service := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer service.Close()
	serviceAssertion := p3ServiceAssertion(t, fixture)
	response = p3Request(t, service.Client(), http.MethodGet, service.URL+"/ui/docs", serviceAssertion, nil, "")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("service page status=%d", response.StatusCode)
	}
	response.Body.Close()
	for _, path := range []string{"/ui/api/docs?limit=1", "/ui/api/docs/prefixes"} {
		response = p3Request(t, service.Client(), http.MethodGet, service.URL+path, serviceAssertion, nil, "")
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("service %s status=%d", path, response.StatusCode)
		}
		response.Body.Close()
	}
}

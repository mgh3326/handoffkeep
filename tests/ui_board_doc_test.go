package tests

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The board list and detail both carry body_doc verbatim (key or key#section);
// a task without one omits the field.
func TestUIBoardTaskBodyDoc(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "bodydoc")
	withDoc, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: "one line", Kind: "implement", CreatedBy: "board-test", BodyDoc: "design/body-test#3"})
	if err != nil {
		t.Fatal(err)
	}
	plain := createUITask(t, s, lane, "no body")

	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/board/tasks?lane="+lane, assertion, &list); status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	byID := map[float64]map[string]any{}
	for _, row := range list.Tasks {
		byID[row["id"].(float64)] = row
	}
	if got := byID[float64(withDoc.ID)]["body_doc"]; got != "design/body-test#3" {
		t.Fatalf("list body_doc=%v", got)
	}
	if _, present := byID[float64(plain.ID)]["body_doc"]; present {
		t.Fatalf("list row without body_doc carries the field: %v", byID[float64(plain.ID)])
	}
	var detail struct {
		Task map[string]any `json:"task"`
	}
	if status := boardJSON(t, h.Client(), fmt.Sprintf("%s/ui/api/board/tasks/%d", h.URL, withDoc.ID), assertion, &detail); status != http.StatusOK {
		t.Fatalf("detail status=%d", status)
	}
	if detail.Task["body_doc"] != "design/body-test#3" {
		t.Fatalf("detail body_doc=%v", detail.Task["body_doc"])
	}
}

type boardDocPayload struct {
	Key       string `json:"key"`
	Kind      string `json:"kind"`
	SHA256    string `json:"sha256"`
	UpdatedAt string `json:"updated_at"`
	Bytes     int    `json:"bytes"`
	Format    string `json:"format"`
	Reason    string `json:"reason"`
	Body      string `json:"body"`
}

// /ui/api/board/doc serves a document by exact key for the task overview and
// distinguishes found / missing / invalid key / unsupported format.
func TestUIBoardDoc(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	base := fmt.Sprintf("board-doc/%d", time.Now().UnixNano())
	docURL := func(key string) string { return h.URL + "/ui/api/board/doc?key=" + url.QueryEscape(key) }

	md := "# Title\n\n<script>alert(1)</script>\n\n[x](javascript:alert(1))\n"
	seedPolicyDoc(t, s, base+"/md", md)
	var got boardDocPayload
	if status := boardJSON(t, h.Client(), docURL(base+"/md"), assertion, &got); status != http.StatusOK {
		t.Fatalf("markdown status=%d", status)
	}
	// The body is returned as data, byte for byte; rendering safety is the
	// console's sanitizer boundary, not a server-side rewrite.
	if got.Format != "markdown" || got.Body != md || got.Key != base+"/md" || len(got.SHA256) != 64 || got.Bytes != len(md) || got.UpdatedAt == "" {
		t.Fatalf("markdown payload=%+v", got)
	}

	seedPolicyDoc(t, s, base+"/json", `{"release":"r1","items":[]}`)
	got = boardDocPayload{}
	if status := boardJSON(t, h.Client(), docURL(base+"/json"), assertion, &got); status != http.StatusOK || got.Format != "unsupported" || got.Reason != "json" {
		t.Fatalf("json status=%d payload=%+v", status, got)
	}
	large := strings.Repeat("a", 256<<10+1)
	seedPolicyDoc(t, s, base+"/large", large)
	got = boardDocPayload{}
	if status := boardJSON(t, h.Client(), docURL(base+"/large"), assertion, &got); status != http.StatusOK || got.Format != "unsupported" || got.Reason != "too_large" || got.Body != large {
		t.Fatalf("large status=%d format=%q reason=%q bytes=%d", status, got.Format, got.Reason, got.Bytes)
	}

	if status := boardJSON(t, h.Client(), docURL(base+"/missing"), assertion, &got); status != http.StatusNotFound {
		t.Fatalf("missing status=%d", status)
	}
	// The key is shape-checked like /ui/doc: no traversal, no absolute path,
	// no section suffix, nothing that could name a local file.
	for _, bad := range []string{"", "../etc/passwd", "/etc/passwd", base + "/md#3", "a b", "k/../x", "file:///etc/passwd"} {
		if status := boardJSON(t, h.Client(), docURL(bad), assertion, &got); status != http.StatusBadRequest {
			t.Fatalf("key %q status=%d, want 400", bad, status)
		}
	}
	response := uiRequest(t, h.Client(), http.MethodGet, docURL(base+"/md"), "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = uiRequest(t, h.Client(), http.MethodPost, docURL(base+"/md"), assertion, "")
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", response.StatusCode)
	}
	response.Body.Close()

	// A service principal may read the board JSON but has no /ui/doc access;
	// the document JSON view must not widen that.
	service := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer service.Close()
	serviceAssertion := p3ServiceAssertion(t, fixture)
	response = p3Request(t, service.Client(), http.MethodGet, service.URL+"/ui/api/board/doc?key="+url.QueryEscape(base+"/md"), serviceAssertion, nil, "")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("service doc status=%d", response.StatusCode)
	}
	response.Body.Close()
}

package youtrack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// SetArticleParent is one POST to the article whose body sets parentArticle
// alone, and the answer is decoded with the parent in it.
func TestClientSetArticleParent(t *testing.T) {
	var method, path, fields string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, fields = r.Method, r.URL.Path, r.URL.Query().Get("fields")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = w.Write([]byte(`{"id":"3-2","summary":"briefs","parentArticle":{"id":"3-1"}}`))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.SetArticleParent(context.Background(), "3-2", "3-1")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/articles/3-2" {
		t.Fatalf("%s %s, want POST /api/articles/3-2", method, path)
	}
	parent, ok := body["parentArticle"].(map[string]any)
	if !ok || parent["id"] != "3-1" || len(body) != 1 {
		t.Fatalf("body %v, want parentArticle alone", body)
	}
	if !strings.Contains(fields, "parentArticle(id)") {
		t.Fatalf("fields %q, want the parent in the answer", fields)
	}
	if got.ID != "3-2" || got.ParentArticle == nil || got.ParentArticle.ID != "3-1" {
		t.Fatalf("answer %+v", got)
	}
}

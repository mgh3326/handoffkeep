package youtrack

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// articleHit is one article in YouTrack's JSON shape.
func articleHit(id, projectID, summary, parent, content string) map[string]any {
	a := map[string]any{"id": id, "summary": summary, "$type": "Article", "project": map[string]any{"id": projectID, "$type": "Project"}}
	if parent != "" {
		a["parentArticle"] = map[string]any{"id": parent}
	}
	if content != "" {
		a["content"] = content
	}
	return a
}

// FindArticles is one search GET scoped to the project by short name and
// the summary phrase, keeping the top-level hits of the project id with
// exactly that summary, then one GET of each kept hit's content.
func TestWriteRemoteFindArticles(t *testing.T) {
	const summary = `[design] design/"q".md [note]`
	var query string
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/articles":
			query = r.URL.Query().Get("query")
			_ = json.NewEncoder(w).Encode([]any{
				articleHit("3-1", "0-30", summary, "", ""),
				articleHit("3-2", "0-31", summary, "", ""),
				articleHit("3-3", "0-30", summary+" / part 1 of 2", "", ""),
				articleHit("3-4", "0-30", summary, "3-1", ""),
			})
		case "/api/articles/3-1":
			_ = json.NewEncoder(w).Encode(articleHit("3-1", "0-30", summary, "", "body <!-- hk-op:9 -->"))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}}
	got, err := y.serve(t).FindArticles(t.Context(), "SHRDOC", "0-30", summary, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (ytwrite.RemoteArticle{ID: "3-1", ProjectID: "0-30", Summary: summary, Content: "body <!-- hk-op:9 -->"}) {
		t.Fatalf("hits %+v", got)
	}
	if query != `project: SHRDOC "[design] design/\"q\".md [note]"` || y.count() != 2 {
		t.Fatalf("query %q requests %v", query, y.requests)
	}
}

// A full search page is not proof of completeness: an error, not a create.
func TestWriteRemoteFindArticlesFullPageIsAnError(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		page := make([]any, articleSearchTop)
		for i := range page {
			page[i] = articleHit("3-x", "0-30", "s", "", "")
		}
		_ = json.NewEncoder(w).Encode(page)
	}}
	if _, err := y.serve(t).FindArticles(t.Context(), "SHRDOC", "0-30", "s", ""); err == nil || !strings.Contains(err.Error(), "not complete") {
		t.Fatalf("err %v", err)
	}
}

// With a parent, FindArticles keeps only the children of that parent: the
// same summary under another parent and a top-level article are not hits.
func TestWriteRemoteFindArticlesScopedToParent(t *testing.T) {
	const summary = "design/x.md"
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/articles":
			_ = json.NewEncoder(w).Encode([]any{
				articleHit("3-1", "0-30", summary, "", ""),
				articleHit("3-4", "0-30", summary, "3-1", ""),
				articleHit("3-5", "0-30", summary, "3-9", ""),
			})
		case "/api/articles/3-4":
			_ = json.NewEncoder(w).Encode(articleHit("3-4", "0-30", summary, "3-1", "body"))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}}
	got, err := y.serve(t).FindArticles(t.Context(), "SHRDOC", "0-30", summary, "3-1")
	if err != nil {
		t.Fatal(err)
	}
	want := ytwrite.RemoteArticle{ID: "3-4", ProjectID: "0-30", Summary: summary, Content: "body", ParentID: "3-1"}
	if len(got) != 1 || got[0] != want || y.count() != 2 {
		t.Fatalf("hits %+v requests %v", got, y.requests)
	}
}

// CreateArticle sends parentArticle when the create names a parent; a
// top-level create carries no parentArticle key.
func TestWriteRemoteArticleCreateParent(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"3-78","$type":"Article"}`))
	}}
	wr := y.serve(t)
	if _, err := wr.CreateArticle(t.Context(), ytwrite.ArticleCreate{ProjectID: "0-30", ParentID: "3-1", Summary: "s", Content: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := wr.CreateArticle(t.Context(), ytwrite.ArticleCreate{ProjectID: "0-30", Summary: "s", Content: "c"}); err != nil {
		t.Fatal(err)
	}
	var child struct {
		ParentArticle *struct {
			ID string `json:"id"`
		} `json:"parentArticle"`
	}
	if err := json.Unmarshal([]byte(y.bodies[0]), &child); err != nil || child.ParentArticle == nil || child.ParentArticle.ID != "3-1" {
		t.Fatalf("child body %s %v", y.bodies[0], err)
	}
	if strings.Contains(y.bodies[1], "parentArticle") {
		t.Fatalf("top-level body %s", y.bodies[1])
	}
}

// CreateArticle is one POST with the project, the summary and the content;
// UpdateArticle one POST to the article with the summary and the content.
func TestWriteRemoteArticleWrites(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"3-77","$type":"Article"}`))
	}}
	wr := y.serve(t)
	id, err := wr.CreateArticle(t.Context(), ytwrite.ArticleCreate{ProjectID: "0-30", Summary: "s", Content: "c <!-- hk-op:9 -->"})
	if err != nil || id != "3-77" {
		t.Fatalf("create %q %v", id, err)
	}
	id, err = wr.UpdateArticle(t.Context(), "3-77", "s2", "c2")
	if err != nil || id != "3-77" {
		t.Fatalf("update %q %v", id, err)
	}
	if y.count() != 2 || y.requests[0] != "POST /api/articles" || y.requests[1] != "POST /api/articles/3-77" {
		t.Fatalf("requests %v", y.requests)
	}
	var create struct {
		Project struct{ ID string } `json:"project"`
		Summary string              `json:"summary"`
		Content string              `json:"content"`
	}
	if err := json.Unmarshal([]byte(y.bodies[0]), &create); err != nil || create.Project.ID != "0-30" || create.Summary != "s" || create.Content != "c <!-- hk-op:9 -->" {
		t.Fatalf("create body %s %v", y.bodies[0], err)
	}
	if y.bodies[1] != `{"summary":"s2","content":"c2"}` {
		t.Fatalf("update body %s", y.bodies[1])
	}
}

// A 5xx on a write is a *ytwrite.RemoteError the protocol makes
// remote_unknown; the adapter never retries it.
func TestWriteRemoteArticleCreate5xxIsNotRetried(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}}
	_, err := y.serve(t).CreateArticle(t.Context(), ytwrite.ArticleCreate{ProjectID: "0-30", Summary: "s", Content: "c"})
	re, ok := err.(*ytwrite.RemoteError)
	if !ok || re.Status != http.StatusBadGateway || y.count() != 1 {
		t.Fatalf("err %v requests %v", err, y.requests)
	}
}

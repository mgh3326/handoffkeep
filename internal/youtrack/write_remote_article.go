package youtrack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

var _ ytwrite.ArticleRemote = (*WriteRemote)(nil)

// articleSearchTop is the most articles one FindArticles search reads; a
// page that full is not proof the search is complete.
const articleSearchTop = 100

// articleSearchFields are what the search reads of each hit: no content, so
// a page of long articles stays under the response cap.
const articleSearchFields = "id,summary,project(id),parentArticle(id)"

// articleSummaryQuery is the search for the articles of project short whose
// summary holds summary as a phrase: `project: <short> "<summary>"`, with
// the quote and the backslash escaped. The hits are checked against the
// exact summary afterwards.
func articleSummaryQuery(short, summary string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(summary)
	return "project: " + short + ` "` + escaped + `"`
}

// FindArticles searches the project's articles for summary in one request
// (article_create, hk 4206), keeps the top-level articles of projectID whose
// summary is exactly summary, and reads each one's content with one GET.
func (r *WriteRemote) FindArticles(ctx context.Context, projectShort, projectID, summary string) ([]ytwrite.RemoteArticle, error) {
	q := url.Values{}
	q.Set("query", articleSummaryQuery(projectShort, summary))
	q.Set("fields", articleSearchFields)
	q.Set("$top", strconv.Itoa(articleSearchTop))
	data, readErr, err := r.send(ctx, http.MethodGet, "/api/articles", q, nil)
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	var page []Article
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, fmt.Errorf("youtrack GET /api/articles: decode: %w", err)
	}
	if len(page) >= articleSearchTop {
		return nil, fmt.Errorf("youtrack GET /api/articles: %d or more articles match the summary; the search is not complete", articleSearchTop)
	}
	out := []ytwrite.RemoteArticle{}
	for _, a := range page {
		if a.ID == "" || a.HasParent() || a.Project == nil || a.Project.ID != projectID || a.Summary != summary {
			continue
		}
		full, err := r.ReadArticle(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return out, nil
}

// CreateArticle makes one POST /api/articles with the project, the summary
// and the content.
func (r *WriteRemote) CreateArticle(ctx context.Context, in ytwrite.ArticleCreate) (string, error) {
	body := struct {
		Summary string `json:"summary"`
		Content string `json:"content"`
		Project idRef  `json:"project"`
	}{Summary: in.Summary, Content: in.Content, Project: idRef{ID: in.ProjectID}}
	return r.articleWrite(ctx, "/api/articles", body)
}

// UpdateArticle makes one POST /api/articles/{id} with the summary and the
// content.
func (r *WriteRemote) UpdateArticle(ctx context.Context, articleID, summary, content string) (string, error) {
	return r.articleWrite(ctx, "/api/articles/"+url.PathEscape(articleID), struct {
		Summary string `json:"summary"`
		Content string `json:"content"`
	}{summary, content})
}

// articleWrite makes the one POST and reads the article id from the reply;
// a 2xx it cannot read an id from is "", nil (the write may exist).
func (r *WriteRemote) articleWrite(ctx context.Context, path string, body any) (string, error) {
	q := url.Values{}
	q.Set("fields", "id")
	data, readErr, err := r.send(ctx, http.MethodPost, path, q, body)
	if err != nil {
		return "", err
	}
	if readErr != nil {
		return "", nil
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &out) != nil {
		return "", nil
	}
	return out.ID, nil
}

// ReadArticle reads one article with its content, project and parent in one
// GET.
func (r *WriteRemote) ReadArticle(ctx context.Context, articleID string) (ytwrite.RemoteArticle, error) {
	q := url.Values{}
	q.Set("fields", articleSearchFields+",content")
	data, readErr, err := r.send(ctx, http.MethodGet, "/api/articles/"+url.PathEscape(articleID), q, nil)
	if err != nil {
		return ytwrite.RemoteArticle{}, err
	}
	if readErr != nil {
		return ytwrite.RemoteArticle{}, readErr
	}
	var a Article
	if err := json.Unmarshal(data, &a); err != nil {
		return ytwrite.RemoteArticle{}, fmt.Errorf("youtrack GET article %s: decode: %w", articleID, err)
	}
	out := ytwrite.RemoteArticle{ID: a.ID, Summary: a.Summary, Content: a.Content, Child: a.HasParent()}
	if a.Project != nil {
		out.ProjectID = a.Project.ID
	}
	return out, nil
}

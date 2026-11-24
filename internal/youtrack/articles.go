package youtrack

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

const articleFields = "id,idReadable,summary"

// Article is one 기술 자료 article. Content is markdown.
type Article struct {
	ID         string      `json:"id"`
	IDReadable string      `json:"idReadable"`
	Summary    string      `json:"summary"`
	Content    string      `json:"content,omitempty"`
	Project    *ProjectRef `json:"project,omitempty"`
	// ParentArticle is set on a child article (a split document's part);
	// read by ListArticles and ReadArticle only.
	ParentArticle *ArticleRef `json:"parentArticle,omitempty"`
	// ChildArticles is set by ReadArticle only: the article's sub-articles,
	// ids alone. The roots merge reads it to prove a duplicate root has no
	// child left before it deletes it (hk 4806).
	ChildArticles []ArticleRef `json:"childArticles,omitempty"`
	// Created is the article's creation time in epoch milliseconds (0 when the
	// field list did not ask for it). The family root lookup orders the roots
	// by it (hk 4797).
	Created int64 `json:"created,omitempty"`
	// Updated is the article's last change in epoch milliseconds (0 when the
	// field list did not ask for it). A split family read uses it to see that
	// a part did not change between its reads (hk 5744).
	Updated int64  `json:"updated,omitempty"`
	Type    string `json:"$type"`
}

// ArticleRef names another article.
type ArticleRef struct {
	ID string `json:"id"`
}

// HasParent reports whether the article is a child of another article.
func (a Article) HasParent() bool { return a.ParentArticle != nil && a.ParentArticle.ID != "" }

// articleBindingFields are what the article bindings (hk 4053) read: the
// identity, summary, project and parent of an article.
const articleBindingFields = "id,idReadable,summary,project(id,shortName,$type),parentArticle(id)"

// ListArticles lists every article the bot can see, page by page, without
// content (articleBindingFields).
func (c *Client) ListArticles(ctx context.Context) ([]Article, error) {
	return getAll[Article](ctx, c, "/api/articles", articleBindingFields)
}

// ReadArticle reads one article by id or readable id, with its content,
// project, parent and children. A missing article is a 404 (IsNotFound).
func (c *Client) ReadArticle(ctx context.Context, articleID string) (Article, error) {
	q := url.Values{}
	q.Set("fields", articleBindingFields+",content,childArticles(id)")
	var out Article
	err := c.get(ctx, "/api/articles/"+url.PathEscape(articleID), q, &out)
	return out, err
}

// ArticleWrite is one create call. ParentID empty makes a top-level article.
type ArticleWrite struct {
	ProjectID string
	ParentID  string
	Summary   string
	Content   string
}

// CreateArticle makes one article in a project. It is not retried on 5xx: a
// lost response may already have created it.
func (c *Client) CreateArticle(ctx context.Context, in ArticleWrite) (Article, error) {
	if strings.TrimSpace(in.ProjectID) == "" || strings.TrimSpace(in.Summary) == "" {
		return Article{}, fmt.Errorf("%w: project and summary are required", ErrConfig)
	}
	body := struct {
		Summary       string `json:"summary"`
		Content       string `json:"content"`
		Project       idRef  `json:"project"`
		ParentArticle *idRef `json:"parentArticle,omitempty"`
	}{Summary: in.Summary, Content: in.Content, Project: idRef{ID: in.ProjectID}}
	if in.ParentID != "" {
		body.ParentArticle = &idRef{ID: in.ParentID}
	}
	q := url.Values{}
	q.Set("fields", articleFields)
	var out Article
	err := c.postOnce(ctx, "/api/articles", q, body, &out)
	return out, err
}

// SearchArticles returns up to ten articles whose content holds query, with
// their content and project, so the caller can check both. The sync uses it to
// find an article a lost create response left behind.
func (c *Client) SearchArticles(ctx context.Context, query string) ([]Article, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("fields", "id,idReadable,summary,content,project(id,shortName,name,$type)")
	q.Set("$top", "10")
	var out []Article
	err := c.get(ctx, "/api/articles", q, &out)
	return out, err
}

// GetArticle reads one article. A deleted article answers not found.
func (c *Client) GetArticle(ctx context.Context, articleID string) (Article, error) {
	q := url.Values{}
	q.Set("fields", articleFields+",content")
	var out Article
	err := c.get(ctx, "/api/articles/"+url.PathEscape(articleID), q, &out)
	return out, err
}

// UpdateArticle rewrites an article's title and content. Same content is a
// no-op on YouTrack, so the 5xx retry is safe.
func (c *Client) UpdateArticle(ctx context.Context, articleID, summary, content string) (Article, error) {
	q := url.Values{}
	q.Set("fields", articleFields)
	var out Article
	err := c.post(ctx, "/api/articles/"+url.PathEscape(articleID), q, struct {
		Summary string `json:"summary"`
		Content string `json:"content"`
	}{summary, content}, &out)
	return out, err
}

// SetArticleParent moves an article under another one with one POST to the
// article whose body sets the parent alone. `parentArticle` is writable and
// `project` is read-only (YouTrack REST, Operations with Specific Article), so
// a root's children can be gathered under one root inside the project. The
// parent of one article is a single value, so a 5xx retry is safe.
func (c *Client) SetArticleParent(ctx context.Context, articleID, parentID string) (Article, error) {
	if strings.TrimSpace(articleID) == "" || strings.TrimSpace(parentID) == "" {
		return Article{}, fmt.Errorf("%w: article and parent are required", ErrConfig)
	}
	q := url.Values{}
	q.Set("fields", articleFields+",parentArticle(id)")
	var out Article
	err := c.post(ctx, "/api/articles/"+url.PathEscape(articleID), q, struct {
		ParentArticle idRef `json:"parentArticle"`
	}{idRef{ID: parentID}}, &out)
	return out, err
}

// DeleteArticle removes a surplus part after a document shrinks.
func (c *Client) DeleteArticle(ctx context.Context, articleID string) error {
	return c.delete(ctx, "/api/articles/"+url.PathEscape(articleID), nil)
}

type idRef struct {
	ID string `json:"id"`
}

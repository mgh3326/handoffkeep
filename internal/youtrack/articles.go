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
	Type       string      `json:"$type"`
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

// DeleteArticle removes a surplus part after a document shrinks.
func (c *Client) DeleteArticle(ctx context.Context, articleID string) error {
	return c.delete(ctx, "/api/articles/"+url.PathEscape(articleID), nil)
}

type idRef struct {
	ID string `json:"id"`
}

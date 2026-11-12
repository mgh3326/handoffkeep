package ytwritetest

import (
	"context"
	"fmt"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

var _ ytwrite.ArticleRemote = (*FakeYouTrack)(nil)

// FakeArticleWrite is one CreateArticle or UpdateArticle call the fake
// received: ID is the updated article ("" for a create).
type FakeArticleWrite struct {
	ID string
	ytwrite.ArticleCreate
}

// InjectArticle adds an article hk did not create (the import's, or one a
// person wrote) to the project and returns its id.
func (f *FakeYouTrack) InjectArticle(projectID, summary, content string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addArticleLocked(projectID, summary, content)
}

func (f *FakeYouTrack) addArticleLocked(projectID, summary, content string) string {
	f.nextID++
	id := fmt.Sprintf("3-%d", f.nextID)
	f.articles = append(f.articles, &ytwrite.RemoteArticle{ID: id, ProjectID: projectID, Summary: summary, Content: content})
	return id
}

func (f *FakeYouTrack) articleLocked(id string) *ytwrite.RemoteArticle {
	for _, a := range f.articles {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// Article returns the article as it is now.
func (f *FakeYouTrack) Article(id string) (ytwrite.RemoteArticle, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.articleLocked(id); a != nil {
		return *a, true
	}
	return ytwrite.RemoteArticle{}, false
}

// Articles returns every article of the project, in creation order.
func (f *FakeYouTrack) Articles(projectID string) []ytwrite.RemoteArticle {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ytwrite.RemoteArticle
	for _, a := range f.articles {
		if a.ProjectID == projectID {
			out = append(out, *a)
		}
	}
	return out
}

// ArticleWrites returns every create and update call the fake received, in
// order.
func (f *FakeYouTrack) ArticleWrites() []FakeArticleWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeArticleWrite(nil), f.articleWrites...)
}

// FindArticles returns every top-level article of the project with exactly
// summary, failing as QueueFind says. The short name is not checked.
func (f *FakeYouTrack) FindArticles(ctx context.Context, _, projectID, summary string) ([]ytwrite.RemoteArticle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(f.findFaults) > 0 {
		err := f.findFaults[0]
		f.findFaults = f.findFaults[1:]
		return nil, err
	}
	out := []ytwrite.RemoteArticle{}
	for _, a := range f.articles {
		if a.ProjectID == projectID && !a.Child && a.Summary == summary {
			out = append(out, *a)
		}
	}
	return out, nil
}

// CreateArticle creates one article, failing as the next queued Fault says.
func (f *FakeYouTrack) CreateArticle(ctx context.Context, in ytwrite.ArticleCreate) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.articleWrites = append(f.articleWrites, FakeArticleWrite{ArticleCreate: in})
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return f.createLocked(func() string { return f.addArticleLocked(in.ProjectID, in.Summary, in.Content) })
}

// UpdateArticle rewrites the article's summary and content, failing as the
// next queued Fault says; an unknown article is a 404.
func (f *FakeYouTrack) UpdateArticle(ctx context.Context, articleID, summary, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.articleWrites = append(f.articleWrites, FakeArticleWrite{ID: articleID, ArticleCreate: ytwrite.ArticleCreate{Summary: summary, Content: content}})
	if err := ctx.Err(); err != nil {
		return "", err
	}
	a := f.articleLocked(articleID)
	if a == nil {
		return "", &ytwrite.RemoteError{Status: http.StatusNotFound, Message: "article not found"}
	}
	return f.createLocked(func() string {
		a.Summary, a.Content = summary, content
		return a.ID
	})
}

// ReadArticle returns the article as it is now, failing as QueueRead says;
// an unknown article is a 404.
func (f *FakeYouTrack) ReadArticle(ctx context.Context, articleID string) (ytwrite.RemoteArticle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := ctx.Err(); err != nil {
		return ytwrite.RemoteArticle{}, err
	}
	if len(f.readFaults) > 0 {
		err := f.readFaults[0]
		f.readFaults = f.readFaults[1:]
		return ytwrite.RemoteArticle{}, err
	}
	a := f.articleLocked(articleID)
	if a == nil {
		return ytwrite.RemoteArticle{}, &ytwrite.RemoteError{Status: http.StatusNotFound, Message: "article not found"}
	}
	return *a, nil
}

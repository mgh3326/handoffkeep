package ytwritetest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

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

// legacyIndexID is the parent of the children InjectChildArticle adds: the
// legacy sync's per-prefix index article. The fake never issues "3-0".
const legacyIndexID = "3-0"

// InjectChildArticle adds a child article (one under a parent, as the legacy
// sync put every document under its prefix's index) and returns its id.
func (f *FakeYouTrack) InjectChildArticle(projectID, summary, content string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.addArticleLocked(projectID, summary, content)
	f.articleLocked(id).ParentID = legacyIndexID
	return id
}

// InjectChildOf adds an article under parentID, as a person would add a child
// to an hk index, and returns its id.
func (f *FakeYouTrack) InjectChildOf(parentID, projectID, summary, content string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.addArticleLocked(projectID, summary, content)
	f.articleLocked(id).ParentID = parentID
	return id
}

func (f *FakeYouTrack) addArticleLocked(projectID, summary, content string) string {
	f.nextID++
	id := fmt.Sprintf("3-%d", f.nextID)
	a := &ytwrite.RemoteArticle{ID: id, ProjectID: projectID, Summary: summary, Content: content,
		CreatedAt: time.UnixMilli(int64(f.nextID)).UTC()}
	f.touchLocked(a)
	f.articles = append(f.articles, a)
	return id
}

// touchLocked gives a its next revision: every change to an article's text
// moves UpdatedAt, a restore to the same text included.
func (f *FakeYouTrack) touchLocked(a *ytwrite.RemoteArticle) {
	f.articleRevs++
	a.UpdatedAt = time.UnixMilli(f.articleRevs).UTC()
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

// Searches returns every FindArticles search the fake received, in order.
func (f *FakeYouTrack) Searches() []ytwrite.ArticleSearch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ytwrite.ArticleSearch(nil), f.searches...)
}

// FindArticles returns every article of the project whose summary or content
// holds the search's Query and whose summary is exactly its Summary and whose
// parent is its ParentID ("" for top-level), failing as QueueFind says or, for
// a Summary FailFindsForSummary names, every time. The short name is not
// checked.
func (f *FakeYouTrack) FindArticles(ctx context.Context, s ytwrite.ArticleSearch) ([]ytwrite.RemoteArticle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds++
	f.searches = append(f.searches, s)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.findFail != nil && f.findFailSummary == s.Summary {
		return nil, f.findFail
	}
	if len(f.findFaults) > 0 {
		err := f.findFaults[0]
		f.findFaults = f.findFaults[1:]
		return nil, err
	}
	out := []ytwrite.RemoteArticle{}
	for _, a := range f.articles {
		if a.ProjectID != s.ProjectID || a.ParentID != s.ParentID || a.Summary != s.Summary {
			continue
		}
		if !strings.Contains(a.Summary, s.Query) && !strings.Contains(a.Content, s.Query) {
			continue
		}
		out = append(out, *a)
	}
	return out, nil
}

// CreateArticle creates one article, failing as the next queued Fault says.
func (f *FakeYouTrack) CreateArticle(ctx context.Context, in ytwrite.ArticleCreate) (string, error) {
	if f.BeforeArticleCreate != nil {
		f.BeforeArticleCreate(ctx, in)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.articleWrites = append(f.articleWrites, FakeArticleWrite{ArticleCreate: in})
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return f.createLocked(func() string {
		id := f.addArticleLocked(in.ProjectID, in.Summary, in.Content)
		f.articleLocked(id).ParentID = in.ParentID
		return id
	})
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
		f.touchLocked(a)
		return a.ID
	})
}

// ReadArticle returns the article as it is now, failing as QueueRead says;
// an unknown article is a 404.
func (f *FakeYouTrack) ReadArticle(ctx context.Context, articleID string) (ytwrite.RemoteArticle, error) {
	if h := f.BeforeReadArticle; h != nil {
		h(articleID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := ctx.Err(); err != nil {
		return ytwrite.RemoteArticle{}, err
	}
	if len(f.readFaults) > 0 {
		err := f.readFaults[0]
		f.readFaults = f.readFaults[1:]
		if err != nil { // a nil entry lets one read through
			return ytwrite.RemoteArticle{}, err
		}
	}
	a := f.articleLocked(articleID)
	if a == nil {
		return ytwrite.RemoteArticle{}, &ytwrite.RemoteError{Status: http.StatusNotFound, Message: "article not found"}
	}
	out := *a
	out.ChildIDs = nil
	for _, c := range f.articles {
		if c.ParentID == a.ID {
			out.ChildIDs = append(out.ChildIDs, c.ID)
		}
	}
	return out, nil
}

// EditArticle sets an article's content as a person would, outside hk.
func (f *FakeYouTrack) EditArticle(id, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.articleLocked(id); a != nil {
		a.Content = content
		f.touchLocked(a)
	}
}

// SetArticleSummary renames an article as a person would.
func (f *FakeYouTrack) SetArticleSummary(id, summary string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.articleLocked(id); a != nil {
		a.Summary = summary
		f.touchLocked(a)
	}
}

// MoveArticle moves an article and its descendants to another project, as
// YouTrack moves a sub-tree.
func (f *FakeYouTrack) MoveArticle(id, projectID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	moved := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for _, a := range f.articles {
			if (moved[a.ID] || moved[a.ParentID]) && a.ProjectID != projectID {
				a.ProjectID, moved[a.ID], changed = projectID, true, true
			}
		}
	}
}

// DeleteArticle removes an article as a person would.
func (f *FakeYouTrack) DeleteArticle(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.articles {
		if a.ID == id {
			f.articles = append(f.articles[:i], f.articles[i+1:]...)
			return
		}
	}
}

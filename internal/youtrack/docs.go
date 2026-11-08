package youtrack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// docPrefixes are the hk key prefixes mirrored into YouTrack 기술 자료 now:
// the documents the operator reads. Agent-read prefixes (policy, brief,
// reference, reports, deploy) stay in hk.
var docPrefixes = []string{"design", "decision", "runbook", "research", "review", "experiment"}

// docArticleMaxBytes caps one article body. A larger document gets a short
// stub that links back to hk, so one big document cannot use the budget.
const docArticleMaxBytes = 256 << 10

// docTitleMaxRunes keeps an article title inside YouTrack's summary limit.
const docTitleMaxRunes = 200

// DocAction is one document the run considered. Action is create, update,
// adopt (an article a lost create response left behind, found by its marker
// and updated), stub (over the cap; the article holds only the link), skipped
// (no YouTrack project for its task), or would_create / would_update in
// dry-run mode.
type DocAction struct {
	Key       string `json:"key"`
	HKProject string `json:"hk_project,omitempty"`
	ArticleID string `json:"article_id,omitempty"`
	Action    string `json:"action"`
	Bytes     int64  `json:"bytes"`
}

// syncDocs mirrors one bounded batch of due documents. A document is due when
// it has no article, its sha256 changed since the last write, or its project
// changed, so an unchanged document costs no YouTrack call. A document that
// failed or was skipped waits behind the others (see recordAttempt). Deletes
// are logged, not applied.
func (s *Service) syncDocs(ctx context.Context, opts SyncOptions, report *SyncReport) {
	patterns := make([]string, 0, len(docPrefixes))
	for _, prefix := range docPrefixes {
		patterns = append(patterns, prefix+"/%")
	}
	docs, err := s.store.DueYouTrackDocs(ctx, patterns, s.project, opts.DocLimit)
	if err != nil {
		report.fail("docs: %v", err)
		return
	}
	for _, doc := range docs {
		action, err := s.pushDoc(ctx, doc, opts.DryRun)
		report.Docs = append(report.Docs, action)
		if err != nil {
			report.fail("doc %s: %v", doc.Key, err)
			s.recordAttempt(ctx, opts, store.YouTrackQueueDoc, doc.Key, err.Error(), "", report)
			continue
		}
		if action.Action == "skipped" {
			s.recordAttempt(ctx, opts, store.YouTrackQueueDoc, doc.Key, "", store.YouTrackSkipProjectMissing, report)
		} else {
			s.recordAttempt(ctx, opts, store.YouTrackQueueDoc, doc.Key, "", "", report)
			if !opts.DryRun {
				report.DocBytes += action.Bytes
			}
		}
	}
	orphans, err := s.store.OrphanYouTrackDocArticles(ctx, opts.DocLimit)
	if err != nil {
		report.fail("doc orphans: %v", err)
		return
	}
	for _, o := range orphans {
		report.warn("doc %s: hk 문서가 삭제되었습니다. YouTrack 기술 자료 %s은 그대로 둡니다", o.DocKey, o.ArticleID)
	}
}

func (s *Service) pushDoc(ctx context.Context, doc store.Document, dry bool) (DocAction, error) {
	action := DocAction{Key: doc.Key}
	hkProject := ""
	if doc.TaskID != nil {
		task, found, err := s.store.GetTask(ctx, *doc.TaskID)
		if err != nil {
			return action, err
		}
		if found && task.Project != nil {
			hkProject = *task.Project
		}
	}
	project, projectID, err := s.projectForTask(ctx, hkProject, !dry)
	if err != nil {
		if errors.Is(err, ErrProjectMissing) {
			action.Action = "skipped"
			return action, nil
		}
		return action, err
	}
	action.HKProject = project.HKProject
	content, oversize := s.docContent(doc)
	action.Bytes = int64(len(content))
	title := docTitle(doc)
	existing, found, err := s.store.GetYouTrackDocArticle(ctx, doc.Key)
	if err != nil {
		return action, err
	}
	if found && existing.HKProject != project.HKProject {
		// The task moved to another project. The article stays where it is,
		// and the document gets a new one in the project it now belongs to.
		found = false
	}
	if dry {
		action.Action = "would_create"
		if found {
			action.Action, action.ArticleID = "would_update", existing.ArticleID
		}
		return action, nil
	}
	parentID, err := s.docIndex(ctx, docPrefix(doc.Key), project.HKProject, projectID)
	if err != nil {
		return action, err
	}
	var article Article
	if found {
		article, err = s.client.UpdateArticle(ctx, existing.ArticleID, title, content)
		if IsNotFound(err) {
			// The article was deleted in YouTrack. Write a new one; the ledger
			// row is replaced below.
			found = false
		} else if err != nil {
			return action, err
		} else {
			action.Action = "update"
		}
	}
	if !found {
		// A create whose response was lost may already hold this document, so
		// adopt the article that carries its marker before creating another.
		marked, ok, err := s.findMarkedArticle(ctx, projectID, doc.Key)
		if err != nil {
			return action, err
		}
		if ok {
			article, err = s.client.UpdateArticle(ctx, marked.ID, title, content)
			if err != nil {
				return action, err
			}
			action.Action = "adopt"
		} else {
			article, err = s.client.CreateArticle(ctx, ArticleWrite{ProjectID: projectID, ParentID: parentID, Summary: title, Content: content})
			if err != nil {
				return action, err
			}
			action.Action = "create"
		}
	}
	if oversize {
		action.Action = "stub"
	}
	action.ArticleID = article.ID
	err = s.store.UpsertYouTrackDocArticle(ctx, store.YouTrackDocArticle{DocKey: doc.Key, HKProject: project.HKProject, ArticleID: article.ID, SourceSHA: doc.SHA256, Bytes: action.Bytes, SyncedAt: s.now()})
	return action, err
}

// docIndex returns the parent article for one prefix in one hk project,
// creating it the first time. Articles are grouped under it by prefix.
func (s *Service) docIndex(ctx context.Context, prefix, hkProject, projectID string) (string, error) {
	key := "index:" + prefix + ":" + hkProject
	row, found, err := s.store.GetYouTrackDocArticle(ctx, key)
	if err != nil {
		return "", err
	}
	if found {
		// The ledger row outlives the article when someone deletes it in
		// YouTrack; every document under a deleted parent would then fail.
		// A deleted parent is written again below and the row replaced.
		_, gerr := s.client.GetArticle(ctx, row.ArticleID)
		if gerr == nil {
			return row.ArticleID, nil
		}
		if !IsNotFound(gerr) {
			return "", gerr
		}
	}
	// As for a document, a lost create response may have left the index behind.
	marked, ok, err := s.findMarkedArticle(ctx, projectID, key)
	if err != nil {
		return "", err
	}
	if ok {
		err = s.store.UpsertYouTrackDocArticle(ctx, store.YouTrackDocArticle{DocKey: key, HKProject: hkProject, ArticleID: marked.ID, SyncedAt: s.now()})
		return marked.ID, err
	}
	article, err := s.client.CreateArticle(ctx, ArticleWrite{
		ProjectID: projectID,
		Summary:   prefix,
		Content:   docMarker(key) + "\n" + fmt.Sprintf("hk의 `%s/` 문서를 모은 목록입니다. 내용은 hk에서 관리하며 여기서 고치지 마세요.", prefix),
	})
	if err != nil {
		return "", err
	}
	err = s.store.UpsertYouTrackDocArticle(ctx, store.YouTrackDocArticle{DocKey: key, HKProject: hkProject, ArticleID: article.ID, SyncedAt: s.now()})
	return article.ID, err
}

// docMarkerToken is the search word for one ledger key: a document key or an
// index key. It is one token, so a full-text search can find it.
func docMarkerToken(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "hkdoc" + hex.EncodeToString(sum[:12])
}

// docMarker is the line that ties an article body to its ledger key.
func docMarker(key string) string {
	return "<!-- hk-marker:" + docMarkerToken(key) + " -->"
}

// findMarkedArticle returns the article in one YouTrack project whose body
// carries the marker of a ledger key. An article in another project never
// matches, because a moved document keeps its old article where it is.
func (s *Service) findMarkedArticle(ctx context.Context, projectID, key string) (Article, bool, error) {
	marker := docMarker(key)
	hits, err := s.client.SearchArticles(ctx, docMarkerToken(key))
	if err != nil {
		return Article{}, false, err
	}
	for _, hit := range hits {
		if hit.Project != nil && hit.Project.ID == projectID && strings.Contains(hit.Content, marker) {
			return hit, true, nil
		}
	}
	return Article{}, false, nil
}

// docContent is the article body: a header with the hk key, the date and a
// console link, then the document's markdown as is. The second result is
// true when the body was over the cap and only the stub was written.
func (s *Service) docContent(doc store.Document) (string, bool) {
	link := strings.TrimRight(s.console, "/") + "/ui/doc/" + doc.Key
	header := docMarker(doc.Key) + "\n" + fmt.Sprintf("> hk 문서 `%s` · 작성 %s · [hk에서 보기](%s)\n\n", doc.Key, doc.CreatedAt.UTC().Format("2006-01-02"), link)
	if len(doc.Body) > docArticleMaxBytes {
		return header + fmt.Sprintf("이 문서(%d bytes)는 한도(%d bytes)를 넘어 여기에 옮기지 않았습니다. hk에서 읽어 주세요.\n", len(doc.Body), docArticleMaxBytes), true
	}
	return header + doc.Body, false
}

// docTitle is the document's first level-one heading, falling back to its key.
func docTitle(doc store.Document) string {
	title := doc.Key
	for _, line := range strings.Split(doc.Body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			if h := strings.TrimSpace(strings.TrimPrefix(t, "# ")); h != "" {
				title = h
				break
			}
		}
	}
	if r := []rune(title); len(r) > docTitleMaxRunes {
		title = string(r[:docTitleMaxRunes])
	}
	return title
}

func docPrefix(key string) string {
	prefix, _, _ := strings.Cut(key, "/")
	return prefix
}

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

// docArticleMaxBytes is the per-part cap. JetBrains does not publish an article
// content limit; keep the established 256 KiB cap for compatibility.
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
	Parts     int    `json:"parts"`
}

// syncDocs mirrors one bounded batch of due documents. A document is due when
// it has no article, its sha256 changed since the last write, or its project
// changed, so an unchanged document costs no YouTrack call. A document that
// failed or was skipped waits behind the others (see recordAttempt). Surplus
// article parts are deleted when a document shrinks; document deletions remain
// logged for operator review.
func (s *Service) syncDocs(ctx context.Context, opts SyncOptions, report *SyncReport) {
	patterns := []string{"%"}
	var docs []store.Document
	var err error
	if opts.DryRun {
		docs, err = s.store.AllYouTrackDocs(ctx, opts.DocLimit)
	} else {
		docs, err = s.store.DueYouTrackDocs(ctx, patterns, s.project, opts.DocLimit)
	}
	if err != nil {
		report.fail("docs: %v", err)
		return
	}
	for _, doc := range docs {
		action, err := s.pushDoc(ctx, doc, opts.DryRun)
		report.Docs = append(report.Docs, action)
		prefix := docPrefix(doc.Key)
		if report.DocPrefixes == nil {
			report.DocPrefixes = map[string]DocPrefixSummary{}
		}
		summary := report.DocPrefixes[prefix]
		switch action.Action {
		case "create", "adopt", "would_create":
			summary.Create++
		case "update", "would_update":
			summary.Update++
		case "unchanged":
			summary.Unchanged++
		}
		summary.Parts += action.Parts
		summary.Bytes += action.Bytes
		report.DocPrefixes[prefix] = summary
		if err != nil {
			report.fail("doc %s: %v", doc.Key, err)
			s.recordAttempt(ctx, opts, store.YouTrackQueueDoc, doc.Key, err.Error(), "", report)
			continue
		}
		if action.Action == "skipped" {
			s.recordAttempt(ctx, opts, store.YouTrackQueueDoc, doc.Key, "", store.YouTrackSkipProjectMissing, report)
		} else {
			s.recordAttempt(ctx, opts, store.YouTrackQueueDoc, doc.Key, "", "", report)
			report.DocBytes += action.Bytes
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
	content := s.docContent(doc)
	action.Bytes = int64(len(content))
	parts := docArticleParts(content, doc.Key)
	action.Parts = len(parts)
	title := docTitle(doc)
	existingRows, err := s.store.YouTrackDocArticles(ctx, doc.Key)
	if err != nil {
		return action, err
	}
	existing, found := store.YouTrackDocArticle{}, len(existingRows) > 0
	if found {
		existing = existingRows[0]
	}
	if found && existing.HKProject != project.HKProject {
		// The task moved to another project. The article stays where it is,
		// and the document gets a new one in the project it now belongs to.
		found = false
	}
	if dry {
		action.Action = "would_create"
		if found {
			action.ArticleID = existing.ArticleID
			if completeDocLedger(existingRows, parts, doc.SHA256, project.HKProject) {
				action.Action = "unchanged"
			} else {
				action.Action = "would_update"
			}
		}
		return action, nil
	}
	parentID, err := s.docIndex(ctx, docPrefix(doc.Key), project.HKProject, projectID)
	if err != nil {
		return action, err
	}
	if found && completeDocLedger(existingRows, parts, doc.SHA256, project.HKProject) {
		action.Action = "unchanged"
		action.ArticleID = existing.ArticleID
		return action, nil
	}
	if found {
		action.Action = "update"
	}
	for i, part := range parts {
		ledgerKey := docPartLedgerKey(doc.Key, i+1)
		markerKey := docPartMarkerKey(doc.Key, i+1)
		var article Article
		row, rowFound, e := s.store.GetYouTrackDocArticle(ctx, ledgerKey)
		if e != nil {
			return action, e
		}
		partTitle := title
		if i > 0 {
			partTitle = docPartTitle(doc.Key, i+1, len(parts))
		}
		if rowFound && row.HKProject == project.HKProject {
			article, e = s.client.UpdateArticle(ctx, row.ArticleID, partTitle, part)
			if IsNotFound(e) {
				rowFound = false
			} else if e != nil {
				return action, e
			}
		} else if rowFound {
			rowFound = false
		}
		if !rowFound {
			marked, ok, e := s.findMarkedArticle(ctx, projectID, markerKey)
			if e != nil {
				return action, e
			}
			if ok {
				article, e = s.client.UpdateArticle(ctx, marked.ID, partTitle, part)
				if e == nil && action.Action == "" {
					action.Action = "adopt"
				}
			} else {
				parent := parentID
				if i > 0 {
					parent = action.ArticleID
				}
				article, e = s.client.CreateArticle(ctx, ArticleWrite{ProjectID: projectID, ParentID: parent, Summary: partTitle, Content: part})
				if e == nil && action.Action == "" {
					action.Action = "create"
				}
			}
			if e != nil {
				return action, e
			}
		}
		if i == 0 {
			action.ArticleID = article.ID
		}
		ledgerEntry := store.YouTrackDocArticle{DocKey: ledgerKey, DocumentKey: doc.Key, PartIndex: i + 1, PartCount: len(parts), PartSHA: docPartSHA(doc.SHA256, part), HKProject: project.HKProject, ArticleID: article.ID, SourceSHA: doc.SHA256, Bytes: int64(len(part)), SyncedAt: s.now()}
		if i == 0 {
			existing = ledgerEntry // commit the source SHA only after all child work succeeds
		} else if e = s.store.UpsertYouTrackDocArticle(ctx, ledgerEntry); e != nil {
			return action, e
		}
	}
	for _, row := range existingRows {
		if row.HKProject == project.HKProject && row.PartIndex > len(parts) {
			if e := s.client.DeleteArticle(ctx, row.ArticleID); e != nil && !IsNotFound(e) {
				return action, e
			}
		}
	}
	if err = s.store.DeleteYouTrackDocArticleParts(ctx, doc.Key, len(parts)+1); err != nil {
		return action, err
	}
	if err = s.store.UpsertYouTrackDocArticle(ctx, existing); err != nil {
		return action, err
	}
	return action, nil
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
		Content:   docMarker(key) + "\n" + fmt.Sprintf("`%s/` 문서를 찾아 읽는 목록입니다. 문서 본문은 hk의 작성 내용이 동기화되며, 변경은 hk에서 작성해 주세요.", prefix),
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

// docContent is the article body: a header with the hk key, date and console link, then the Markdown.
func (s *Service) docContent(doc store.Document) string {
	link := strings.TrimRight(s.console, "/") + "/ui/doc/" + doc.Key
	header := docMarker(doc.Key) + "\n" + fmt.Sprintf("> hk 문서 `%s` · 작성 %s · [hk에서 보기](%s)\n\n", doc.Key, doc.CreatedAt.UTC().Format("2006-01-02"), link)
	return header + doc.Body
}

func docArticleParts(content, key string) []string {
	parts := splitDocParts(content, docArticleMaxBytes)
	for i := 1; i < len(parts); i++ {
		parts[i] = docMarker(docPartMarkerKey(key, i+1)) + "\n" + parts[i]
	}
	return parts
}

func docPartSHA(sourceSHA, content string) string {
	sum := sha256.Sum256([]byte("youtrack-doc-part-v2\x00" + sourceSHA + "\x00" + content))
	return "v2:" + hex.EncodeToString(sum[:])
}

func docPartTitle(key string, part, total int) string {
	suffix := fmt.Sprintf(" (%d/%d)", part, total)
	runes := []rune(key)
	keep := docTitleMaxRunes - len([]rune(suffix))
	if keep < 0 {
		keep = 0
	}
	if len(runes) > keep {
		runes = runes[:keep]
	}
	return string(runes) + suffix
}

func completeDocLedger(rows []store.YouTrackDocArticle, parts []string, sourceSHA, project string) bool {
	if len(rows) != len(parts) {
		return false
	}
	for i, row := range rows {
		if row.PartIndex != i+1 || row.PartCount != len(parts) || row.SourceSHA != sourceSHA || row.PartSHA != docPartSHA(sourceSHA, parts[i]) || row.HKProject != project {
			return false
		}
	}
	return true
}

func docPartLedgerKey(key string, part int) string {
	if part == 1 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("/hk-part/%s/%d", hex.EncodeToString(sum[:]), part)
}
func docPartMarkerKey(key string, part int) string {
	return docPartLedgerKey(key, part)
}

func splitDocParts(content string, limit int) []string {
	limit -= 2048 // room for markers and the maximum document key in the header
	if len(content) <= limit {
		return []string{content}
	}
	var parts []string
	for len(content) > limit {
		end := limit
		for end > 0 && end < len(content) && (content[end]&0xc0) == 0x80 {
			end--
		}
		if end == 0 {
			end = limit
		}
		// Prefer a paragraph boundary in the latter half of the chunk.
		if n := strings.LastIndex(content[:end], "\n\n"); n >= limit/2 {
			end = n + 2
		}
		parts = append(parts, content[:end])
		content = content[end:]
	}
	return append(parts, content)
}

// ReadDocumentArticle reassembles stored article bodies in their original byte order.
func (s *Service) ReadDocumentArticle(ctx context.Context, key string) (string, error) {
	rows, err := s.store.YouTrackDocArticles(ctx, key)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("document %q has no YouTrack articles", key)
	}
	if rows[0].PartIndex != 1 || rows[0].PartCount != len(rows) {
		return "", fmt.Errorf("document %q has incomplete article parts", key)
	}
	var b strings.Builder
	for i, row := range rows {
		if row.PartIndex != i+1 || row.PartCount != rows[0].PartCount || row.SourceSHA != rows[0].SourceSHA {
			return "", fmt.Errorf("document %q has missing part %d", key, i+1)
		}
		a, e := s.client.GetArticle(ctx, row.ArticleID)
		if e != nil {
			return "", e
		}
		if strings.HasPrefix(row.PartSHA, "v2:") && docPartSHA(row.SourceSHA, a.Content) != row.PartSHA {
			return "", fmt.Errorf("document %q part %d content does not match its ledger", key, i+1)
		}
		part := strings.TrimPrefix(a.Content, docMarker(docPartMarkerKey(key, i+1))+"\n")
		b.WriteString(part)
	}
	content := b.String()
	marker := docMarker(key) + "\n"
	content = strings.TrimPrefix(content, marker)
	if i := strings.Index(content, "\n\n"); i >= 0 {
		content = content[i+2:]
	}
	return content, nil
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

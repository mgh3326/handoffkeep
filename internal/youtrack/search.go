package youtrack

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// DefaultSearchTimeout bounds the YouTrack half of one search. A search that
// runs past it answers from PostgreSQL instead.
const DefaultSearchTimeout = 3 * time.Second

const (
	searchIssueFields   = "id,idReadable,summary,description"
	searchArticleFields = "id,idReadable,summary,content,parentArticle(id)"
	searchExcerptRunes  = 160
	searchDefaultLimit  = 20
	searchMaxLimit      = 100
	searchMaxTaskLimit  = 50
)

// SearchIssues returns up to top issues matching a YouTrack query. A 5xx is not
// retried: a search answers within its own deadline, and PostgreSQL answers
// when YouTrack does not.
func (c *Client) SearchIssues(ctx context.Context, query string, top int) ([]Issue, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("fields", searchIssueFields)
	q.Set("$top", strconv.Itoa(top))
	var out []Issue
	err := c.do(ctx, http.MethodGet, "/api/issues", q, nil, &out, false)
	return out, err
}

// SearchArticleText returns up to top 기술 자료 articles whose text matches a
// YouTrack query, with their content for the excerpt. A 5xx is not retried,
// for the same reason as SearchIssues.
func (c *Client) SearchArticleText(ctx context.Context, query string, top int) ([]Article, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("fields", searchArticleFields)
	q.Set("$top", strconv.Itoa(top))
	var out []Article
	err := c.do(ctx, http.MethodGet, "/api/articles", q, nil, &out, false)
	return out, err
}

// Search answers one /v1/search call from YouTrack exclusively.
// Exact document keys and numeric ID lookups (#<id> or <id>) are answered
// directly by exact store lookups without querying YouTrack.
// Scope "ctx" searches checkpoints and memory via SearchCtx.
//
// Scope all merges three lanes round-robin, one hit from each non-empty lane in
// turn: YouTrack issues (tasks), YouTrack articles (docs), and hk ctx
// newest first. Each lane keeps its own order, so the YouTrack lanes
// keep YouTrack's relevance order. Scopes tasks and docs keep YouTrack's order.
//
// Each YouTrack request asks for limit+1 hits. The page is truncated when a
// lane returned more than limit, even if some of those hits had no hk mapping,
// or when the merged list is longer than limit. A truncated page sets Truncated
// on every row.
func (s *Service) Search(ctx context.Context, q, scope, session string, limit int) ([]store.SearchResult, error) {
	if err := store.CheckSearch(q, scope, session); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(q)
	limit = searchLimit(scope, limit)

	if scope == "ctx" {
		return s.store.SearchCtx(ctx, text, session, limit)
	}

	if store.NumericIDQuery(text) {
		hits, ok, err := s.store.ExactNumericHit(ctx, text, scope, session)
		if err != nil {
			return nil, err
		}
		if ok {
			if len(hits) > limit {
				hits = hits[:limit]
				for i := range hits {
					hits[i].Truncated = true
				}
			}
			return hits, nil
		}
	}

	wantTasks := scope == "tasks" || scope == "all"
	wantDocs := scope == "docs" || scope == "all"

	if wantDocs {
		hits, ok, err := s.store.ExactDocHit(ctx, text, session)
		if err != nil {
			return nil, err
		}
		if ok {
			if len(hits) > limit {
				hits = hits[:limit]
				for i := range hits {
					hits[i].Truncated = true
				}
			}
			return hits, nil
		}
	}

	var names []string
	if wantTasks {
		projects, err := s.store.ListYouTrackProjects(ctx)
		if err != nil {
			return nil, err
		}
		names = mirroredShortNames(projects)
	}

	yctx, cancel := context.WithTimeout(ctx, s.searchTimeout)
	defer cancel()
	var issues []Issue
	var articles []Article
	var err error
	timedOut := false
	if len(names) > 0 {
		issues, err = s.client.SearchIssues(yctx, issueSearchQuery(names, text), limit+1)
	}
	if err == nil && wantDocs {
		clean := cleanSearchText(text)
		if clean != "" {
			articles, err = s.client.SearchArticleText(yctx, clean, limit+1)
		}
	}
	if err != nil {
		log.Printf("search.youtrack_error reason=%s scope=%q err=%v", searchFailureReason(yctx, err), scope, err)
		if errors.Is(yctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			if scope != "all" || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, store.ErrSearchTimeout
			}
			// Scope all has independent local context results. Keep any YouTrack
			// lane that completed before the shared deadline and mark the page.
			timedOut = true
			err = nil
		} else {
			return nil, store.ErrSearchUnavailable
		}
	}

	var lanes [][]store.SearchResult
	overflow := false
	if wantTasks {
		overflow = overflow || len(issues) > limit
		hits, err := s.store.YouTrackTaskHits(ctx, s.tenant, issueIDs(issues), session)
		if err != nil {
			return nil, err
		}
		lane := []store.SearchResult{}
		for _, issue := range issues {
			x, ok := hits[issue.ID]
			if !ok {
				continue
			}
			x.Title = issue.Summary
			x.Snippet = excerpt(issue.Description, issue.Summary)
			lane = append(lane, x)
		}
		lanes = append(lanes, lane)
	}
	if wantDocs {
		overflow = overflow || len(articles) > limit
		allArticleIDs := articleIDs(articles)
		for _, a := range articles {
			if a.HasParent() {
				allArticleIDs = append(allArticleIDs, a.ParentArticle.ID)
			}
		}
		hits, err := s.store.YouTrackDocHits(ctx, allArticleIDs, session)
		if err != nil {
			return nil, err
		}
		lane := []store.SearchResult{}
		for _, article := range articles {
			x, ok := hits[article.ID]
			if !ok && article.HasParent() {
				x, ok = hits[article.ParentArticle.ID]
			}
			if !ok {
				continue
			}
			x.Snippet = excerpt(article.Content, article.Summary)
			lane = append(lane, x)
		}
		lanes = append(lanes, lane)
	}
	if scope == "all" {
		ctxHits, err := s.store.SearchCtx(ctx, text, session, limit+1)
		if err != nil {
			return nil, err
		}
		overflow = overflow || len(ctxHits) > limit
		lanes = append(lanes, ctxHits)
	}
	merged := interleave(lanes)
	truncated := overflow || len(merged) > limit
	if len(merged) > limit {
		merged = merged[:limit]
	}
	for i := range merged {
		merged[i].Truncated = truncated
		merged[i].Partial = timedOut
		merged[i].TimedOut = timedOut
	}
	return merged, nil
}

// searchLimit applies the same defaults and caps as the PostgreSQL search:
// 20 by default, 100 at most, and 50 for scope tasks.
func searchLimit(scope string, limit int) int {
	switch {
	case limit < 1:
		return searchDefaultLimit
	case scope == "tasks" && limit > searchMaxTaskLimit:
		return searchMaxTaskLimit
	case limit > searchMaxLimit:
		return searchMaxLimit
	}
	return limit
}

// searchFailureReason names why YouTrack did not answer, for the fallback log.
// It reads the search's own deadline, because a transport error from a
// deadline does not wrap context.DeadlineExceeded.
func searchFailureReason(yctx context.Context, err error) string {
	switch {
	case errors.Is(yctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrResponseTooLarge):
		return "oversize"
	case apiStatus(err) != 0:
		return "http_" + strconv.Itoa(apiStatus(err))
	}
	return "error"
}

// mirroredShortNames are the YouTrack short names of the mirrored projects. A
// name with braces is skipped, because YouTrack's {name} form cannot hold one.
func mirroredShortNames(projects []store.YouTrackProject) []string {
	names := []string{}
	for _, p := range projects {
		name := strings.TrimSpace(p.ShortName)
		if name == "" || strings.ContainsAny(name, "{}") {
			continue
		}
		names = append(names, name)
	}
	return names
}

var leadingConjunctionRE = regexp.MustCompile(`^(?i)(?:or|and)\s+`)

var querySyntaxReplacer = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	`{`, `\{`,
	`}`, `\}`,
	`(`, `\(`,
	`)`, `\)`,
	`#`, `\#`,
	`:`, `\:`,
)

// escapeYouTrackQuery escapes YouTrack query syntax characters (backslash, quotes,
// braces, parentheses, hash, colons) so user text matches literally while words
// still match individually, preventing query syntax errors and filter injections.
func escapeYouTrackQuery(text string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	s := strings.Join(words, " ")
	escaped := querySyntaxReplacer.Replace(s)
	return strings.TrimSpace(leadingConjunctionRE.ReplaceAllString(escaped, ""))
}

// cleanSearchText escapes YouTrack query syntax characters so user text matches
// literally while words still match individually.
func cleanSearchText(text string) string {
	return escapeYouTrackQuery(text)
}

// issueSearchQuery restricts the issue search to the mirrored projects using
// an isolated project clause, combined with the cleaned user text tokens.
func issueSearchQuery(names []string, text string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, "project: {"+name+"}")
	}
	clean := cleanSearchText(text)
	if len(parts) == 0 {
		return clean
	}
	projClause := strings.Join(parts, " or ")
	if clean == "" {
		return "(" + projClause + ")"
	}
	return "(" + projClause + ") and (" + clean + ")"
}

// excerpt is the first searchExcerptRunes runes of the body with whitespace
// collapsed, or of the fallback when the body is empty.
func excerpt(body, fallback string) string {
	text := strings.Join(strings.Fields(body), " ")
	if text == "" {
		text = strings.Join(strings.Fields(fallback), " ")
	}
	runes := []rune(text)
	if len(runes) > searchExcerptRunes {
		return string(runes[:searchExcerptRunes])
	}
	return text
}

func issueIDs(issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, issue := range issues {
		out = append(out, issue.ID)
	}
	return out
}

func articleIDs(articles []Article) []string {
	out := make([]string, 0, len(articles))
	for _, article := range articles {
		out = append(out, article.ID)
	}
	return out
}

// interleave merges the lanes round-robin, one hit from each non-empty lane in
// turn. Each lane keeps its own order.
func interleave(lanes [][]store.SearchResult) []store.SearchResult {
	out := []store.SearchResult{}
	next := make([]int, len(lanes))
	for {
		moved := false
		for i, lane := range lanes {
			if next[i] < len(lane) {
				out = append(out, lane[next[i]])
				next[i]++
				moved = true
			}
		}
		if !moved {
			return out
		}
	}
}

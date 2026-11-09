package youtrack

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
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
	searchArticleFields = "id,idReadable,summary,content"
	searchExcerptRunes  = 160
	searchDefaultLimit  = 20
	searchMaxLimit      = 100
	searchMaxTaskLimit  = 50
)

// searchQuoteEscaper keeps user text inside one YouTrack quoted phrase: a
// backslash or a quote the user typed cannot end the phrase early.
var searchQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// SearchFallback answers a search from PostgreSQL. The api passes Store.Search.
type SearchFallback func(ctx context.Context, q, scope, session string, limit int) ([]store.SearchResult, error)

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

// Search answers one /v1/search call. Scope tasks and docs ask YouTrack, and
// scope all asks it for both and adds PostgreSQL's ctx and memory hits. Every
// other request goes to fallback: scope ctx or memory, a bare id or "#<id>",
// an exact document key, and any YouTrack failure (timeout, HTTP error,
// oversize answer). A YouTrack failure is logged once as
// search.youtrack_fallback with its reason.
//
// Scope all merges three lanes round-robin, one hit from each non-empty lane in
// turn: YouTrack issues (tasks), YouTrack articles (docs), and PostgreSQL ctx
// and memory newest first. Each lane keeps its own order, so the YouTrack lanes
// keep YouTrack's relevance order. Scopes tasks and docs keep YouTrack's order.
//
// Each YouTrack request asks for limit+1 hits. The page is truncated when a
// lane returned more than limit, even if some of those hits had no hk mapping,
// or when the merged list is longer than limit. A truncated page sets Truncated
// on every row.
func (s *Service) Search(ctx context.Context, fallback SearchFallback, q, scope, session string, limit int) ([]store.SearchResult, error) {
	if err := store.CheckSearch(q, scope, session); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(q)
	wantTasks := scope == "tasks" || scope == "all"
	wantDocs := scope == "docs" || scope == "all"
	if (!wantTasks && !wantDocs) || text == "" || store.NumericIDQuery(text) {
		return fallback(ctx, q, scope, session, limit)
	}
	if wantDocs {
		exact, err := s.store.DocumentKeyExists(ctx, text)
		if err != nil {
			return nil, err
		}
		if exact {
			return fallback(ctx, q, scope, session, limit)
		}
	}
	limit = searchLimit(scope, limit)

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
	if len(names) > 0 {
		issues, err = s.client.SearchIssues(yctx, issueSearchQuery(names, text), limit+1)
	}
	if err == nil && wantDocs {
		articles, err = s.client.SearchArticleText(yctx, quoteSearchText(text), limit+1)
	}
	if err != nil {
		log.Printf("search.youtrack_fallback reason=%s scope=%s", searchFailureReason(yctx, err), scope)
		return fallback(ctx, q, scope, session, limit)
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
		hits, err := s.store.YouTrackDocHits(ctx, articleIDs(articles), session)
		if err != nil {
			return nil, err
		}
		lane := []store.SearchResult{}
		for _, article := range articles {
			x, ok := hits[article.ID]
			if !ok {
				continue
			}
			x.Snippet = excerpt(article.Content, article.Summary)
			lane = append(lane, x)
		}
		lanes = append(lanes, lane)
	}
	if scope == "all" {
		var pg []store.SearchResult
		for _, sc := range []string{"ctx", "memory"} {
			rows, err := fallback(ctx, q, sc, session, limit)
			if err != nil {
				return nil, err
			}
			for i := range rows {
				if rows[i].Truncated {
					overflow = true
					rows[i].Truncated = false
				}
			}
			pg = append(pg, rows...)
		}
		sort.SliceStable(pg, func(i, j int) bool { return pg[i].CreatedAt.After(pg[j].CreatedAt) })
		if len(pg) > 0 {
			lanes = append(lanes, pg)
		}
	}

	merged := interleave(lanes)
	truncated := overflow || len(merged) > limit
	if len(merged) > limit {
		merged = merged[:limit]
	}
	for i := range merged {
		merged[i].Truncated = truncated
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

// issueSearchQuery restricts the issue search to the mirrored projects, then
// adds the user's text as one quoted phrase.
func issueSearchQuery(names []string, text string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, "project: {"+name+"}")
	}
	return "(" + strings.Join(parts, " or ") + ") " + quoteSearchText(text)
}

// quoteSearchText wraps user text in one YouTrack quoted phrase, so project:,
// for:, # or braces the user typed match as words, not as filters.
func quoteSearchText(text string) string {
	return `"` + searchQuoteEscaper.Replace(text) + `"`
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

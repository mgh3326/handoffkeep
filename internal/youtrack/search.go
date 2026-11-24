package youtrack

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// DefaultSearchTimeout bounds the YouTrack half of one search. A search that
// runs past it answers from PostgreSQL instead.
const DefaultSearchTimeout = 3 * time.Second

const (
	searchIssueFields = "id,idReadable,summary,description"
	// searchArticleRefFields is a docs-lane candidate without its content: the
	// ids that map it to a document, and its summary for the excerpt fallback.
	// Content is read per shown hit with GetArticle, so the candidate set stays
	// far below the response cap however large the articles are.
	searchArticleRefFields = "id,idReadable,summary,parentArticle(id)"
	searchExcerptRunes     = 160
	searchDefaultLimit     = 20
	searchMaxLimit         = 100
	searchMaxTaskLimit     = 50
	// searchDocContentWorkers bounds the parallel GetArticle calls that load the
	// content of the docs hits a page shows.
	searchDocContentWorkers = 8
)

// docRankOther is the rank of a document key that matches no docRankTable
// prefix. Ranks sort ascending, so a lower rank comes first.
const docRankOther = 6

// docRankJob ranks job copies last: a report or brief copy answers the question
// behind a decision, policy or design doc, so the docs lane shows the latter first.
const docRankJob = 7

// docRankTable orders the docs lane by key prefix. A key takes the rank of the
// first prefix it starts with; a key that matches none takes docRankOther.
// Inside one rank the lane keeps YouTrack's relevance order (a stable sort).
var docRankTable = []struct {
	prefix string
	rank   int
}{
	{"decision/", 0},
	{"policy/", 1},
	{"design/", 2},
	{"findings/", 3},
	{"experiment/", 4},
	{"consult/", 5},
	{"reports/", docRankJob},
	{"briefs/", docRankJob},
	{"brief/", docRankJob},
	{"review/", docRankJob},
}

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

// SearchArticleRefs returns up to top 기술 자료 articles whose text matches a
// YouTrack query, without their content (see searchArticleRefFields). A 5xx is
// not retried, for the same reason as SearchIssues.
func (c *Client) SearchArticleRefs(ctx context.Context, query string, top int) ([]Article, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("fields", searchArticleRefFields)
	q.Set("$top", strconv.Itoa(top))
	var out []Article
	err := c.do(ctx, http.MethodGet, "/api/articles", q, nil, &out, false)
	return out, err
}

// Search answers one /v1/search call from YouTrack exclusively. It is
// SearchPage without the page-level deadline state.
func (s *Service) Search(ctx context.Context, q, scope, session string, limit int) ([]store.SearchResult, error) {
	page, err := s.SearchPage(ctx, q, scope, session, limit)
	return page.Results, err
}

// SearchPage answers one /v1/search call from YouTrack exclusively.
// Exact document keys and numeric ID lookups (#<id> or <id>) are answered
// directly by exact store lookups without querying YouTrack.
// Scope "ctx" searches checkpoints and memory via SearchCtx.
//
// Scope all merges three lanes round-robin, one hit from each non-empty lane in
// turn: YouTrack issues (tasks), YouTrack articles (docs), and hk ctx
// newest first. Each lane keeps its own order, so the YouTrack lanes
// keep YouTrack's relevance order, except that the docs lane is reranked by
// docRankTable. Scopes tasks and docs keep YouTrack's order (docs reranked).
//
// The two YouTrack lanes run concurrently under one shared deadline. The docs
// lane reads its candidates without content, reranks them, and reads the content
// of the hits it shows. A lane that misses the deadline leaves the other lane's
// hits in the page; scope all then marks the page Partial and TimedOut. Scopes
// tasks and docs have one lane, so its deadline is the search's deadline and
// fails the call.
//
// Each YouTrack request asks for limit+1 hits, except the docs candidates, which
// ask for enough hits to rerank (docFetchLimit). The page is truncated when a
// lane returned more than limit, even if some of those hits had no hk mapping,
// or when the merged list is longer than limit. A truncated page sets Truncated
// on every row.
func (s *Service) SearchPage(ctx context.Context, q, scope, session string, limit int) (store.SearchPage, error) {
	if err := store.CheckSearch(q, scope, session); err != nil {
		return store.SearchPage{}, err
	}
	text := strings.TrimSpace(q)
	limit = searchLimit(scope, limit)

	if scope == "ctx" {
		rows, err := s.store.SearchCtx(ctx, text, session, limit)
		return store.SearchPage{Results: rows}, err
	}

	if store.NumericIDQuery(text) {
		hits, ok, err := s.store.ExactNumericHit(ctx, text, scope, session)
		if err != nil {
			return store.SearchPage{}, err
		}
		if ok {
			if len(hits) > limit {
				hits = hits[:limit]
				for i := range hits {
					hits[i].Truncated = true
				}
			}
			return store.SearchPage{Results: hits}, nil
		}
	}

	wantTasks := scope == "tasks" || scope == "all"
	wantDocs := scope == "docs" || scope == "all"

	if wantDocs {
		hits, ok, err := s.store.ExactDocHit(ctx, text, session)
		if err != nil {
			return store.SearchPage{}, err
		}
		if ok {
			if len(hits) > limit {
				hits = hits[:limit]
				for i := range hits {
					hits[i].Truncated = true
				}
			}
			return store.SearchPage{Results: hits}, nil
		}
	}

	var names []string
	if wantTasks {
		projects, err := s.store.ListYouTrackProjects(ctx)
		if err != nil {
			return store.SearchPage{}, err
		}
		names = mirroredShortNames(projects)
	}

	yctx, cancel := context.WithTimeout(ctx, s.searchTimeout)
	defer cancel()

	clean := cleanSearchText(text)
	var (
		issues        []Issue
		candidates    []Article
		docs          []docHit
		issuesFail    *laneFailure
		candidatesErr *laneFailure
		contentFail   *laneFailure
		docsErr       error
		wg            sync.WaitGroup
	)
	if len(names) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			issues, err = s.client.SearchIssues(yctx, issueSearchQuery(names, text), limit+1)
			if err != nil {
				issuesFail = failure(yctx, "issues", err)
			}
		}()
	}
	if wantDocs && clean != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			candidates, err = s.client.SearchArticleRefs(yctx, clean, docFetchLimit(limit))
			if err != nil {
				candidatesErr = failure(yctx, "articles", err)
				return
			}
			// The docs lane finishes here, inside its own goroutine: a slow issue
			// lane must not hold the content loads until the shared deadline.
			docs, docsErr = s.rankedDocHits(ctx, candidates, session, limit)
			if docsErr != nil {
				return
			}
			contentFail = s.loadDocContent(yctx, docs)
		}()
	}
	wg.Wait()

	if issuesFail != nil {
		issues = nil
	}
	var failures []laneFailure
	for _, f := range []*laneFailure{issuesFail, candidatesErr, contentFail} {
		if f != nil {
			failures = append(failures, *f)
		}
	}
	timedOut, err := settleLanes(ctx, scope, failures)
	if err != nil {
		return store.SearchPage{}, err
	}
	if candidatesErr != nil {
		candidates, docs = nil, nil
	}
	if docsErr != nil {
		return store.SearchPage{}, docsErr
	}

	var lanes [][]store.SearchResult
	overflow := false
	if wantTasks {
		overflow = overflow || len(issues) > limit
		hits, err := s.store.YouTrackTaskHits(ctx, s.tenant, issueIDs(issues), session)
		if err != nil {
			return store.SearchPage{}, err
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
		overflow = overflow || len(candidates) > limit
		lane := make([]store.SearchResult, 0, len(docs))
		for _, d := range docs {
			x := d.hit
			x.Snippet = excerpt(d.article.Content, d.article.Summary)
			lane = append(lane, x)
		}
		lanes = append(lanes, lane)
	}
	if scope == "all" {
		ctxHits, err := s.store.SearchCtx(ctx, text, session, limit+1)
		if err != nil {
			return store.SearchPage{}, err
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
	return store.SearchPage{Results: merged, Partial: timedOut, TimedOut: timedOut}, nil
}

// laneFailure is one YouTrack call that did not answer. It is classified when
// its own lane returns, so a timeout is read at the deadline that caused it.
type laneFailure struct {
	lane    string
	reason  string
	err     error
	timeout bool
}

// failure classifies a lane's error as it returns. A lane is a timeout only
// when the shared deadline had passed by then, or the call itself reports a
// deadline; an HTTP error that came back before the deadline stays an upstream
// failure, even if the other lane runs into the deadline later.
func failure(yctx context.Context, lane string, err error) *laneFailure {
	reason := searchFailureReason(yctx, err)
	return &laneFailure{
		lane:    lane,
		reason:  reason,
		err:     err,
		timeout: reason == "timeout" || errors.Is(err, context.DeadlineExceeded),
	}
}

// settleLanes logs every failed lane, then decides the page. An upstream error
// fails the search whatever the other lane did. A deadline fails a single-lane
// scope; scope all keeps the other lane's hits and reports timedOut.
func settleLanes(ctx context.Context, scope string, failures []laneFailure) (bool, error) {
	for _, f := range failures {
		log.Printf("search.youtrack_error lane=%s reason=%s scope=%q err=%v", f.lane, f.reason, scope, f.err)
	}
	for _, f := range failures {
		if !f.timeout {
			return false, store.ErrSearchUnavailable
		}
	}
	if len(failures) == 0 {
		return false, nil
	}
	if scope != "all" || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return false, store.ErrSearchTimeout
	}
	return true, nil
}

// docHit is one docs-lane hit with the article it came from. The article's
// content is loaded only for the hits a page shows.
type docHit struct {
	hit     store.SearchResult
	article Article
}

// rankedDocHits maps the candidate articles to their documents, reranks them by
// docRankTable (stable), and keeps the first limit+1, the most the lane ever
// returned before the rerank.
func (s *Service) rankedDocHits(ctx context.Context, candidates []Article, session string, limit int) ([]docHit, error) {
	ids := articleIDs(candidates)
	for _, a := range candidates {
		if a.HasParent() {
			ids = append(ids, a.ParentArticle.ID)
		}
	}
	hits, err := s.store.YouTrackDocHits(ctx, ids, session)
	if err != nil {
		return nil, err
	}
	out := []docHit{}
	for _, article := range candidates {
		x, ok := hits[article.ID]
		if !ok && article.HasParent() {
			x, ok = hits[article.ParentArticle.ID]
		}
		if ok {
			out = append(out, docHit{hit: x, article: article})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return docRank(out[i].hit.Key) < docRank(out[j].hit.Key)
	})
	if len(out) > limit+1 {
		out = out[:limit+1]
	}
	return out, nil
}

// loadDocContent reads the content of each hit's article, for the excerpt. A
// 404 (e.g. an article deleted between SearchArticleRefs and GetArticle) falls
// back to the article's summary. A non-timeout failure (401, 403, 5xx,
// transport) is returned first so settleLanes fails the search immediately;
// otherwise the first timeout failure in hit order is returned, keeping
// partial-page behaviour under scope all.
func (s *Service) loadDocContent(yctx context.Context, docs []docHit) *laneFailure {
	failures := make([]*laneFailure, len(docs))
	sem := make(chan struct{}, searchDocContentWorkers)
	var wg sync.WaitGroup
	for i := range docs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			a, err := s.client.GetArticle(yctx, docs[i].article.ID)
			if err != nil {
				if IsNotFound(err) {
					return
				}
				failures[i] = failure(yctx, "articles", err)
				return
			}
			docs[i].article.Content = a.Content
		}(i)
	}
	wg.Wait()
	for _, f := range failures {
		if f != nil && !f.timeout {
			return f
		}
	}
	for _, f := range failures {
		if f != nil {
			return f
		}
	}
	return nil
}

// docFetchLimit is how many candidate articles the docs lane asks YouTrack for:
// three times the page limit plus one, so the rerank has room to lift a
// decision or policy doc above job copies. It is capped at searchMaxLimit+1,
// the most the lane asked for before the rerank.
func docFetchLimit(limit int) int {
	return min(3*limit+1, searchMaxLimit+1)
}

// docRank is the rank of a document key in docRankTable.
func docRank(key string) int {
	for _, c := range docRankTable {
		if strings.HasPrefix(key, c.prefix) {
			return c.rank
		}
	}
	return docRankOther
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

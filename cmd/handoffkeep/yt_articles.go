package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// Article bindings (hk 4053, docs/youtrack-write-protocol.md §Article
// bindings). `youtrack articles-preflight` finds, for every hk document the
// import made an article for, that article in YouTrack and reports what a
// binding would record; it writes nothing. `youtrack articles-backfill` makes
// the same classification and binds the documents whose article verifies
// (matched and sha_differs). Both open the database directly and verify
// against the tenant the write worker uses (the host of YOUTRACK_URL).
//
// An article verifies when it is a top-level article of the document's
// YouTrack project whose summary is the import's rendering of the document
// (store.YTArticleSummary, or store.YTArticleIndexSummary for a split one).
// It is found by one GET of the readable id <SHORT>-A-<document id>, the id
// the import's article key would give it; when the first such GET of a
// project does not verify, that project's articles are found by summary in
// the full article list instead (lookup "summary"). On either path, a second
// top-level article of the project with the document's summary makes it
// ambiguous, never bound: the full article list is read once per run, before
// the first document that needs it.

// The classes of a document.
const (
	// ytArticleBound: bound on this tenant; no YouTrack read.
	ytArticleBound = "bound"
	// ytArticleMatched: the article verifies and holds the document's body.
	ytArticleMatched = "matched"
	// ytArticleSHADiffers: the article verifies but holds other content, or
	// the document is split (its index content is the import's): bound with
	// needs_update.
	ytArticleSHADiffers = "sha_differs"
	// ytArticleMissing: no article verifies, or the document's project has
	// no YouTrack project.
	ytArticleMissing = "missing"
	// ytArticleAmbiguous: more than one article has the document's summary,
	// a binding on another tenant, or a binding the publication refused.
	ytArticleAmbiguous = "ambiguous"
	// ytArticleExcluded: the import made no article for the document
	// (store.YTArticleExcluded).
	ytArticleExcluded = "excluded"
	// ytArticleError: a read failed; run again.
	ytArticleError = "error"
)

// How a project's articles were found.
const (
	ytArticleLookupReadable = "readable_id"
	ytArticleLookupSummary  = "summary"
)

// ytArticleBucketNames are the names the import gives its synthetic article
// projects (youtrack/import/client.js getProjects): documents with no task go
// to "Shared documents", documents of a task with no project to "Unassigned
// hk tasks". Their short names (SHRDOC, HK) are only a base: the import adds
// a numeric tail when an hk project already took one (SHRDOC2), so a bucket
// is resolved by its name and its short name read from YouTrack.
var ytArticleBucketNames = map[string]string{
	store.YTArticleBucketShared:     "Shared documents",
	store.YTArticleBucketUnassigned: "Unassigned hk tasks",
}

// ytArticleBucket is a synthetic bucket's YouTrack project.
type ytArticleBucket struct {
	ID        string
	ShortName string
}

// ytArticleListTimeout bounds reading the whole article list (one GET per
// 100 articles), outside any one document's budget.
const ytArticleListTimeout = 10 * time.Minute

// ytArticleDocPage is how many documents a run lists at a time (a variable
// so a test can page a few).
var ytArticleDocPage = store.YTArticleDocPageMax

type ytArticlesClass struct {
	Count       int     `json:"count"`
	DocumentIDs []int64 `json:"document_ids"`
}

func (c *ytArticlesClass) add(id int64) {
	c.Count++
	if len(c.DocumentIDs) < ytCommentsSample {
		c.DocumentIDs = append(c.DocumentIDs, id)
	}
}

type ytArticlesProject struct {
	Project    string          `json:"project"`
	Lookup     string          `json:"lookup"`
	Bound      ytArticlesClass `json:"bound"`
	Matched    ytArticlesClass `json:"matched"`
	SHADiffers ytArticlesClass `json:"sha_differs"`
	Missing    ytArticlesClass `json:"missing"`
	Ambiguous  ytArticlesClass `json:"ambiguous"`
	// SummaryCut is the ambiguous documents refused for reason summary_cut
	// (also counted in Ambiguous).
	SummaryCut ytArticlesClass `json:"summary_cut"`
	Excluded   ytArticlesClass `json:"excluded"`
	// ListUnstable is set when the project's article list differed between
	// two reads (reason list_unstable): its documents are class error.
	ListUnstable bool            `json:"list_unstable,omitempty"`
	Error        ytArticlesClass `json:"error"`
	ytProject    string
}

func (p *ytArticlesProject) class(name string) *ytArticlesClass {
	switch name {
	case ytArticleBound:
		return &p.Bound
	case ytArticleMatched:
		return &p.Matched
	case ytArticleSHADiffers:
		return &p.SHADiffers
	case ytArticleMissing:
		return &p.Missing
	case ytArticleAmbiguous:
		return &p.Ambiguous
	case ytArticleExcluded:
		return &p.Excluded
	default:
		return &p.Error
	}
}

// ytArticlesReport is the output of both commands. BoundNow is set by the
// backfill only.
type ytArticlesReport struct {
	Tenant    string               `json:"tenant"`
	Project   string               `json:"project,omitempty"`
	Documents int                  `json:"documents"`
	Projects  []*ytArticlesProject `json:"projects"`
	BoundNow  *int                 `json:"bound_now,omitempty"`
	errors    int
}

// ytArticleRemote is the YouTrack the commands read.
type ytArticleRemote interface {
	ReadArticle(ctx context.Context, articleID string) (youtrack.Article, error)
	ListArticles(ctx context.Context) ([]youtrack.Article, error)
}

// ytArticler runs one preflight or backfill.
type ytArticler struct {
	st      *store.Store
	remote  ytArticleRemote
	tenant  string
	buckets map[string]ytArticleBucket // synthetic bucket -> its YouTrack project
	// lookup is each YouTrack project's lookup, decided by its first GET.
	lookup map[string]string
	// index is the full article list by project id and summary, top-level
	// articles only; read once, before the first document that needs a
	// lookup and outside that document's budget (run), or by classify when
	// it is called alone.
	index map[string]map[string][]youtrack.Article
	// unstable is the YouTrack projects whose article list differed between
	// the two reads (decision-4): their documents are never bound.
	unstable map[string]bool
}

// ytArticleListUnstable is the reason a project's documents are not
// classified when its article list was not the same on two reads.
const ytArticleListUnstable = "list_unstable"

// ytArticleListEntry is what two reads of a project's article list must
// agree on, per article id.
type ytArticleListEntry struct {
	summary, parent string
}

// readArticleList reads the full article list once and groups it by
// project; an id seen twice in one read (an offset page that shifted) marks
// its project unstable.
func (a *ytArticler) readArticleList(ctx context.Context) ([]youtrack.Article, map[string]map[string]ytArticleListEntry, map[string]bool, error) {
	gctx, cancel := context.WithTimeout(ctx, ytArticleListTimeout)
	all, err := a.remote.ListArticles(gctx)
	cancel()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list YouTrack articles: %w", err)
	}
	byProject := map[string]map[string]ytArticleListEntry{}
	dup := map[string]bool{}
	for _, x := range all {
		if x.Project == nil {
			continue
		}
		set := byProject[x.Project.ID]
		if set == nil {
			set = map[string]ytArticleListEntry{}
			byProject[x.Project.ID] = set
		}
		if _, seen := set[x.ID]; seen || x.ID == "" {
			dup[x.Project.ID] = true
		}
		parent := ""
		if x.HasParent() {
			parent = x.ParentArticle.ID
		}
		set[x.ID] = ytArticleListEntry{summary: x.Summary, parent: parent}
	}
	return all, byProject, dup, nil
}

// ytArticleResult is one document's class, with the binding a backfill
// would publish when the class is matched or sha_differs.
type ytArticleResult struct {
	class   string
	detail  string
	binding store.YTArticleBinding
	// summaryCut marks an ambiguous result refused because the document's
	// summary is cut at 250 code points (decision-5, hk 4053): counted under
	// summary_cut as well.
	summaryCut bool
}

// ytArticleTarget is the YouTrack project of a document's article: its short
// name and id, or a reason it has none.
func (a *ytArticler) target(c store.YTArticleCandidate) (short, id, missing string) {
	if _, ok := ytArticleBucketNames[c.Bucket]; ok {
		b := a.buckets[c.Bucket]
		return b.ShortName, b.ID, ""
	}
	// A row from before `schema apply` (the store seeds handoffkeep with HK)
	// can still name a synthetic bucket's project, not the hk project's own.
	for name, b := range a.buckets {
		if c.YTProjectID != "" && c.YTProjectID == b.ID {
			return "", "", fmt.Sprintf("project %q maps to the import's %s project %s, not its own", c.Bucket, name, b.ShortName)
		}
	}
	if c.ShortName == "" || c.YTProjectID == "" {
		return "", "", fmt.Sprintf("project %q has no YouTrack project id in youtrack_projects", c.Bucket)
	}
	return c.ShortName, c.YTProjectID, ""
}

// verifies reports whether article x is the document's article in project
// projectID: top level, in that project, with either of its summaries.
func ytArticleVerifies(x youtrack.Article, projectID, single, index string) bool {
	return x.ID != "" && !x.HasParent() && x.Project != nil && x.Project.ID == projectID && (x.Summary == single || x.Summary == index)
}

// needsLookup reports whether classify reads YouTrack for d.
func (a *ytArticler) needsLookup(d store.YTArticleDoc) bool {
	return d.BoundTenant == "" && !store.YTArticleExcluded(d.Key, d.TaskID) && !store.YTArticleSummaryCut(d.Key, d.Kind)
}

// loadIndex reads the article list twice, as EnumerateStableComments reads
// comments (internal/ytwrite/enumerate.go), and keeps the second read. A
// project whose articles (ids, summaries, parents) differ between the two
// reads, or repeat within one, is unstable: offset pages shifted or a writer
// ran while it was read. The backfill's precondition is that no article
// writer runs (the import ended, D3); the double read detects a breach.
func (a *ytArticler) loadIndex(ctx context.Context) error {
	if a.index != nil {
		return nil
	}
	_, first, dup1, err := a.readArticleList(ctx)
	if err != nil {
		return err
	}
	all, second, dup2, err := a.readArticleList(ctx)
	if err != nil {
		return err
	}
	unstable := map[string]bool{}
	for _, sets := range []map[string]map[string]ytArticleListEntry{first, second} {
		for projectID := range sets {
			if dup1[projectID] || dup2[projectID] || !maps.Equal(first[projectID], second[projectID]) {
				unstable[projectID] = true
			}
		}
	}
	a.unstable = unstable
	index := map[string]map[string][]youtrack.Article{}
	for _, x := range all {
		if x.ID == "" || x.HasParent() || x.Project == nil {
			continue
		}
		bySummary := index[x.Project.ID]
		if bySummary == nil {
			bySummary = map[string][]youtrack.Article{}
			index[x.Project.ID] = bySummary
		}
		bySummary[x.Summary] = append(bySummary[x.Summary], x)
	}
	a.index = index
	return nil
}

// classify classifies one document; bindBy is the would-be binding's
// bound_by. A bound or excluded document is classified without a YouTrack
// read.
func (a *ytArticler) classify(ctx context.Context, d store.YTArticleDoc, bindBy string) ytArticleResult {
	failed := func(err error) ytArticleResult { return ytArticleResult{class: ytArticleError, detail: err.Error()} }
	c := d.YTArticleCandidate
	switch {
	case store.YTArticleExcluded(c.Key, c.TaskID):
		return ytArticleResult{class: ytArticleExcluded}
	case d.BoundTenant == a.tenant:
		return ytArticleResult{class: ytArticleBound}
	case d.BoundTenant != "":
		return ytArticleResult{class: ytArticleAmbiguous, detail: "bound on tenant " + d.BoundTenant}
	}
	short, projectID, missing := a.target(c)
	if missing != "" {
		return ytArticleResult{class: ytArticleMissing, detail: missing}
	}
	// A summary cut at 250 code points no longer holds the whole key, so
	// another document can render it too, and neither a unique article nor
	// equal content proves which document an article is (two documents may
	// share a body). Such a document is never bound (decision-5): it is
	// ambiguous, reason summary_cut, with no YouTrack read.
	if store.YTArticleSummaryCut(c.Key, c.Kind) {
		return ytArticleResult{class: ytArticleAmbiguous, summaryCut: true, detail: "summary_cut: the summary is cut at 250 code points and cannot name the document"}
	}
	single, index := store.YTArticleSummary(c.Key, c.Kind), store.YTArticleIndexSummary(c.Key, c.Kind)
	if err := a.loadIndex(ctx); err != nil {
		return failed(err)
	}
	if a.unstable[projectID] {
		return ytArticleResult{class: ytArticleError, detail: ytArticleListUnstable + ": the article list of project " + short + " differed between two reads; nothing is bound in it"}
	}
	hits := append(append([]youtrack.Article{}, a.index[projectID][single]...), a.index[projectID][index]...)
	ambiguous := func(hits []youtrack.Article) ytArticleResult {
		ids := make([]string, len(hits))
		for i, x := range hits {
			ids[i] = x.ID
		}
		return ytArticleResult{class: ytArticleAmbiguous, detail: fmt.Sprintf("%d articles of project %s have the summary: %s", len(hits), short, strings.Join(ids, ","))}
	}
	var found youtrack.Article
	haveContent := false
	if mode := a.lookup[projectID]; mode != ytArticleLookupSummary {
		readable := short + "-A-" + strconv.FormatInt(c.DocumentID, 10)
		gctx, cancel := context.WithTimeout(ctx, ytCommentsGetTimeout)
		x, err := a.remote.ReadArticle(gctx, readable)
		cancel()
		switch {
		case err == nil && strings.EqualFold(x.IDReadable, readable) && ytArticleVerifies(x, projectID, single, index):
			found, haveContent = x, true
			a.lookup[projectID] = ytArticleLookupReadable
		case err != nil && !youtrack.IsNotFound(err):
			return failed(fmt.Errorf("read article %s: %w", readable, err))
		case mode == "":
			// The first GET of the project did not verify: the import's
			// key does not make the readable id, or this article is gone.
			// Find the project's articles by summary from here on.
			a.lookup[projectID] = ytArticleLookupSummary
		}
	}
	switch {
	case found.ID != "":
		// The readable id names the article; any other top-level article
		// of the project with the summary still makes it ambiguous.
		all := hits
		if !slices.ContainsFunc(hits, func(x youtrack.Article) bool { return x.ID == found.ID }) {
			all = append([]youtrack.Article{found}, hits...)
		}
		if len(all) > 1 {
			return ambiguous(all)
		}
	case len(hits) == 0:
		return ytArticleResult{class: ytArticleMissing, detail: "no article of project " + short + " has summary " + strconv.Quote(single)}
	case len(hits) == 1:
		found = hits[0]
	default:
		return ambiguous(hits)
	}
	b := store.YTArticleBinding{DocumentID: c.DocumentID, Tenant: a.tenant, ArticleID: found.ID, ArticleReadable: found.IDReadable, ProjectID: projectID, BoundBy: bindBy}
	if found.Summary == index {
		b.NeedsUpdate = true
		return ytArticleResult{class: ytArticleSHADiffers, detail: "split document: index article " + found.ID, binding: b}
	}
	if !haveContent {
		gctx, cancel := context.WithTimeout(ctx, ytCommentsGetTimeout)
		x, err := a.remote.ReadArticle(gctx, found.ID)
		cancel()
		if err != nil {
			return failed(fmt.Errorf("read article %s: %w", found.ID, err))
		}
		if x.ID != found.ID || !ytArticleVerifies(x, projectID, single, index) {
			return ytArticleResult{class: ytArticleAmbiguous, detail: "article " + found.ID + " changed while it was read"}
		}
		found = x
	}
	sum := sha256.Sum256([]byte(found.Content))
	b.ContentSHA = hex.EncodeToString(sum[:])
	if b.ContentSHA != c.SHA256 {
		b.NeedsUpdate = true
		return ytArticleResult{class: ytArticleSHADiffers, detail: "article " + found.ID + " content sha " + b.ContentSHA, binding: b}
	}
	return ytArticleResult{class: ytArticleMatched, binding: b}
}

// run classifies every document of bucket ("" for all), in id order, a page
// at a time, and when bindBy is not "" (the backfill) publishes each matched
// and sha_differs binding; a refused publication counts as ambiguous and the
// run goes on. It never stops on one document.
func (a *ytArticler) run(ctx context.Context, bucket, bindBy string) (ytArticlesReport, error) {
	r := ytArticlesReport{Tenant: a.tenant, Project: bucket, Projects: []*ytArticlesProject{}}
	if a.lookup == nil {
		a.lookup = map[string]string{}
	}
	byProject := map[string]*ytArticlesProject{}
	boundNow := 0
	var after int64
	for {
		docs, err := a.st.YTArticleDocs(ctx, bucket, after, ytArticleDocPage)
		if err != nil {
			return r, err
		}
		for _, d := range docs {
			after = d.DocumentID
			r.Documents++
			// The article list is read under the run's context, before the
			// document's budget starts: a long list never eats it.
			var listErr error
			if a.needsLookup(d) {
				listErr = a.loadIndex(ctx)
			}
			tctx, cancel := context.WithTimeout(ctx, ytCommentsTaskTimeout)
			got := ytArticleResult{class: ytArticleError}
			if listErr != nil {
				got.detail = listErr.Error()
			} else {
				got = a.classify(tctx, d, bindBy)
			}
			if bindBy != "" && (got.class == ytArticleMatched || got.class == ytArticleSHADiffers) {
				_, err := a.st.AddYTArticleBinding(tctx, got.binding, d.YTArticleCandidate)
				switch {
				case err == nil:
					boundNow++
				case errors.Is(err, store.ErrYTArticleBindingConflict):
					got = ytArticleResult{class: ytArticleAmbiguous, detail: err.Error()}
				default:
					got = ytArticleResult{class: ytArticleError, detail: err.Error()}
				}
			}
			cancel()
			switch got.class {
			case ytArticleError:
				r.errors++
				log.Printf("youtrack.articles_classify_failed document_id=%d err=%q", d.DocumentID, got.detail)
			case ytArticleMissing, ytArticleAmbiguous, ytArticleSHADiffers:
				log.Printf("youtrack.articles_classified document_id=%d class=%s detail=%q", d.DocumentID, got.class, got.detail)
			}
			p := byProject[d.Bucket]
			if p == nil {
				p = &ytArticlesProject{Project: d.Bucket}
				for _, name := range []string{ytArticleBound, ytArticleMatched, ytArticleSHADiffers, ytArticleMissing, ytArticleAmbiguous, ytArticleExcluded, ytArticleError} {
					p.class(name).DocumentIDs = []int64{}
				}
				p.SummaryCut.DocumentIDs = []int64{}
				byProject[d.Bucket] = p
				r.Projects = append(r.Projects, p)
			}
			if p.ytProject == "" {
				_, p.ytProject, _ = a.target(d.YTArticleCandidate)
			}
			p.class(got.class).add(d.DocumentID)
			if got.summaryCut {
				p.SummaryCut.add(d.DocumentID)
			}
		}
		if len(docs) < ytArticleDocPage {
			break
		}
	}
	for _, p := range r.Projects {
		p.Lookup = a.lookup[p.ytProject]
		p.ListUnstable = a.unstable[p.ytProject]
	}
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].Project < r.Projects[j].Project })
	if bindBy != "" {
		r.BoundNow = &boundNow
	}
	return r, nil
}

// ytArticleBuckets resolves each synthetic bucket's YouTrack project by the
// name the import gave it (ytArticleBucketNames): none, or more than one,
// refuses the run, as youtrack.ResolveBucketProject does for HK. The short
// name is the project's own, tail included.
func ytArticleBuckets(ctx context.Context, projects youtrack.ProjectLister) (map[string]ytArticleBucket, error) {
	all, err := projects.ListAdminProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]ytArticleBucket{}
	for bucket, name := range ytArticleBucketNames {
		var hits []ytArticleBucket
		for _, p := range all {
			if p.Name == name {
				hits = append(hits, ytArticleBucket{ID: p.ID, ShortName: p.ShortName})
			}
		}
		if len(hits) != 1 || hits[0].ID == "" || hits[0].ShortName == "" {
			return nil, fmt.Errorf("%w: %d YouTrack projects are named %q %v, want exactly one with an id and short name", youtrack.ErrBucketProject, len(hits), name, hits)
		}
		out[bucket] = hits[0]
	}
	return out, nil
}

const (
	youtrackArticlesPreflightUsage = "usage: youtrack articles-preflight [--project P]"
	youtrackArticlesBackfillUsage  = "usage: youtrack articles-backfill [--project P] [--by NAME]"
)

// youtrackArticlesCmd is `youtrack articles-preflight` (backfill false):
// read-only, it reads YouTrack articles but writes nothing, not even a
// migration (store.OpenReadOnly); and `youtrack articles-backfill` (backfill
// true), which also publishes each verified binding. --project is an hk
// project, /shared or /unassigned. The report goes to stdout; a document
// whose reads failed (class error) makes the command exit 1 after the report.
func youtrackArticlesCmd(args []string, out io.Writer, backfill bool) error {
	name, usage := "articles-preflight", youtrackArticlesPreflightUsage
	if backfill {
		name, usage = "articles-backfill", youtrackArticlesBackfillUsage
	}
	fs := flag.NewFlagSet("youtrack "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "only documents of this hk project (or /shared, /unassigned)")
	valueFlags := []string{"project"}
	var by *string
	if backfill {
		by = fs.String("by", "operator:cli", "who bound the documents")
		valueFlags = append(valueFlags, "by")
	}
	ordered, err := splitArgs(args, valueFlags...)
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, usage)
	}
	if fs.NArg() != 0 {
		return errors.New(usage)
	}
	bindBy := ""
	if backfill {
		if bindBy = strings.TrimSpace(*by); bindBy == "" {
			return errors.New(usage)
		}
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if dbURL == "" || baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL, YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	openCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The preflight never writes: it opens without migrating, on a pool
	// whose every transaction is READ ONLY, and refuses a schema without
	// yt_article_bindings. The backfill migrates as the server does.
	openStore := store.OpenReadOnly
	if backfill {
		openStore = store.Open
	}
	st, err := openStore(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if !backfill {
		applied, err := st.MigrationApplied(openCtx, store.YTArticleBindingsMigration)
		if err != nil {
			return err
		}
		if !applied {
			return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("the database lacks migration %s: deploy the release that adds it first; the preflight never migrates", store.YTArticleBindingsMigration)}
		}
	}
	bucketCtx, cancelBucket := context.WithTimeout(context.Background(), ytCommentsGetTimeout)
	buckets, err := ytArticleBuckets(bucketCtx, client)
	cancelBucket()
	if errors.Is(err, youtrack.ErrBucketProject) {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	if err != nil {
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("list YouTrack projects: %w", err)}
	}
	a := &ytArticler{st: st, remote: client, tenant: youtrack.NewWriteRemote(client).Tenant(), buckets: buckets}
	report, err := a.run(context.Background(), strings.TrimSpace(*project), bindBy)
	if err != nil {
		return err
	}
	if err := printJSON(out, report); err != nil {
		return err
	}
	if report.errors > 0 {
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("%d documents could not be classified (class error); run again", report.errors)}
	}
	return nil
}

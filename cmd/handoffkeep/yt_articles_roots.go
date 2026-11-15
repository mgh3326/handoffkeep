package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// `youtrack articles roots` is the read-only report of the family roots a
// project holds (hk 4797). A family root is the top-level article every
// document of a family goes under; a retry an earlier release made can leave
// several articles carrying one family's marker (production had 30 "briefs"
// roots, 28 "brief", 14 "deploy"). The report names them all, the children
// each has, and the one hk uses: the OLDEST by creation time, then by id,
// which is what the article_create send picks when the family has no recorded
// root. Deleting or merging the other roots is an operator's action in
// YouTrack; this command writes nothing and reads only.
const youtrackArticlesRootsUsage = "usage: youtrack articles roots [--family F] [--project P]"

// ytArticleRootTimeout bounds the report: one article list, then one GET per
// top-level article whose summary can be a family.
const ytArticleRootTimeout = 10 * time.Minute

// ytArticleRoot is one root article of a family.
type ytArticleRoot struct {
	Family string `json:"family"`
	// Project is the YouTrack project id, ProjectShort its short name.
	Project      string `json:"project"`
	ProjectShort string `json:"project_short,omitempty"`
	ArticleID    string `json:"article_id"`
	// Created is the article's creation time in RFC 3339 UTC, "" when YouTrack
	// did not report it.
	Created string `json:"created,omitempty"`
	// Children is how many articles sit under it.
	Children int `json:"children"`
	// Used is set on the root hk uses for the family: the oldest of its roots.
	Used bool `json:"used"`
}

// ytArticleRootsReport is the command's answer.
type ytArticleRootsReport struct {
	Family string `json:"family,omitempty"`
	// Duplicates counts the families with more than one root.
	Duplicates int             `json:"duplicates"`
	Roots      []ytArticleRoot `json:"roots"`
}

// ytArticleList is the YouTrack read the report makes.
type ytArticleList interface {
	ListArticles(ctx context.Context) ([]youtrack.Article, error)
	ReadArticle(ctx context.Context, articleID string) (youtrack.Article, error)
}

// youtrackArticlesRootsCmd is `youtrack articles roots`.
func youtrackArticlesRootsCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "roots" {
		return errors.New(youtrackArticlesRootsUsage)
	}
	fs := flag.NewFlagSet("youtrack articles roots", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	family := fs.String("family", "", "only this family")
	project := fs.String("project", "", "only the articles of this YouTrack project (short name or id)")
	ordered, err := splitArgs(args[1:], "family", "project")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, youtrackArticlesRootsUsage)
	}
	if fs.NArg() != 0 {
		return errors.New(youtrackArticlesRootsUsage)
	}
	fam, proj := strings.TrimSpace(*family), strings.TrimSpace(*project)
	if fam != "" && !ytwrite.ValidFamily(fam) {
		return fmt.Errorf("%v: family %q is not a document key's first segment", errors.New(youtrackArticlesRootsUsage), fam)
	}
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), ytArticleRootTimeout)
	defer cancel()
	report, err := ytArticleRoots(ctx, client, fam, proj)
	if err != nil {
		return exitCodeError{code: youtrackExitFailed, err: err}
	}
	return printJSON(out, report)
}

// ytArticleRoots lists the family roots of every project the bot can see, or
// of one family, or of one project, and marks the root each family uses.
func ytArticleRoots(ctx context.Context, remote ytArticleList, family, project string) (ytArticleRootsReport, error) {
	report := ytArticleRootsReport{Family: family}
	all, err := remote.ListArticles(ctx)
	if err != nil {
		return report, fmt.Errorf("list YouTrack articles: %w", err)
	}
	children := map[string]int{}
	for _, a := range all {
		if a.HasParent() {
			children[a.ParentArticle.ID]++
		}
	}
	byFamily := map[string][]ytArticleRoot{}
	for _, a := range all {
		if a.HasParent() || a.Project == nil || a.ID == "" || !ytwrite.ValidFamily(a.Summary) {
			continue
		}
		if family != "" && a.Summary != family {
			continue
		}
		if project != "" && a.Project.ShortName != project && a.Project.ID != project {
			continue
		}
		// A family root carries its family's marker; an article with the
		// family's summary and no marker is someone else's text.
		full, err := remote.ReadArticle(ctx, a.ID)
		if err != nil {
			return report, fmt.Errorf("read article %s: %w", a.ID, err)
		}
		if !strings.Contains(full.Content, ytwrite.FamilyRootMarker(a.Summary)) {
			continue
		}
		root := ytArticleRoot{Family: a.Summary, Project: a.Project.ID, ProjectShort: a.Project.ShortName,
			ArticleID: a.ID, Children: children[a.ID]}
		if full.Created > 0 {
			root.Created = time.UnixMilli(full.Created).UTC().Format(time.RFC3339)
		}
		byFamily[a.Project.ID+"\x00"+a.Summary] = append(byFamily[a.Project.ID+"\x00"+a.Summary], root)
	}
	for _, roots := range byFamily {
		used := ytOldestRoot(roots)
		for i := range roots {
			roots[i].Used = roots[i].ArticleID == used
		}
		if len(roots) > 1 {
			report.Duplicates++
		}
		report.Roots = append(report.Roots, roots...)
	}
	sort.Slice(report.Roots, func(i, j int) bool {
		a, b := report.Roots[i], report.Roots[j]
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Created < b.Created
	})
	return report, nil
}

// ytOldestRoot is the root hk uses among a family's: the oldest by creation
// time, then by id (ytwrite.OldestFamilyRoot, the rule the article_create send
// applies). "" for no roots.
func ytOldestRoot(roots []ytArticleRoot) string {
	articles := make([]ytwrite.RemoteArticle, 0, len(roots))
	for _, r := range roots {
		a := ytwrite.RemoteArticle{ID: r.ArticleID}
		if r.Created != "" {
			if at, err := time.Parse(time.RFC3339, r.Created); err == nil {
				a.CreatedAt = at
			}
		}
		articles = append(articles, a)
	}
	oldest, ok := ytwrite.OldestFamilyRoot(articles)
	if !ok {
		return ""
	}
	return oldest.ID
}

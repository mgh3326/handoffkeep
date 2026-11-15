package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// `youtrack articles roots merge` cleans up a family's duplicate roots
// (hk 4806): a retry an earlier release made left several articles carrying
// one family's marker (production had 30 "briefs" roots, 28 "brief", 14
// "deploy"), the send uses one of them, and the others hold the family's
// documents. The merge keeps the root hk uses, moves every other root's
// children under it, and deletes that root once it has no child left and
// still carries the family marker. Without --apply it is a dry run: it reads
// and prints the plan, and writes nothing.
const ytRootsMergeUsage = "usage: youtrack articles roots merge [--family F] [--project P] [--apply]"

// Where the kept root came from, and why a family is refused. A refusal is a
// family the merge cannot decide safely; it writes nothing for it.
const (
	ytKeptRecorded = "recorded"
	ytKeptOldest   = "oldest"

	ytRefusedRecordedMissing   = "recorded_root_missing"
	ytRefusedRecordedNotOldest = "recorded_root_not_oldest"
)

// Why a duplicate root is left in place. The first three are decided from the
// duplicate's re-read; family_stopped marks a duplicate an earlier failure in
// the same family left unprocessed, so a later run takes it.
const (
	ytSkipHasChildren   = "has_children"
	ytSkipNotTopLevel   = "not_top_level"
	ytSkipMarkerMissing = "marker_missing"
	ytSkipFamilyStopped = "family_stopped"
)

// ytRootsMergeReport is the command's answer: the plan without --apply, and
// the plan with what each step did with --apply.
type ytRootsMergeReport struct {
	Apply bool `json:"apply"`
	// Families holds one entry per family with more than one root.
	Families []ytRootsMergeFamily `json:"families"`
	// Counts totals the families' counts.
	Counts ytRootsMergeCounts `json:"counts"`
}

// ytRootsMergeFamily is one family's plan and result.
type ytRootsMergeFamily struct {
	Family string `json:"family"`
	// Project is the YouTrack project id, ProjectShort its short name.
	Project      string `json:"project"`
	ProjectShort string `json:"project_short,omitempty"`
	// Kept is the root every child is gathered under; "" when Refused is set.
	Kept string `json:"kept,omitempty"`
	// KeptSource is ytKeptRecorded or ytKeptOldest.
	KeptSource string `json:"kept_source,omitempty"`
	// Refused names why the family is left alone, "" when it is not.
	Refused string `json:"refused,omitempty"`
	// Error is the failure that stopped this family, "" when none did.
	Error string `json:"error,omitempty"`
	// Duplicates are the family's other roots, oldest first.
	Duplicates []ytRootsMergeDuplicate `json:"duplicates"`
	// Counts is what the apply did for this family, and how many of its
	// duplicates it left (skipped) or refused.
	Counts ytRootsMergeCounts `json:"counts"`
}

// ytRootsMergeDuplicate is one root the merge empties and deletes.
type ytRootsMergeDuplicate struct {
	ArticleID string `json:"article_id"`
	Summary   string `json:"summary"`
	// Created is the root's creation time in RFC 3339 UTC, "" when YouTrack
	// did not report it.
	Created string `json:"created,omitempty"`
	// Children are the articles under this root, moved to the kept root.
	Children []ytRootsMergeChild `json:"children"`
	// Delete is the delete that follows the moves: what the dry run plans and
	// what the apply did. False when the duplicate is left in place.
	Delete bool `json:"delete"`
	// Skip names why the duplicate was left in place, "" when it was deleted
	// or is still planned to be.
	Skip string `json:"skip,omitempty"`
}

// ytRootsMergeChild is one article under a duplicate root.
type ytRootsMergeChild struct {
	ArticleID string `json:"article_id"`
	Summary   string `json:"summary"`
}

// ytRootsMergeCounts counts one family's or the whole run's steps.
type ytRootsMergeCounts struct {
	Moved   int `json:"moved"`
	Deleted int `json:"deleted"`
	Skipped int `json:"skipped"`
	Refused int `json:"refused"`
}

// ytRootsMergeRemote is the YouTrack surface the merge needs: the roots read
// the report makes, the parent move, and the delete.
type ytRootsMergeRemote interface {
	ytArticleList
	SetArticleParent(ctx context.Context, articleID, parentID string) (youtrack.Article, error)
	DeleteArticle(ctx context.Context, articleID string) error
}

// ytFamilyRootSource reads the root the store recorded for a family (hk 4797).
// *store.YTWriteLedger is it; the unit tests hand in their own.
type ytFamilyRootSource interface {
	FamilyRoot(ctx context.Context, tenant, projectID, family string) (string, bool, error)
}

// youtrackArticleRootsMergeCmd is `youtrack articles roots merge`.
func youtrackArticleRootsMergeCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack articles roots merge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	family := fs.String("family", "", "only this family")
	project := fs.String("project", "", "only the articles of this YouTrack project (short name or id)")
	apply := fs.Bool("apply", false, "move the duplicates' children and delete the emptied duplicates")
	ordered, err := splitArgs(args, "family", "project")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, ytRootsMergeUsage)
	}
	if fs.NArg() != 0 {
		return errors.New(ytRootsMergeUsage)
	}
	fam, proj := strings.TrimSpace(*family), strings.TrimSpace(*project)
	if fam != "" && !ytwrite.ValidFamily(fam) {
		return fmt.Errorf("%v: family %q is not a document key's first segment", errors.New(ytRootsMergeUsage), fam)
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
	// Read-only in both modes: the merge writes YouTrack, never hk, and the
	// recorded root is a read. It never migrates, so a schema without the
	// family record is refused below instead of opened.
	st, err := store.OpenReadOnly(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if applied, err := st.MigrationApplied(openCtx, store.YTFamilyRootsMigration); err != nil {
		return err
	} else if !applied {
		return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("the database lacks migration %s: deploy the release that adds it first; the merge never migrates", store.YTFamilyRootsMigration)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), ytArticleRootTimeout)
	defer cancel()
	report, err := ytRootsMergePlan(ctx, client, st.YTWriteLedger(), youtrack.NewWriteRemote(client).Tenant(), fam, proj)
	if err != nil {
		return exitCodeError{code: youtrackExitFailed, err: err}
	}
	report.Apply = *apply
	if *apply {
		report = ytRootsMergeApply(ctx, client, report)
	}
	if err := printJSON(out, report); err != nil {
		return err
	}
	if report.failed() {
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("the merge is incomplete: %d families stopped; run it again", report.failedCount())}
	}
	return nil
}

// ytRootsMergePlan reads the roots, decides each family's kept root, and
// lists the children to move and the delete that follows for every duplicate.
// It writes nothing. A family is refused when the store's recorded root and
// the oldest root disagree — the sends then resolve a root other than the one
// recorded, and either choice could strand documents under the other — or
// when the recorded root is no longer a root in YouTrack.
func ytRootsMergePlan(ctx context.Context, remote ytRootsMergeRemote, rec ytFamilyRootSource, tenant, family, project string) (ytRootsMergeReport, error) {
	report := ytRootsMergeReport{}
	scan, err := ytScanRoots(ctx, remote, family, project)
	if err != nil {
		return report, err
	}
	byFamily := map[string][]ytArticleRoot{}
	for _, r := range scan.roots {
		key := r.Project + "\x00" + r.Family
		byFamily[key] = append(byFamily[key], r)
	}
	var keys []string
	for key, roots := range byFamily {
		if len(roots) > 1 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := byFamily[keys[i]][0], byFamily[keys[j]][0]
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		return a.Project < b.Project
	})
	for _, key := range keys {
		roots := byFamily[key]
		entry := ytRootsMergeFamily{Family: roots[0].Family, Project: roots[0].Project, ProjectShort: roots[0].ProjectShort}
		recorded, found, err := rec.FamilyRoot(ctx, tenant, entry.Project, entry.Family)
		if err != nil {
			return report, fmt.Errorf("read the recorded root of family %q on project %s: %w", entry.Family, entry.Project, err)
		}
		oldest := ytOldestRoot(roots)
		switch {
		case !found:
			entry.Kept, entry.KeptSource = oldest, ytKeptOldest
		case recorded == oldest:
			entry.Kept, entry.KeptSource = recorded, ytKeptRecorded
		case !ytHasRoot(roots, recorded):
			entry.Refused = ytRefusedRecordedMissing
		default:
			entry.Refused = ytRefusedRecordedNotOldest
		}
		if entry.Refused != "" {
			entry.Counts.Refused = 1
			report.Families = append(report.Families, entry)
			continue
		}
		for _, r := range roots {
			if r.ArticleID == entry.Kept {
				continue
			}
			dup := ytRootsMergeDuplicate{ArticleID: r.ArticleID, Summary: r.Family, Created: r.Created, Delete: true}
			for _, c := range scan.children[r.ArticleID] {
				dup.Children = append(dup.Children, ytRootsMergeChild{ArticleID: c.ID, Summary: c.Summary})
			}
			sort.Slice(dup.Children, func(i, j int) bool { return dup.Children[i].ArticleID < dup.Children[j].ArticleID })
			entry.Duplicates = append(entry.Duplicates, dup)
		}
		sort.SliceStable(entry.Duplicates, func(i, j int) bool { return ytRootOlder(entry.Duplicates[i], entry.Duplicates[j]) })
		report.Families = append(report.Families, entry)
	}
	report.Counts = ytRootsMergeTotals(report)
	return report, nil
}

// ytRootsMergeApply runs the plan: per duplicate, the children move to the
// kept root, then the duplicate is re-read and deleted only when it has no
// child left, is top-level and still carries the family marker. A failure
// stops that family and leaves the rest unprocessed there; other families
// continue. Rerunning converges: a moved child is already under the kept root,
// a deleted duplicate is gone, and a root the run left is planned again.
func ytRootsMergeApply(ctx context.Context, remote ytRootsMergeRemote, report ytRootsMergeReport) ytRootsMergeReport {
	for fi := range report.Families {
		entry := &report.Families[fi]
		if entry.Refused != "" {
			continue
		}
		for di := range entry.Duplicates {
			dup := &entry.Duplicates[di]
			stopped := false
			for _, child := range dup.Children {
				if _, err := remote.SetArticleParent(ctx, child.ArticleID, entry.Kept); err != nil {
					log.Printf("yt.roots_merge.moved_failed family=%q article_id=%s parent_id=%s err=%q", entry.Family, child.ArticleID, entry.Kept, err.Error())
					entry.Error = err.Error()
					stopped = true
					break
				}
				log.Printf("yt.roots_merge.moved family=%q article_id=%s parent_id=%s", entry.Family, child.ArticleID, entry.Kept)
				entry.Counts.Moved++
			}
			if !stopped {
				now, err := remote.ReadArticle(ctx, dup.ArticleID)
				switch {
				case err != nil:
					log.Printf("yt.roots_merge.deleted_failed family=%q article_id=%s err=%q", entry.Family, dup.ArticleID, err.Error())
					entry.Error = err.Error()
					stopped = true
				case len(now.ChildArticles) > 0:
					dup.Delete, dup.Skip = false, ytSkipHasChildren
				case now.HasParent():
					dup.Delete, dup.Skip = false, ytSkipNotTopLevel
				case !strings.Contains(now.Content, ytwrite.FamilyRootMarker(entry.Family)):
					dup.Delete, dup.Skip = false, ytSkipMarkerMissing
				default:
					if err := remote.DeleteArticle(ctx, dup.ArticleID); err != nil {
						log.Printf("yt.roots_merge.deleted_failed family=%q article_id=%s err=%q", entry.Family, dup.ArticleID, err.Error())
						entry.Error = err.Error()
						stopped = true
						break
					}
					log.Printf("yt.roots_merge.deleted family=%q article_id=%s", entry.Family, dup.ArticleID)
					entry.Counts.Deleted++
				}
			}
			if stopped {
				ytRootsMergeStop(entry, di)
				break
			}
			if dup.Skip != "" {
				entry.Counts.Skipped++
			}
		}
	}
	report.Counts = ytRootsMergeTotals(report)
	return report
}

// ytRootsMergeStop marks the duplicates from index on that the family's
// failure left undeleted, so the report says what a rerun will take rather
// than claiming the rest of the plan ran.
func ytRootsMergeStop(entry *ytRootsMergeFamily, from int) {
	for di := from; di < len(entry.Duplicates); di++ {
		dup := &entry.Duplicates[di]
		if dup.Skip != "" {
			continue
		}
		dup.Delete, dup.Skip = false, ytSkipFamilyStopped
		entry.Counts.Skipped++
	}
}

// ytRootOlder orders two duplicates: the older creation time first, a root
// YouTrack did not date after one it did (ytwrite's root order), then the id.
func ytRootOlder(a, b ytRootsMergeDuplicate) bool {
	switch {
	case (a.Created == "") != (b.Created == ""):
		return b.Created == ""
	case a.Created != b.Created:
		return a.Created < b.Created
	default:
		return a.ArticleID < b.ArticleID
	}
}

// ytRootsMergeTotals sums the families' counts.
func ytRootsMergeTotals(report ytRootsMergeReport) ytRootsMergeCounts {
	var c ytRootsMergeCounts
	for _, entry := range report.Families {
		c.Moved += entry.Counts.Moved
		c.Deleted += entry.Counts.Deleted
		c.Skipped += entry.Counts.Skipped
		c.Refused += entry.Counts.Refused
	}
	return c
}

// ytHasRoot reports whether the family's roots hold the article.
func ytHasRoot(roots []ytArticleRoot, articleID string) bool {
	for _, r := range roots {
		if r.ArticleID == articleID {
			return true
		}
	}
	return false
}

// failedCount is how many families a failure stopped.
func (r ytRootsMergeReport) failedCount() int {
	n := 0
	for _, entry := range r.Families {
		if entry.Error != "" {
			n++
		}
	}
	return n
}

// failed reports whether any family was stopped by a failure, which makes the
// run incomplete and worth repeating.
func (r ytRootsMergeReport) failed() bool { return r.failedCount() > 0 }

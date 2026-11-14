package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"regexp"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// hierarchyBackfillOp is one proposed parent or related change (hk 3041).
type hierarchyBackfillOp struct {
	Op      string `json:"op"` // "parent" or "related"
	TaskID  int64  `json:"task_id"`
	Target  int64  `json:"target"`
	Title   string `json:"title"`
	Current int64  `json:"current,omitempty"` // parent ops only: current parent, 0 when unset
}

var seriesTitleRE = regexp.MustCompile(`^\[(\d+)\s`)

// seriesParent returns the parent task id encoded in a "[N …]" series title
// such as "[2698 S3] …". The bracket must open the title.
func seriesParent(title string) (int64, bool) {
	m := seriesTitleRE.FindStringSubmatch(title)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || id < 1 {
		return 0, false
	}
	return id, true
}

var (
	followUpKeywordRE  = regexp.MustCompile(`(?i)\bfollow.?ups?\b|\bredo\b|\brespawn\b`)
	hkBeforeKeywordRE  = regexp.MustCompile(`(?i)\bhk\s+#?\d+\s+(?:follow.?ups?|redo|respawn)\b`)
	followUpTargetRE   = regexp.MustCompile(`(?i)\b(?:of|to|from)\s+hk\s+#?(\d+)\b`)
	followUpWindowSize = 60
)

// relatedTarget returns the hk task id a follow-up/redo/respawn title names as
// its origin: the first "of/to/from hk N" after a keyword. A title that also
// carries the "hk N follow-up" form names its origin before the keyword, so the
// trailing "from hk M" is a different reference and the title is skipped.
func relatedTarget(title string) (int64, bool) {
	if hkBeforeKeywordRE.MatchString(title) {
		return 0, false
	}
	for _, loc := range followUpKeywordRE.FindAllStringIndex(title, -1) {
		end := loc[1] + followUpWindowSize
		if end > len(title) {
			end = len(title)
		}
		m := followUpTargetRE.FindStringSubmatch(title[loc[1]:end])
		if m == nil {
			continue
		}
		id, err := strconv.ParseInt(m[1], 10, 64)
		if err == nil && id >= 1 {
			return id, true
		}
	}
	return 0, false
}

// hierarchyBackfillPlan computes the parent and related changes the titles
// call for. A parent is proposed only when it differs from the current one and
// names a task in the set; a related edge is proposed for every title match
// (the server dedupes repeats).
func hierarchyBackfillPlan(tasks []store.Task) (parents, relations []hierarchyBackfillOp) {
	ids := make(map[int64]bool, len(tasks))
	for _, t := range tasks {
		ids[t.ID] = true
	}
	for _, t := range tasks {
		if p, ok := seriesParent(t.Title); ok && p != t.ID && ids[p] {
			var cur int64
			if t.ParentID != nil {
				cur = *t.ParentID
			}
			if cur != p {
				parents = append(parents, hierarchyBackfillOp{Op: "parent", TaskID: t.ID, Target: p, Title: t.Title, Current: cur})
			}
		}
		if r, ok := relatedTarget(t.Title); ok && r != t.ID && ids[r] {
			relations = append(relations, hierarchyBackfillOp{Op: "related", TaskID: t.ID, Target: r, Title: t.Title})
		}
	}
	return parents, relations
}

// hasRelatedEdge reports whether the task already carries a live related edge
// to other. The store normalizes related so the lower id is the source, but
// the check accepts either direction.
func hasRelatedEdge(relations []store.TaskRelation, a, b int64) bool {
	for _, r := range relations {
		if r.Kind == store.RelationRelated && ((r.SrcID == a && r.DstID == b) || (r.SrcID == b && r.DstID == a)) {
			return true
		}
	}
	return false
}

func listAllTasks(ctx context.Context, c *remote.Client) ([]store.Task, error) {
	var all []store.Task
	var afterID int64
	for {
		page, err := c.ListTasksPage(ctx, "", "", "", nil, nil, afterID, 1000)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		all = append(all, page...)
		afterID = page[len(page)-1].ID
		if len(page) < 1000 {
			return all, nil
		}
	}
}

// taskBackfillHierarchyCmd proposes (dry run, the default) or applies the hk
// 3041 hierarchy backfill: "[N …]" series titles get parent N, and
// follow-up/redo/respawn titles get a related edge to the hk task they name.
// Both go through the parent and relation routes, which never run the
// auto-close rule.
//
//	tasks backfill-hierarchy [--apply]
func taskBackfillHierarchyCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks backfill-hierarchy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	apply := fs.Bool("apply", false, "apply the changes (default: dry run)")
	createdBefore := fs.String("created-before", "", "only tasks created before this RFC3339 time (empty: all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: tasks backfill-hierarchy [--apply] [--created-before <RFC3339>]")
	}
	var cutoff time.Time
	if *createdBefore != "" {
		parsed, err := time.Parse(time.RFC3339Nano, *createdBefore)
		if err != nil {
			return errors.New("--created-before must be RFC3339")
		}
		cutoff = parsed
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	all, err := listAllTasks(ctx, c)
	if err != nil {
		return err
	}
	var tasks []store.Task
	for _, t := range all {
		if cutoff.IsZero() || t.CreatedAt.Before(cutoff) {
			tasks = append(tasks, t)
		}
	}
	parents, relations := hierarchyBackfillPlan(tasks)
	pending := relations[:0]
	for _, op := range relations {
		t, found, err := c.GetTask(ctx, op.TaskID)
		if err != nil {
			return err
		}
		if found && hasRelatedEdge(t.Relations, op.TaskID, op.Target) {
			continue
		}
		pending = append(pending, op)
	}
	relations = pending
	result := map[string]any{
		"dry_run":        !*apply,
		"tasks":          len(all),
		"in_scope":       len(tasks),
		"created_before": *createdBefore,
		"parents":        parents,
		"relations":      relations,
	}
	if !*apply {
		return printJSON(out, result)
	}
	type applied struct {
		hierarchyBackfillOp
		Changed bool   `json:"changed"`
		Error   string `json:"error,omitempty"`
	}
	var done []applied
	for _, op := range parents {
		target := op.Target
		_, err := c.SetTaskParent(ctx, op.TaskID, &target)
		a := applied{hierarchyBackfillOp: op}
		if err != nil {
			a.Error = err.Error()
		} else {
			a.Changed = true
		}
		done = append(done, a)
	}
	for _, op := range relations {
		res, err := c.AddTaskRelation(ctx, op.TaskID, store.RelationRelated, op.Target)
		a := applied{hierarchyBackfillOp: op}
		if err != nil {
			a.Error = err.Error()
		} else {
			a.Changed = res.Changed
		}
		done = append(done, a)
	}
	result["applied"] = done
	return printJSON(out, result)
}

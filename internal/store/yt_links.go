package store

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// YouTrack issue-link projection (hk 3841, design/2026-11-13/yt-issue-links).
// This file holds the store side of slice S2: which task writes each link
// (I2) and the set of links a writer wants (§5.1). The issue_links kind, its
// consumer, the bind hook and the sweep come in later slices; nothing calls
// these yet outside tests, and the projection's mode stays off.

// ytLinkKindParent names a parent edge in a desired or held set; the other
// two kinds are the relation kinds RelationRelated and RelationDuplicateOf.
// blocks is not projected.
const ytLinkKindParent = "parent"

// ytLinkTarget is one link a writer wants: the hk task at the other end and
// that task's bound issue on the tenant.
type ytLinkTarget struct {
	TaskID  int64
	IssueID string
}

// ytLinkHeld is an edge whose target task has no binding on the tenant. It is
// neither written nor removed until the target is bound (I14).
type ytLinkHeld struct {
	Kind   string
	TaskID int64
}

// ytLinksDesired is the set of links one writer task wants on its own issue,
// built from current rows. Parent is nil when the task has no parent or the
// parent is held. Slices are sorted by task id and never nil.
type ytLinksDesired struct {
	Parent     *ytLinkTarget
	Related    []ytLinkTarget
	Duplicates []ytLinkTarget
	Held       []ytLinkHeld
}

// ytLinkWriter returns the one task that writes the YouTrack link of an hk
// edge (I2): the child for a parent edge, the duplicate (src) for
// duplicate_of, and the lower id for related, which is the stored src_id
// (normalizeRelation). Two lanes therefore never mutate one link. ok is false
// for a kind that is not projected (blocks).
func ytLinkWriter(kind string, src, dst int64) (writer int64, ok bool) {
	switch kind {
	case ytLinkKindParent, RelationDuplicateOf:
		// For a parent edge src is the child.
		return src, true
	case RelationRelated:
		return min(src, dst), true
	}
	return 0, false
}

// ytLinksDesiredTx builds the desired set of writer task id on tenant: its
// parent, the live related edges where it is src_id (the lower id), and its
// live duplicate_of edges. An edge whose target has no binding on tenant goes
// to Held. found is false when the task does not exist. The caller holds the
// task's lane lock, so the set and the op prepared from it agree.
func ytLinksDesiredTx(ctx context.Context, q pgx.Tx, tenant string, id int64) (ytLinksDesired, bool, error) {
	out := ytLinksDesired{Related: []ytLinkTarget{}, Duplicates: []ytLinkTarget{}, Held: []ytLinkHeld{}}
	var parentID *int64
	var parentIssue *string
	err := q.QueryRow(ctx, `SELECT t.parent_id, b.issue_id FROM tasks t
		LEFT JOIN yt_task_bindings b ON b.task_id = t.parent_id AND b.tenant = $2
		WHERE t.id = $1`, id, tenant).Scan(&parentID, &parentIssue)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytLinksDesired{}, false, nil
	}
	if err != nil {
		return ytLinksDesired{}, false, err
	}
	if parentID != nil {
		if parentIssue == nil {
			out.Held = append(out.Held, ytLinkHeld{Kind: ytLinkKindParent, TaskID: *parentID})
		} else {
			out.Parent = &ytLinkTarget{TaskID: *parentID, IssueID: *parentIssue}
		}
	}
	// Only edges this task writes: related is stored src<dst, so src_id is the
	// writer, and duplicate_of is written by its src.
	rows, err := q.Query(ctx, `SELECT r.kind, r.dst_id, b.issue_id FROM task_relations r
		LEFT JOIN yt_task_bindings b ON b.task_id = r.dst_id AND b.tenant = $2
		WHERE r.src_id = $1 AND r.archived_at IS NULL AND r.kind IN ('related','duplicate_of')
		ORDER BY r.kind, r.dst_id, r.id`, id, tenant)
	if err != nil {
		return ytLinksDesired{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var dst int64
		var issue *string
		if err := rows.Scan(&kind, &dst, &issue); err != nil {
			return ytLinksDesired{}, false, err
		}
		if issue == nil {
			out.Held = append(out.Held, ytLinkHeld{Kind: kind, TaskID: dst})
			continue
		}
		target := ytLinkTarget{TaskID: dst, IssueID: *issue}
		switch kind {
		case RelationRelated:
			out.Related = append(out.Related, target)
		case RelationDuplicateOf:
			out.Duplicates = append(out.Duplicates, target)
		}
	}
	if err := rows.Err(); err != nil {
		return ytLinksDesired{}, false, err
	}
	slices.SortFunc(out.Held, func(a, b ytLinkHeld) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), cmp.Compare(a.TaskID, b.TaskID))
	})
	return out, true, nil
}

// ytLinkWritersOfTx returns, ascending and without repeats, the tasks whose
// desired set points at task id through a live edge: its children, the
// sources of duplicate_of edges into it, and the src of each related edge
// where it is dst_id (the lower id writes). Binding id makes their held edge
// writable (I14), so the bind hook prepares each of them; id itself is not in
// the result.
func ytLinkWritersOfTx(ctx context.Context, q pgx.Tx, id int64) ([]int64, error) {
	rows, err := q.Query(ctx, `SELECT id FROM tasks WHERE parent_id = $1
		UNION
		SELECT src_id FROM task_relations
		WHERE dst_id = $1 AND archived_at IS NULL AND kind IN ('related','duplicate_of')
		ORDER BY 1`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

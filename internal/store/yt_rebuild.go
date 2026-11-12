package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The restore drill (hk 4121, `handoffkeep youtrack audit --rebuild`,
// docs/youtrack-write-protocol.md §Rebuild audit) rebuilds from YouTrack
// what hk needs and diffs it against these reads. The reads give the PG side
// and the id maps (bindings, op links); nothing here reads YouTrack. Every
// read is a plain read, so a read-only store (OpenReadOnly) runs the drill.

// YTRebuildTaskPageMax bounds one page of YTRebuildTasks.
const YTRebuildTaskPageMax = 1000

// YTRebuildTask is one task bound on the drill's tenant: what PG says the
// task is, and the issue its binding names.
type YTRebuildTask struct {
	TaskID        int64
	Title         string
	Project       string
	IssueID       string
	IssueReadable string
}

// YTRebuildTasks lists, in id order, a page of the tasks bound on tenant
// (only project's when project is not ""), with id greater than afterID, at
// most limit. The caller pages with the last id until a page comes back
// short.
func (s *Store) YTRebuildTasks(ctx context.Context, tenant, project string, afterID int64, limit int) ([]YTRebuildTask, error) {
	if tenant == "" || afterID < 0 || limit < 1 || limit > YTRebuildTaskPageMax {
		return nil, errors.New("invalid rebuild task filter")
	}
	rows, err := s.pool.Query(ctx, `SELECT t.id, t.title, coalesce(t.project,''), b.issue_id, b.issue_readable
		FROM yt_task_bindings b JOIN tasks t ON t.id = b.task_id
		WHERE b.tenant = $1 AND ($2 = '' OR t.project = $2) AND t.id > $3 ORDER BY t.id LIMIT $4`, tenant, project, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YTRebuildTask{}
	for rows.Next() {
		var t YTRebuildTask
		if err := rows.Scan(&t.TaskID, &t.Title, &t.Project, &t.IssueID, &t.IssueReadable); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// YTRebuildSampleMax bounds each part of a comment sample.
const YTRebuildSampleMax = 100000

// YTRebuildSample is one sampled task. Top is true for a task picked for its
// comment count, false for a random pick.
type YTRebuildSample struct {
	YTRebuildTask
	Top bool
}

// YTRebuildSampleTasks picks the tasks whose comments the drill compares,
// among the tasks bound on tenant (project's only when project is not ""):
// the top tasks with the most comments that belong on an issue (every one
// except the activity pull's, YouTrackOwnedActor; ties by id, tasks with no
// such comment never), and n more of the rest at random. The random order is
// md5("<seed>:<task id>"), so one seed picks the same tasks on the same
// data, and the run that printed a seed can be repeated. Result in task id
// order.
func (s *Store) YTRebuildSampleTasks(ctx context.Context, tenant, project string, seed int64, n, top int) ([]YTRebuildSample, error) {
	if tenant == "" || n < 0 || top < 0 || n > YTRebuildSampleMax || top > YTRebuildSampleMax {
		return nil, errors.New("invalid rebuild sample")
	}
	rows, err := s.pool.Query(ctx, `WITH bound AS (
			SELECT t.id, t.title, coalesce(t.project,'') AS project, b.issue_id, b.issue_readable,
				(SELECT count(*) FROM task_comments c WHERE c.task_id = t.id AND c.author <> $4) AS n
			FROM yt_task_bindings b JOIN tasks t ON t.id = b.task_id
			WHERE b.tenant = $1 AND ($2 = '' OR t.project = $2)),
		top AS (SELECT id FROM bound WHERE n > 0 ORDER BY n DESC, id LIMIT $5),
		rnd AS (SELECT id FROM bound WHERE id NOT IN (SELECT id FROM top)
			ORDER BY md5($3::text || ':' || id::text), id LIMIT $6)
		SELECT b.id, b.title, b.project, b.issue_id, b.issue_readable, b.id IN (SELECT id FROM top)
		FROM bound b WHERE b.id IN (SELECT id FROM top) OR b.id IN (SELECT id FROM rnd) ORDER BY b.id`,
		tenant, project, strconv.FormatInt(seed, 10), YouTrackOwnedActor, top, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YTRebuildSample{}
	for rows.Next() {
		var x YTRebuildSample
		if err := rows.Scan(&x.TaskID, &x.Title, &x.Project, &x.IssueID, &x.IssueReadable, &x.Top); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// YTRebuildComment is one hk comment as PG has it. OpID is its
// comment_append op on the issue the drill reads (0: none), the id map an
// hk-written YouTrack comment is matched by. LegacyYTID is the YouTrack id
// the legacy mirror push recorded for it (youtrack_comment_links, as
// stored; youtrack.CommentLinkIsID tells an id from a marker).
type YTRebuildComment struct {
	ID         int64
	Author     string
	Body       string
	CreatedAt  time.Time
	OpID       int64
	LegacyYTID string
}

// YTRebuildComments is one task's side of the comment comparison.
type YTRebuildComments struct {
	// Comments are the task's comments that belong on its issue, in id
	// order: every one except the activity pull's (YouTrackOwnedActor).
	Comments []YTRebuildComment
	// PulledYTIDs are the YouTrack comments the pull copied into the task:
	// they came from YouTrack and are left out on both sides.
	PulledYTIDs map[string]bool
}

// YTRebuildTaskComments reads task taskID's comments for the drill against
// issue issueID.
func (s *Store) YTRebuildTaskComments(ctx context.Context, taskID int64, issueID string) (YTRebuildComments, error) {
	out := YTRebuildComments{Comments: []YTRebuildComment{}, PulledYTIDs: map[string]bool{}}
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.author, c.body, c.created_at,
			coalesce((SELECT o.id FROM yt_comment_write_links w JOIN yt_write_ops o ON o.id = w.op_id
				WHERE w.comment_id = c.id AND o.kind = $2 AND o.yt_target_id = $3), 0),
			coalesce((SELECT l.youtrack_comment_id FROM youtrack_comment_links l WHERE l.hk_comment_id = c.id), '')
		FROM task_comments c WHERE c.task_id = $1 ORDER BY c.id`, taskID, ytwrite.KindCommentAppend, issueID)
	if err != nil {
		return YTRebuildComments{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var c YTRebuildComment
		if err := rows.Scan(&c.ID, &c.Author, &c.Body, &c.CreatedAt, &c.OpID, &c.LegacyYTID); err != nil {
			return YTRebuildComments{}, err
		}
		c.CreatedAt = c.CreatedAt.UTC()
		if c.Author == YouTrackOwnedActor {
			if c.LegacyYTID != "" {
				out.PulledYTIDs[c.LegacyYTID] = true
			}
			continue
		}
		out.Comments = append(out.Comments, c)
	}
	return out, rows.Err()
}

// YTRebuildDocument is one document bound to an article on the drill's
// tenant: what PG says the document is, and the article its binding names.
type YTRebuildDocument struct {
	DocumentID int64
	Key        string
	SHA256     string
	BodyBytes  int
	ArticleID  string
}

// YTRebuildDocuments lists, in id order, a page of the documents with a
// yt_article_bindings row on tenant (only those of project's tasks when
// project is not ""), with id greater than afterID, at most limit. The table
// comes with migration 20261112-yt-article-bindings (hk 4053); the caller
// checks it is applied first.
func (s *Store) YTRebuildDocuments(ctx context.Context, tenant, project string, afterID int64, limit int) ([]YTRebuildDocument, error) {
	if tenant == "" || afterID < 0 || limit < 1 || limit > YTRebuildTaskPageMax {
		return nil, errors.New("invalid rebuild document filter")
	}
	rows, err := s.pool.Query(ctx, `SELECT d.id, d.key, d.sha256, octet_length(d.body), b.article_id
		FROM yt_article_bindings b JOIN documents d ON d.id = b.document_id LEFT JOIN tasks t ON t.id = d.task_id
		WHERE b.tenant = $1 AND ($2 = '' OR t.project = $2) AND d.id > $3 ORDER BY d.id LIMIT $4`, tenant, project, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YTRebuildDocument{}
	for rows.Next() {
		var d YTRebuildDocument
		if err := rows.Scan(&d.DocumentID, &d.Key, &d.SHA256, &d.BodyBytes, &d.ArticleID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// YTRebuildCount counts what a skipped target would have checked: what is
// "documents" (every document; only those of project's tasks when project
// is not "") or "decisions" (every decision request, whatever project).
func (s *Store) YTRebuildCount(ctx context.Context, what, project string) (int, error) {
	var n int
	var err error
	switch what {
	case "documents":
		err = s.pool.QueryRow(ctx, `SELECT count(*) FROM documents d LEFT JOIN tasks t ON t.id = d.task_id WHERE $1 = '' OR t.project = $1`, project).Scan(&n)
	case "decisions":
		err = s.pool.QueryRow(ctx, `SELECT count(*) FROM decision_requests`).Scan(&n)
	default:
		err = errors.New("invalid rebuild count")
	}
	return n, err
}

// YTRebuildDecision is one decision bound to a DEC issue on the drill's
// tenant. Text is the decision's DEC issue text as PG has it (the same
// decisionTextOf the projection writes); Deleted is set when the decision
// row is gone and the binding is left.
type YTRebuildDecision struct {
	DecisionID int64
	IssueID    string
	Text       ytwrite.DecisionText
	Deleted    bool
}

// YTRebuildDecisions lists, in decision id order, a page of the decisions
// with a yt_decision_bindings row on tenant, with id greater than afterID, at
// most limit. The table comes with migration YTDecisionBindingsMigration
// (hk 4054); the caller checks it is applied first. One read transaction, so
// a page's bindings and decisions are read at one snapshot.
func (s *Store) YTRebuildDecisions(ctx context.Context, tenant string, afterID int64, limit int) ([]YTRebuildDecision, error) {
	if tenant == "" || afterID < 0 || limit < 1 || limit > YTRebuildTaskPageMax {
		return nil, errors.New("invalid rebuild decision filter")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT decision_id::bigint, issue_id FROM yt_decision_bindings
		WHERE tenant = $1 AND decision_id::bigint > $2 ORDER BY decision_id::bigint LIMIT $3`, tenant, afterID, limit)
	if err != nil {
		return nil, err
	}
	out := []YTRebuildDecision{}
	ids := []int64{}
	for rows.Next() {
		var d YTRebuildDecision
		if err := rows.Scan(&d.DecisionID, &d.IssueID); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
		ids = append(ids, d.DecisionID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byID, err := readYTDecisionRows(ctx, tx, ids, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	for i := range out {
		r := byID[out[i].DecisionID]
		if r == nil {
			out[i].Deleted = true
			continue
		}
		out[i].Text = decisionTextOf(r.record, r.channel)
	}
	return out, tx.Commit(ctx)
}

package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// YTLinksMode gates task-link snapshots (HANDOFFKEEP_YT_LINKS_MODE). It stays
// off until the YouTrack import has ended.
type YTLinksMode string

const (
	YTLinksOff YTLinksMode = "off"
	YTLinksOn  YTLinksMode = "on"
)

var ErrYTLinksOff = errors.New("yt_links_off")

const (
	YTLinksPrincipal = "hk:task-links"
	ytLinksConsumer  = "task_links"
)

type ytLinksGate struct {
	mode   YTLinksMode
	tenant string
}

// SetYTLinksMode sets the link projection gate and its YouTrack tenant.
func (s *Store) SetYTLinksMode(mode YTLinksMode, tenant string) {
	s.ytLinks.Store(&ytLinksGate{mode: mode, tenant: tenant})
}

func (s *Store) ytLinksNow() (YTLinksMode, string) {
	g := s.ytLinks.Load()
	if g == nil || g.mode != YTLinksOn || g.tenant == "" {
		return YTLinksOff, ""
	}
	return YTLinksOn, g.tenant
}

func init() {
	registerYTKindGate(ytwrite.KindIssueLinks, ytLinksGateSQL)
	registerYTFeedStoreConsumer(ytLinksConsumer, []string{"task"}, ytLinksHandler)
}

func ytLinksGateSQL(s *Store) string {
	if mode, _ := s.ytLinksNow(); mode != YTLinksOn {
		return `FALSE`
	}
	return `o.entity_kind = 'task' AND EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)`
}

type ytLinksOutcome string

const (
	ytLinksPrepared  ytLinksOutcome = "prepared"
	ytLinksUnchanged ytLinksOutcome = "unchanged"
	ytLinksSkipped   ytLinksOutcome = "skipped"
	ytLinksUnmapped  ytLinksOutcome = "unmapped_target"
	ytLinksInvalid   ytLinksOutcome = "invalid"
)

// ytLinksHandler reads current graph state for each changed task and prepares
// one idempotent snapshot op. It runs after the import cutover only.
func ytLinksHandler(s *Store, now time.Time) FeedHandler {
	return func(ctx context.Context, tx pgx.Tx, rows []FeedRow) error {
		mode, tenant := s.ytLinksNow()
		if mode != YTLinksOn {
			return nil
		}
		counts := map[ytLinksOutcome]int{}
		ids := make([]int64, 0, len(rows))
		for _, row := range rows {
			if row.Model != "task" {
				continue
			}
			id, err := strconv.ParseInt(row.ModelID, 10, 64)
			if err != nil || id < 1 {
				continue
			}
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		ids = slices.Compact(ids)
		for _, id := range ids {
			outcome, err := prepareYTLinksTx(ctx, tx, now, tenant, id)
			if err != nil {
				return err
			}
			counts[outcome]++
		}
		log.Printf("ytwrite.links_feed tasks=%d prepared=%d unchanged=%d skipped=%d unmapped_target=%d invalid=%d",
			len(rows), counts[ytLinksPrepared], counts[ytLinksUnchanged], counts[ytLinksSkipped], counts[ytLinksUnmapped], counts[ytLinksInvalid])
		return nil
	}
}

func prepareYTLinksTx(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, id int64) (ytLinksOutcome, error) {
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(id, 10)); err != nil {
		return "", err
	}
	b, found, err := readYTBinding(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if !found || b.Tenant != tenant {
		return ytLinksSkipped, nil
	}
	if source, err := ytBindingDisagreement(ctx, tx, b); err != nil {
		return "", err
	} else if source != "" {
		log.Printf("ytwrite.links_skipped task_id=%d reason=binding_disagreement source=%q", id, source)
		return ytLinksSkipped, nil
	}
	var parentID *int64
	if err := tx.QueryRow(ctx, `SELECT parent_id FROM tasks WHERE id=$1`, id).Scan(&parentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ytLinksSkipped, nil
		}
		return "", err
	}
	links := ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}}
	if parentID != nil {
		var issue string
		err := tx.QueryRow(ctx, `SELECT issue_id FROM yt_task_bindings WHERE task_id=$1 AND tenant=$2`, *parentID, tenant).Scan(&issue)
		if errors.Is(err, pgx.ErrNoRows) {
			return ytLinksUnmapped, nil
		}
		if err != nil {
			return "", err
		}
		links.Parent = issue
	}
	rels, err := tx.Query(ctx, `SELECT r.kind, CASE WHEN r.src_id=$1 THEN r.dst_id ELSE r.src_id END AS other_id, b.issue_id
		FROM task_relations r
		LEFT JOIN yt_task_bindings b ON b.task_id=CASE WHEN r.src_id=$1 THEN r.dst_id ELSE r.src_id END AND b.tenant=$2
		WHERE r.archived_at IS NULL AND r.kind IN ('related','duplicate_of')
		AND (r.src_id=$1 OR (r.kind='related' AND r.dst_id=$1))
		ORDER BY r.kind,other_id`, id, tenant)
	if err != nil {
		return "", err
	}
	for rels.Next() {
		var kind string
		var dst int64
		var issue *string
		if err := rels.Scan(&kind, &dst, &issue); err != nil {
			rels.Close()
			return "", err
		}
		if issue == nil {
			rels.Close()
			return ytLinksUnmapped, nil
		}
		switch kind {
		case RelationRelated:
			links.Related = append(links.Related, *issue)
		case RelationDuplicateOf:
			links.Duplicates = append(links.Duplicates, *issue)
		}
	}
	if err := rels.Err(); err != nil {
		rels.Close()
		return "", err
	}
	rels.Close()
	sort.Strings(links.Related)
	sort.Strings(links.Duplicates)
	previous, err := previousYTLinksTx(ctx, tx, tenant, id, b.IssueID)
	if err != nil {
		return "", err
	}
	n, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{Principal: YTLinksPrincipal, Actor: YTLinksPrincipal, Tenant: tenant,
		IssueID: b.IssueID, TaskID: id, Previous: previous, Links: links})
	if errors.Is(err, ytwrite.ErrInvalid) {
		return ytLinksInvalid, nil
	}
	if err != nil {
		return "", err
	}
	op, made, err := prepareYTSnapshotTx(ctx, tx, now, n, n.PayloadSHA)
	if err != nil {
		return "", err
	}
	if !made {
		return ytLinksUnchanged, nil
	}
	log.Printf("ytwrite.links_enqueued op_id=%d task_id=%d issue_id=%q", op.ID, id, b.IssueID)
	return ytLinksPrepared, nil
}

func previousYTLinksTx(ctx context.Context, tx pgx.Tx, tenant string, taskID int64, issueID string) (ytwrite.IssueLinks, error) {
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM yt_write_ops
		WHERE principal=$1 AND kind=$2 AND entity_kind='task' AND entity_id=$3 AND yt_target_id=$4 AND tenant=$5
		AND state IN ('prepared','dispatched','remote_unknown','applied')
		ORDER BY id DESC LIMIT 1`, YTLinksPrincipal, ytwrite.KindIssueLinks, strconv.FormatInt(taskID, 10), issueID, tenant).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}}, nil
	}
	if err != nil {
		return ytwrite.IssueLinks{}, err
	}
	snapshot, err := ytwrite.DecodeIssueLinksSnapshot(payload)
	if err != nil {
		return ytwrite.IssueLinks{}, err
	}
	return snapshot.Desired, nil
}

// claimYTLinksOwnershipTx ensures every link a snapshot may remove was named
// by a prior applied hk issue_links op. Pending snapshots can describe a
// prospective baseline, but they do not grant deletion authority until the
// preceding op has applied.
func claimYTLinksOwnershipTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op) (string, error) {
	snapshot, err := ytwrite.IssueLinksPayload(op)
	if err != nil {
		return "", err
	}
	var payloads [][]byte
	rows, err := tx.Query(ctx, `SELECT payload FROM yt_write_ops
		WHERE principal=$1 AND kind=$2 AND entity_kind='task' AND entity_id=$3 AND yt_target_id=$4 AND tenant=$5
		AND state='applied' AND id<$6 ORDER BY id`, op.Principal, ytwrite.KindIssueLinks, op.EntityID, op.YTTargetID, op.Tenant, op.ID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return "", err
		}
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	ownedParent := []string{}
	ownedRelated := []string{}
	ownedDuplicates := []string{}
	for _, payload := range payloads {
		prior, err := ytwrite.DecodeIssueLinksSnapshot(payload)
		if err != nil {
			return "", err
		}
		if prior.Desired.Parent != "" {
			ownedParent = append(ownedParent, prior.Desired.Parent)
		}
		ownedRelated = append(ownedRelated, prior.Desired.Related...)
		ownedDuplicates = append(ownedDuplicates, prior.Desired.Duplicates...)
	}
	if snapshot.Previous.Parent != "" && !containsYTLink(ownedParent, snapshot.Previous.Parent) {
		return "unowned parent link to " + snapshot.Previous.Parent, nil
	}
	for _, id := range snapshot.Previous.Related {
		if !containsYTLink(ownedRelated, id) {
			return "unowned relates link to " + id, nil
		}
	}
	for _, id := range snapshot.Previous.Duplicates {
		if !containsYTLink(ownedDuplicates, id) {
			return "unowned duplicate link to " + id, nil
		}
	}
	return "", nil
}

func containsYTLink(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func (s *Store) ytLinksOnBindTx(ctx context.Context, tx pgx.Tx, taskIDs []int64, now time.Time) error {
	mode, tenant := s.ytLinksNow()
	if mode != YTLinksOn {
		return nil
	}
	for _, taskID := range taskIDs {
		outcome, err := prepareYTLinksTx(ctx, tx, now, tenant, taskID)
		if err != nil {
			return err
		}
		log.Printf("ytwrite.links_on_bind task_id=%d outcome=%s", taskID, outcome)
	}
	return nil
}

// BackfillYTLinksOps prepares current snapshots for bound tasks in one tenant.
// afterID and limit make the walk resumable between worker passes.
func (s *Store) BackfillYTLinksOps(ctx context.Context, afterID int64, limit int) (nextID int64, done bool, err error) {
	mode, tenant := s.ytLinksNow()
	if mode != YTLinksOn {
		return afterID, false, ErrYTLinksOff
	}
	if afterID < 0 || limit < 1 {
		return afterID, false, errors.New("backfill cursor must be nonnegative and limit must be positive")
	}
	rows, err := s.pool.Query(ctx, `SELECT task_id FROM yt_task_bindings WHERE tenant=$1 AND task_id>$2 ORDER BY task_id LIMIT $3`, tenant, afterID, limit)
	if err != nil {
		return afterID, false, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return afterID, false, err
	}
	for _, id := range ids {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return nextID, false, err
		}
		if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(id, 10)); err != nil {
			_ = tx.Rollback(ctx)
			return nextID, false, err
		}
		if _, err := prepareYTLinksTx(ctx, tx, time.Now().UTC(), tenant, id); err != nil {
			_ = tx.Rollback(ctx)
			return nextID, false, fmt.Errorf("task %d: %w", id, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nextID, false, err
		}
		nextID = id
	}
	return nextID, len(ids) < limit, nil
}

func ytLinksBindingTasksTx(ctx context.Context, tx pgx.Tx, taskID int64) ([]int64, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM tasks WHERE parent_id=$1
		UNION
		SELECT CASE WHEN r.src_id=$1 THEN r.dst_id ELSE r.src_id END
		FROM task_relations r WHERE r.archived_at IS NULL AND (
			(r.kind='related' AND (r.src_id=$1 OR r.dst_id=$1)) OR
			(r.kind='duplicate_of' AND r.dst_id=$1))
		ORDER BY 1`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{taskID}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return slices.Compact(ids), nil
}

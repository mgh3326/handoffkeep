package store

import (
	"context"
	"errors"
	"log"
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
		for _, row := range rows {
			if row.Model != "task" {
				continue
			}
			id, err := strconv.ParseInt(row.ModelID, 10, 64)
			if err != nil || id < 1 {
				continue
			}
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
	rels, err := tx.Query(ctx, `SELECT r.kind,r.dst_id,b.issue_id FROM task_relations r LEFT JOIN yt_task_bindings b ON b.task_id=r.dst_id AND b.tenant=$2
		WHERE r.src_id=$1 AND r.archived_at IS NULL AND r.kind IN ('related','duplicate_of') ORDER BY r.kind,r.dst_id`, id, tenant)
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
	n, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{Principal: YTLinksPrincipal, Actor: YTLinksPrincipal, Tenant: tenant,
		IssueID: b.IssueID, TaskID: id, Links: links})
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

func (s *Store) ytLinksOnBindTx(ctx context.Context, tx pgx.Tx, taskID int64, now time.Time) error {
	mode, tenant := s.ytLinksNow()
	if mode != YTLinksOn {
		return nil
	}
	outcome, err := prepareYTLinksTx(ctx, tx, now, tenant, taskID)
	if err != nil {
		return err
	}
	log.Printf("ytwrite.links_on_bind task_id=%d outcome=%s", taskID, outcome)
	return nil
}

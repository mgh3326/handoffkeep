package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// `youtrack links plan` (hk 4731, design/2026-11-13/yt-issue-links §9 S5,
// docs/youtrack-write-protocol.md "Canary runbook"): the read-only report of
// what the first issue-link sweep would write. It walks the bound tasks in the
// sweep's order (RunYTLinksSweep: ascending task id, ascending from the
// start) and, per task, builds the desired set the sweep would prepare
// (ytLinksDesiredTx, the writer rule, the held list) and reads the issue's
// links with the S1 adapter. It classifies every link with PlanIssueLinks, the
// pure function the send path's ledger plans with, so the report and the send
// agree on ownership: only a link an open yt_link_owned row names can be
// deleted, and a re-parent is a delete plus an add.
//
// It writes nothing. Each task is read in one read-only, repeatable-read
// transaction, which is rolled back before the YouTrack read; the only request
// the walk makes is a GET. No op, no owned or unowned row, no plan row, no
// feed row, no sweep cursor and no task row changes, so the mode may stay off
// (it does not consult the gate at all).

// YTLinksPlanOptions selects the tasks of one plan run. Project and TaskID
// narrow the walk ("" and 0 for every bound task of the tenant); Limit caps
// the bound tasks visited (0 for no cap), as one sweep slice does.
type YTLinksPlanOptions struct {
	Project string
	TaskID  int64
	Limit   int
}

// YTLinkPlanCounts is one class count of a task's plan (or of the run).
type YTLinkPlanCounts struct {
	// WouldAdd counts desired links the read did not show.
	WouldAdd int `json:"would_add"`
	// WouldDeleteOwned counts links this writer owns and no longer wants:
	// the only links a send deletes (I1).
	WouldDeleteOwned int `json:"would_delete_owned"`
	// UnownedLeft counts managed-type links nobody in hk created, import-era
	// links included: recorded and left, never deleted (I7).
	UnownedLeft int `json:"unowned_left"`
	// Held counts edges whose target task has no binding on the tenant: they
	// are neither written nor removed (I14).
	Held int `json:"held"`
	// AlreadyOK counts desired links the read already showed.
	AlreadyOK int `json:"already_ok"`
}

func (c *YTLinkPlanCounts) add(o YTLinkPlanCounts) {
	c.WouldAdd += o.WouldAdd
	c.WouldDeleteOwned += o.WouldDeleteOwned
	c.UnownedLeft += o.UnownedLeft
	c.Held += o.Held
	c.AlreadyOK += o.AlreadyOK
}

// YTLinkPlanLink is one link a row's write names.
type YTLinkPlanLink struct {
	Kind          string `json:"kind"`
	TargetTaskID  int64  `json:"target_task_id"`
	TargetIssueID string `json:"target_issue_id"`
}

// YTLinkPlanRow is one bound writer task's plan. It is reported when the task
// has a link to add or delete, and Add and Delete name them.
type YTLinkPlanRow struct {
	TaskID  int64  `json:"task_id"`
	IssueID string `json:"issue_id"`
	YTLinkPlanCounts
	Add    []YTLinkPlanLink `json:"add"`
	Delete []YTLinkPlanLink `json:"delete"`
}

// YTLinksPlanReport is what `youtrack links plan` found. Tasks counts the
// bound tasks planned, Skipped the bound tasks the sweep's rule would skip (a
// binding a local source disagrees with, a deleted task, invalid data); Held,
// unowned and already-ok counts of skipped tasks are not in Totals. Gets is
// the number of YouTrack GET requests the walk made; the command sets it from
// its client (the store only reads through the remote it is given).
type YTLinksPlanReport struct {
	Mode    YTLinksMode      `json:"mode"`
	Tenant  string           `json:"tenant"`
	TaskID  int64            `json:"task_id,omitempty"`
	Project string           `json:"project,omitempty"`
	Limit   int              `json:"limit,omitempty"`
	Tasks   int              `json:"tasks"`
	Skipped int              `json:"skipped"`
	Gets    int              `json:"gets"`
	Totals  YTLinkPlanCounts `json:"totals"`
	Rows    []YTLinkPlanRow  `json:"rows"`
}

// PlanYTLinks plans every bound writer task opt selects on the configured
// tenant, in ascending task id, each in its own read-only transaction, and
// reads its issue's links through remote. An unknown opt.TaskID is
// ErrTaskNotFound. Nothing is written.
func (s *Store) PlanYTLinks(ctx context.Context, opt YTLinksPlanOptions, remote ytwrite.IssueLinksRemote) (YTLinksPlanReport, error) {
	mode, tenant := s.ytLinksSetting()
	r := YTLinksPlanReport{Mode: mode, Tenant: tenant, TaskID: opt.TaskID, Project: opt.Project, Limit: opt.Limit,
		Rows: []YTLinkPlanRow{}}
	if opt.Limit < 0 || opt.TaskID < 0 {
		return r, errors.New("plan limit and task id must not be negative")
	}
	if tenant == "" {
		return r, errors.New("youtrack links plan needs a YouTrack tenant")
	}
	if opt.TaskID > 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)`, opt.TaskID).Scan(&exists); err != nil {
			return r, err
		}
		if !exists {
			return r, ErrTaskNotFound
		}
	}
	// The sweep's walk (yt_task_bindings, ascending), narrowed by the filters.
	q := `SELECT b.task_id FROM yt_task_bindings b
		JOIN tasks t ON t.id = b.task_id
		WHERE b.tenant = $1 AND ($2 = '' OR t.project = $2) AND ($3 = 0 OR b.task_id = $3)
		ORDER BY b.task_id`
	args := []any{tenant, opt.Project, opt.TaskID}
	if opt.Limit > 0 {
		q += ` LIMIT $4`
		args = append(args, opt.Limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return r, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return r, err
	}
	for _, id := range ids {
		row, planned, err := s.planYTLinksTask(ctx, tenant, id, remote)
		if err != nil {
			return r, fmt.Errorf("task %d: %w", id, err)
		}
		if !planned {
			r.Skipped++
			continue
		}
		r.Tasks++
		r.Totals.add(row.YTLinkPlanCounts)
		if row.WouldAdd > 0 || row.WouldDeleteOwned > 0 {
			r.Rows = append(r.Rows, row)
		}
	}
	return r, nil
}

// planYTLinksTask is one task's plan: the binding, desired set, owned rows and
// peer edges in one read-only transaction, then the issue's links (a GET),
// then PlanIssueLinks. planned is false for a task the sweep's rule skips: no
// binding on the tenant, a binding a local source disagrees with, a task that
// no longer exists, or a desired set the payload check refuses. No lane lock
// is taken: a read-only report must not wait on a blocked lane, and the write
// path re-plans under the lock before it sends.
func (s *Store) planYTLinksTask(ctx context.Context, tenant string, id int64, remote ytwrite.IssueLinksRemote) (YTLinkPlanRow, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return YTLinkPlanRow{}, false, err
	}
	defer tx.Rollback(ctx)
	b, found, err := readYTBinding(ctx, tx, id)
	if err != nil {
		return YTLinkPlanRow{}, false, err
	}
	if !found || b.Tenant != tenant {
		return YTLinkPlanRow{}, false, nil
	}
	source, err := ytBindingDisagreement(ctx, tx, b)
	if err != nil {
		return YTLinkPlanRow{}, false, err
	}
	if source != "" {
		return YTLinkPlanRow{}, false, nil
	}
	d, exists, err := ytLinksDesiredTx(ctx, tx, tenant, id)
	if err != nil {
		return YTLinkPlanRow{}, false, err
	}
	if !exists {
		return YTLinkPlanRow{}, false, nil
	}
	snap, err := ytwrite.IssueLinksSnapshotOf(ytLinksRequest(tenant, b.IssueID, id, d))
	if errors.Is(err, ytwrite.ErrInvalid) {
		return YTLinkPlanRow{}, false, nil
	}
	if err != nil {
		return YTLinkPlanRow{}, false, err
	}
	owned, _, err := ytLinkRowsTx(ctx, tx, tenant, b.IssueID, true)
	if err != nil {
		return YTLinkPlanRow{}, false, err
	}
	peers, err := ytLinkPeerEdgesTx(ctx, tx, tenant, id)
	if err != nil {
		return YTLinkPlanRow{}, false, err
	}
	if err := tx.Rollback(ctx); err != nil {
		return YTLinkPlanRow{}, false, err
	}
	read, err := remote.ReadIssueLinks(ctx, b.IssueID)
	if err != nil {
		return YTLinkPlanRow{}, false, fmt.Errorf("read links of issue %s: %w", b.IssueID, err)
	}
	op := ytwrite.Op{Kind: ytwrite.KindIssueLinks, EntityKind: "task", EntityID: strconv.FormatInt(id, 10),
		YTTargetID: b.IssueID, Tenant: tenant}
	plan, _, err := ytwrite.PlanIssueLinks(op, snap, read.Links, owned, peers)
	if err != nil {
		return YTLinkPlanRow{}, false, err
	}
	// The target task of each link: the desired set names an add's, and the
	// owned rows name a delete's (a delete is no longer desired, so only its
	// row knows the task).
	tasks := map[ytwrite.IssueLink]int64{}
	for _, d := range snap.Links() {
		tasks[d.Link] = d.TaskID
	}
	for _, o := range owned {
		if !o.Open() || o.From != b.IssueID || o.WriterTask != id {
			continue
		}
		if l := (ytwrite.IssueLink{Kind: o.LinkType, Target: o.To}); tasks[l] == 0 {
			tasks[l] = o.TargetTask
		}
	}
	row := YTLinkPlanRow{TaskID: id, IssueID: b.IssueID, Add: []YTLinkPlanLink{}, Delete: []YTLinkPlanLink{}}
	present := make(map[ytwrite.IssueLink]bool, len(read.Links))
	for _, l := range read.Links {
		present[l] = true
	}
	for _, d := range snap.Links() {
		if present[d.Link] {
			row.AlreadyOK++
		}
	}
	link := func(l ytwrite.IssueLink) YTLinkPlanLink {
		return YTLinkPlanLink{Kind: string(l.Kind), TargetTaskID: tasks[l], TargetIssueID: l.Target}
	}
	row.Held = len(snap.Held)
	row.UnownedLeft = len(plan.Unowned)
	row.WouldAdd = len(plan.Add)
	row.WouldDeleteOwned = len(plan.Remove)
	for _, l := range plan.Add {
		row.Add = append(row.Add, link(l))
	}
	for _, l := range plan.Remove {
		row.Delete = append(row.Delete, link(l))
	}
	return row, true, nil
}

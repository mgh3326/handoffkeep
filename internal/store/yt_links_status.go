package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// `youtrack links status` (hk 4640, design §9 S5): the operator's read of the
// issue-link projection. It changes no state. The rows are read for the
// tenant this process writes to, also when the mode is off, so links written
// while the mode was on stay visible; with no tenant configured there is
// nothing to read. One snapshot (REPEATABLE READ, read-only) covers the sweep
// row and every list, so the numbers agree.

// Held reasons that are not ytwrite reason codes: the op was never sent.
const (
	// YTLinksHeldModeOff: the mode is off or no tenant is set, so the gate
	// admits no issue_links op.
	YTLinksHeldModeOff = "mode_off"
	// YTLinksHeldTargetUnbound: the op names an issue its writer task is not
	// bound to on the op's tenant (the claim rejects it as link_target_changed
	// once the stale rule runs).
	YTLinksHeldTargetUnbound = "target_unbound"
)

// YTLinksSweepView is the sweep row (yt_links_sweep). Complete: the walk
// finished at the current generation.
type YTLinksSweepView struct {
	Generation     int64 `json:"generation"`
	DoneGeneration int64 `json:"done_generation"`
	AfterTaskID    int64 `json:"after_task_id"`
	Complete       bool  `json:"complete"`
}

// YTLinkOwnedRow is one open yt_link_owned row: a link hk created.
type YTLinkOwnedRow struct {
	ID           int64     `json:"id"`
	LinkType     string    `json:"link_type"`
	Edge         string    `json:"edge"`
	FromIssue    string    `json:"from_issue"`
	ToIssue      string    `json:"to_issue"`
	WriterTaskID int64     `json:"writer_task_id"`
	TargetTaskID int64     `json:"target_task_id"`
	IntendedOpID int64     `json:"intended_op_id"`
	IntendedAt   time.Time `json:"intended_at"`
}

// YTLinkUnownedRow is one open yt_link_unowned row: a managed-type link hk
// did not create, seen from IssueID and left alone.
type YTLinkUnownedRow struct {
	ID          int64     `json:"id"`
	IssueID     string    `json:"issue_id"`
	LinkType    string    `json:"link_type"`
	Side        string    `json:"side"`
	OtherIssue  string    `json:"other_issue"`
	FirstSeenOp int64     `json:"first_seen_op_id"`
	LastSeenOp  int64     `json:"last_seen_op_id"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// YTLinkHeldOp is an issue_links op that has not been sent and will not be
// while Reason holds, or one left remote_unknown. Its lane is blocked by it.
type YTLinkHeldOp struct {
	OpID      int64     `json:"op_id"`
	TaskID    int64     `json:"task_id"`
	YTIssueID string    `json:"yt_target_id"`
	State     string    `json:"state"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// YTLinksStatus is the body of GET /v1/yt/links/status. Owned, Unowned and
// Held hold at most Limit rows each, oldest first; the *Open and HeldCount
// fields count every matching row. TaskID 0 means every task of the tenant.
type YTLinksStatus struct {
	Mode        YTLinksMode        `json:"mode"`
	Tenant      string             `json:"tenant"`
	Open        bool               `json:"open"`
	TaskID      int64              `json:"task_id"`
	Limit       int                `json:"limit"`
	Sweep       YTLinksSweepView   `json:"sweep"`
	OwnedOpen   int                `json:"owned_open"`
	Owned       []YTLinkOwnedRow   `json:"owned"`
	UnownedOpen int                `json:"unowned_open"`
	Unowned     []YTLinkUnownedRow `json:"unowned"`
	HeldCount   int                `json:"held_count"`
	Held        []YTLinkHeldOp     `json:"held"`
}

// ytLinksSetting is the gate as configured: its mode and tenant, whatever the
// mode is. ytLinksNow answers only for an open gate.
func (s *Store) ytLinksSetting() (YTLinksMode, string) {
	g := s.ytLinks.Load()
	if g == nil {
		return YTLinksOff, ""
	}
	return g.mode, g.tenant
}

// YTLinksStatusOf reads the link projection's state for tenant (the gate's)
// and, when task > 0, for that task alone. An unknown task is ErrTaskNotFound.
func (s *Store) YTLinksStatusOf(ctx context.Context, task int64, limit int) (YTLinksStatus, error) {
	mode, tenant := s.ytLinksSetting()
	out := YTLinksStatus{
		Mode: mode, Tenant: tenant, Open: mode == YTLinksOn && tenant != "",
		TaskID: task, Limit: limit,
		Owned: []YTLinkOwnedRow{}, Unowned: []YTLinkUnownedRow{}, Held: []YTLinkHeldOp{},
	}
	// One read-only REPEATABLE READ snapshot covers the sweep row and every
	// list on both dialects: beginSnapshotA5 opens the same PostgreSQL
	// transaction options this path used to name inline.
	tx, err := s.beginSnapshotA5(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if task > 0 {
		var exists bool
		if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id=$1)`,
			`SELECT EXISTS (SELECT 1 FROM tasks WHERE id=?)`), task).Scan(&exists); err != nil {
			return out, err
		}
		if !exists {
			return out, ErrTaskNotFound
		}
	}
	st, err := ytLinksSweepStateTx(ctx, tx)
	if err != nil {
		return out, err
	}
	out.Sweep = YTLinksSweepView{Generation: st.Generation, DoneGeneration: st.DoneGeneration, AfterTaskID: st.AfterTaskID, Complete: st.Complete()}

	if err := statusOwnedTx(ctx, tx, &out, tenant, task, limit); err != nil {
		return out, err
	}
	if err := statusUnownedTx(ctx, tx, &out, tenant, task, limit); err != nil {
		return out, err
	}
	if err := statusHeldTx(ctx, tx, &out, tenant, task, limit); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

// statusOwnedTx fills the open ownership rows of tenant touching task (either
// end; every task when task is 0).
func statusOwnedTx(ctx context.Context, tx transaction, out *YTLinksStatus, tenant string, task int64, limit int) error {
	const where = `tenant=$1 AND released_at IS NULL AND ($2::bigint = 0 OR writer_task_id=$2 OR target_task_id=$2)`
	const whereMySQL = `tenant=? AND released_at IS NULL AND (? = 0 OR writer_task_id=? OR target_task_id=?)`
	var err error
	if dialectFor(tx) == mysqlDialect {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM yt_link_owned WHERE `+whereMySQL, tenant, task, task, task).Scan(&out.OwnedOpen)
	} else {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM yt_link_owned WHERE `+where, tenant, task).Scan(&out.OwnedOpen)
	}
	if err != nil {
		return err
	}
	var rows rows
	if dialectFor(tx) == mysqlDialect {
		rows, err = tx.Query(ctx, `SELECT `+ytLinkOwnedColumns+` FROM yt_link_owned WHERE `+whereMySQL+` ORDER BY id LIMIT ?`,
			tenant, task, task, task, limit)
	} else {
		rows, err = tx.Query(ctx, `SELECT `+ytLinkOwnedColumns+` FROM yt_link_owned WHERE `+where+` ORDER BY id LIMIT $3`,
			tenant, task, limit)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scanYTLinkOwned(rows)
		if err != nil {
			return err
		}
		out.Owned = append(out.Owned, ownedRow(o))
	}
	return rows.Err()
}

func ownedRow(o ytwrite.OwnedLink) YTLinkOwnedRow {
	return YTLinkOwnedRow{ID: o.ID, LinkType: string(o.LinkType), Edge: o.Edge, FromIssue: o.From, ToIssue: o.To,
		WriterTaskID: o.WriterTask, TargetTaskID: o.TargetTask, IntendedOpID: o.IntendedOp, IntendedAt: o.IntendedAt}
}

// statusUnownedTx fills the open unowned rows of tenant seen from task's
// bound issue (every issue of the tenant when task is 0).
func statusUnownedTx(ctx context.Context, tx transaction, out *YTLinksStatus, tenant string, task int64, limit int) error {
	const where = `tenant=$1 AND gone_at IS NULL AND ($2::bigint = 0 OR issue_id IN (SELECT b.issue_id FROM yt_task_bindings b WHERE b.task_id=$2 AND b.tenant=$1))`
	const whereMySQL = `tenant=? AND gone_at IS NULL AND (? = 0 OR issue_id IN (SELECT b.issue_id FROM yt_task_bindings b WHERE b.task_id=? AND b.tenant=?))`
	var err error
	if dialectFor(tx) == mysqlDialect {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM yt_link_unowned WHERE `+whereMySQL, tenant, task, task, tenant).Scan(&out.UnownedOpen)
	} else {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM yt_link_unowned WHERE `+where, tenant, task).Scan(&out.UnownedOpen)
	}
	if err != nil {
		return err
	}
	var rows rows
	if dialectFor(tx) == mysqlDialect {
		rows, err = tx.Query(ctx, `SELECT `+ytLinkUnownedColumns+` FROM yt_link_unowned WHERE `+whereMySQL+` ORDER BY id LIMIT ?`,
			tenant, task, task, tenant, limit)
	} else {
		rows, err = tx.Query(ctx, `SELECT `+ytLinkUnownedColumns+` FROM yt_link_unowned WHERE `+where+` ORDER BY id LIMIT $3`,
			tenant, task, limit)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanYTLinkUnowned(rows)
		if err != nil {
			return err
		}
		out.Unowned = append(out.Unowned, YTLinkUnownedRow{ID: u.ID, IssueID: u.IssueID, LinkType: string(u.LinkType), Side: u.Side,
			OtherIssue: u.OtherIssue, FirstSeenOp: u.FirstSeenOp, LastSeenOp: u.LastSeenOp, LastSeenAt: u.LastSeenAt})
	}
	return rows.Err()
}

// statusHeldTx fills the issue_links ops that hold a lane or wait to be sent:
// every remote_unknown one, and every prepared one the gate does not admit
// (the mode is not open, or its target is not the writer's binding on the
// op's tenant) or that waits for an earlier unfinished op on its entity.
// The same test Due applies, read as a reason.
func statusHeldTx(ctx context.Context, tx transaction, out *YTLinksStatus, tenant string, task int64, limit int) error {
	entity := ""
	if task > 0 {
		entity = strconv.FormatInt(task, 10)
	}
	const inner = `SELECT o.id, o.entity_id, o.yt_target_id, o.state, o.created_at,
		EXISTS (SELECT 1 FROM yt_task_bindings b WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant) AS bound,
		EXISTS (SELECT 1 FROM yt_write_ops e WHERE e.entity_kind = o.entity_kind AND e.entity_id = o.entity_id AND e.id < o.id AND e.state NOT IN ('applied','rejected')) AS behind
		FROM yt_write_ops o
		WHERE o.kind = 'issue_links' AND o.tenant = $1 AND o.state IN ('prepared','remote_unknown') AND ($2::text = '' OR o.entity_id = $2)`
	// MySQL cannot cast a bigint to text with ::text; CAST spells the same
	// test, and NOT $3 becomes ? = 0 for the boolean argument (hk 5077).
	const innerMySQL = `SELECT o.id, o.entity_id, o.yt_target_id, o.state, o.created_at,
		EXISTS (SELECT 1 FROM yt_task_bindings b WHERE CAST(b.task_id AS CHAR) = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant) AS bound,
		EXISTS (SELECT 1 FROM yt_write_ops e WHERE e.entity_kind = o.entity_kind AND e.entity_id = o.entity_id AND e.id < o.id AND e.state NOT IN ('applied','rejected')) AS behind
		FROM yt_write_ops o
		WHERE o.kind = 'issue_links' AND o.tenant = ? AND o.state IN ('prepared','remote_unknown') AND (? = '' OR o.entity_id = ?)`
	const held = `SELECT id, entity_id, yt_target_id, state, created_at, bound, behind FROM (` + inner + `) h
		WHERE h.state = 'remote_unknown' OR NOT $3::boolean OR NOT h.bound OR h.behind`
	const heldMySQL = `SELECT id, entity_id, yt_target_id, state, created_at, bound, behind FROM (` + innerMySQL + `) h
		WHERE h.state = 'remote_unknown' OR ? = 0 OR NOT h.bound OR h.behind`
	if dialectFor(tx) == mysqlDialect {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (`+heldMySQL+`) c`, tenant, entity, entity, out.Open).Scan(&out.HeldCount); err != nil {
			return err
		}
	} else if err := tx.QueryRow(ctx, `SELECT count(*) FROM (`+held+`) c`, tenant, entity, out.Open).Scan(&out.HeldCount); err != nil {
		return err
	}
	var rows rows
	var err error
	if dialectFor(tx) == mysqlDialect {
		rows, err = tx.Query(ctx, heldMySQL+` ORDER BY id LIMIT ?`, tenant, entity, entity, out.Open, limit)
	} else {
		rows, err = tx.Query(ctx, held+` ORDER BY id LIMIT $4`, tenant, entity, out.Open, limit)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			h        YTLinkHeldOp
			entityID string
			bound    bool
			behind   bool
		)
		if err := rows.Scan(&h.OpID, &entityID, &h.YTIssueID, &h.State, &h.CreatedAt, &bound, &behind); err != nil {
			return err
		}
		if h.TaskID, err = strconv.ParseInt(entityID, 10, 64); err != nil {
			return fmt.Errorf("issue_links op %d: entity %q is not a task id: %w", h.OpID, entityID, err)
		}
		h.CreatedAt = h.CreatedAt.UTC()
		switch {
		case h.State == string(ytwrite.StateRemoteUnknown):
			h.Reason = ytwrite.ReasonRemoteUnknown
		case !out.Open:
			h.Reason = YTLinksHeldModeOff
		case !bound:
			h.Reason = YTLinksHeldTargetUnbound
		default:
			h.Reason = ytwrite.ReasonWaitingForEarlier
		}
		out.Held = append(out.Held, h)
	}
	return rows.Err()
}

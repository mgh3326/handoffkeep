package store

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// stateMigrationVersion is the schema version that moves tasks.state to the
// eleven-state model, adds the hierarchy and relation tables, and adds the
// task_events columns. scripts/rollback-state-migration.sql carries the same
// number (TestRollbackScriptVersion keeps the two equal).
const stateMigrationVersion = 32

// stateMigrationPlanSQL maps every changed legacy task row to its new state
// ("Migrating existing states" in docs/linear-internalize.md). It reads only
// columns that exist before this migration, so the same text serves the real
// migration and `tasks migrate-states --dry-run` against an unmigrated
// database. Rows that keep their state and need no flag (backlog, merged,
// dropped) are not planned: nothing changes for them.
//
// "A live job" is a jobs row for the task in claimed or spawned; "a recorded
// no-job reason" is a no_job on the newest event into claimed or in_progress;
// "the state it left" is the from of the newest transition into
// needs_decision. The spawn rule gives researching to a live job whose role is
// consult or research, otherwise to a verify or decide task, and developing to
// everything else.
const stateMigrationPlanSQL = `
SELECT task_id, legacy_state, legacy_claimed_by,
  CASE legacy_state
    WHEN 'reopened' THEN 'unstarted'
    WHEN 'claimed' THEN CASE WHEN has_live OR has_nojob THEN spawn_state ELSE 'unstarted' END
    WHEN 'in_progress' THEN spawn_state
    WHEN 'verifying' THEN 'reviewing'
    WHEN 'join' THEN 'reviewing'
    WHEN 'hold' THEN 'icebox'
    WHEN 'needs_decision' THEN CASE
      WHEN is_disposition THEN 'backlog'
      WHEN has_live THEN CASE prev_state
        WHEN 'backlog' THEN 'backlog'
        WHEN 'reopened' THEN 'unstarted'
        WHEN 'verifying' THEN 'reviewing'
        WHEN 'join' THEN 'reviewing'
        WHEN 'hold' THEN 'icebox'
        ELSE spawn_state END
      ELSE 'unstarted' END
  END AS new_state,
  CASE legacy_state
    WHEN 'reopened' THEN 'reopened_unstarted'
    WHEN 'claimed' THEN CASE WHEN has_live OR has_nojob THEN 'claimed_started' ELSE 'claimed_ended' END
    WHEN 'in_progress' THEN 'in_progress_started'
    WHEN 'verifying' THEN 'verifying_reviewing'
    WHEN 'join' THEN 'join_reviewing'
    WHEN 'hold' THEN 'hold_icebox'
    WHEN 'needs_decision' THEN CASE
      WHEN is_disposition THEN 'decision_disposition'
      WHEN has_live THEN 'decision_live_job'
      ELSE 'decision_no_job' END
  END AS rule,
  (legacy_state = 'needs_decision') AS new_awaiting
FROM (
  SELECT t.id AS task_id, t.state AS legacy_state, t.claimed_by AS legacy_claimed_by,
    (t.refs ? 'disposition') AS is_disposition,
    (live.task_id IS NOT NULL) AS has_live,
    (nj.task_id IS NOT NULL) AS has_nojob,
    left_state.prev AS prev_state,
    CASE WHEN live.role IN ('consult','research') THEN 'researching'
         WHEN t.kind IN ('verify','decide') THEN 'researching'
         ELSE 'developing' END AS spawn_state
  FROM tasks t
  LEFT JOIN (SELECT DISTINCT ON (task_id) task_id, role FROM jobs WHERE state IN ('claimed','spawned') ORDER BY task_id, created_at DESC, job_id DESC) live ON live.task_id=t.id
  LEFT JOIN (SELECT DISTINCT ON (task_id) task_id, no_job FROM task_events WHERE kind='transition' AND "to" IN ('claimed','in_progress') ORDER BY task_id, at DESC, id DESC) nj ON nj.task_id=t.id AND nj.no_job<>''
  LEFT JOIN (SELECT DISTINCT ON (task_id) task_id, "from" AS prev FROM task_events WHERE kind='transition' AND "to"='needs_decision' ORDER BY task_id, at DESC, id DESC) left_state ON left_state.task_id=t.id
  WHERE t.state NOT IN ('backlog','merged','dropped')
) base`

// StateMigrationCount is one dry-run line: how many tasks one rule moves.
type StateMigrationCount struct {
	LegacyState string `json:"legacy_state"`
	Rule        string `json:"rule"`
	NewState    string `json:"new_state"`
	Count       int    `json:"count"`
}

// StateMigrationPlan is the read-only result of the dry-run.
type StateMigrationPlan struct {
	Counts []StateMigrationCount `json:"counts"`
	// HoldMirroredToCanceled counts hold tasks that carry a Linear issue link;
	// after the migration they are icebox, which Linear shows as Canceled.
	HoldMirroredToCanceled int `json:"hold_mirrored_to_canceled"`
	// Migrated is true when the database already records the migration, in
	// which case no legacy state is left to plan.
	Migrated bool `json:"migrated"`
}

// PlanStateMigration counts the rows the migration would change, per
// (legacy_state, rule, new_state), and writes nothing. It uses only columns
// that exist before the migration, so it runs against an unmigrated database.
func (s *Store) PlanStateMigration(ctx context.Context) (StateMigrationPlan, error) {
	var plan StateMigrationPlan
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return plan, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=$1)`, stateMigrationVersion).Scan(&plan.Migrated); err != nil {
		return plan, err
	}
	if plan.Migrated {
		plan.Counts = []StateMigrationCount{}
		return plan, tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `SELECT legacy_state, rule, new_state, count(*) FROM (`+stateMigrationPlanSQL+`) plan GROUP BY legacy_state, rule, new_state ORDER BY legacy_state COLLATE "C", rule COLLATE "C", new_state COLLATE "C"`)
	if err != nil {
		return plan, err
	}
	defer rows.Close()
	plan.Counts = []StateMigrationCount{}
	for rows.Next() {
		var c StateMigrationCount
		if err = rows.Scan(&c.LegacyState, &c.Rule, &c.NewState, &c.Count); err != nil {
			return plan, err
		}
		plan.Counts = append(plan.Counts, c)
	}
	if err = rows.Err(); err != nil {
		return plan, err
	}
	rows.Close()
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE state='hold' AND COALESCE(refs->'linear'->>'issue_id','')<>''`).Scan(&plan.HoldMirroredToCanceled); err != nil {
		return plan, err
	}
	return plan, tx.Commit(ctx)
}

// OpenForPlan opens a pool and runs no migration. It exists for the
// migrate-states dry-run, which must see the database exactly as the
// unmigrated server left it.
func OpenForPlan(ctx context.Context, url string) (*Store, error) {
	p, err := newPool(ctx, url)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{pool: p}, nil
}

// migrateStatesV32 moves the schema to the eleven-state model in the single
// transaction migrate already holds. Order follows "Migration mechanics":
//
//  1. task_state_migration records every changed row, so the rollback can
//     restore it;
//  2. the new columns, tables and state values are added, with the old state
//     values still allowed;
//  3. tasks are updated by the map and one state_migration event is appended
//     per changed task;
//  4. the old state values are dropped from the constraint.
//
// Locks: the ALTER TABLE statements on tasks and task_events take ACCESS
// EXCLUSIVE, and the constraint swaps scan tasks while holding it. The
// UPDATE emits one sync_log row per changed task through the sync_tasks
// trigger. Both costs scale with the dry-run counts. lock_timeout bounds the
// wait for the lock so a long reader makes the start fail and retry instead
// of queueing the task API behind it. No Linear operation is enqueued; the
// reconciler corrects mirrored issues afterwards, under projection policy.
func migrateStatesV32(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, stateMigrationDDL); err != nil {
		return err
	}
	if err := execAll(ctx, tx, []string{
		// A rollback followed by a second migration finds the previous
		// snapshot; the snapshot describes the latest migration only.
		`DELETE FROM task_state_migration`,
		`INSERT INTO task_state_migration(task_id,legacy_state,legacy_claimed_by,new_state,new_awaiting,rule,migrated_at)
		 SELECT task_id, legacy_state, legacy_claimed_by, new_state, new_awaiting, rule, now() FROM (` + stateMigrationPlanSQL + `) plan`,
		`UPDATE tasks t SET state=m.new_state,
		   claimed_by=CASE WHEN m.rule='claimed_ended' THEN '' ELSE t.claimed_by END,
		   awaiting_decision=m.new_awaiting, updated_at=now()
		 FROM task_state_migration m WHERE m.task_id=t.id`,
		`INSERT INTO task_events(task_id,"from","to","by",note,refs,at,kind,no_job,source,source_ref,field,from_value,to_value,auto)
		 SELECT m.task_id, m.legacy_state, m.new_state, 'migration', m.rule, t.refs, now(), 'state_migration', '',
		   'migration', 'v` + strconv.Itoa(stateMigrationVersion) + `@' || to_char(now() AT TIME ZONE 'UTC','YYYYMMDD"T"HH24MISS.US'), 'state', m.legacy_state, m.new_state, true
		 FROM task_state_migration m JOIN tasks t ON t.id=m.task_id`,
		`ALTER TABLE tasks DROP CONSTRAINT tasks_state_check, ADD CONSTRAINT tasks_state_check CHECK(state IN ('triage','backlog','unstarted','researching','developing','reviewing','merged','deployed','dropped','duplicate','icebox'))`,
		`INSERT INTO schema_version(version) VALUES (` + strconv.Itoa(stateMigrationVersion) + `)`,
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout); err != nil {
		return err
	}
	return nil
}

// stateMigrationDDL is step 1 and 2: the snapshot table, the new columns and
// tables, and a state constraint that allows old and new values together.
var stateMigrationDDL = []string{
	`CREATE TABLE IF NOT EXISTS task_state_migration (task_id BIGINT PRIMARY KEY, legacy_state TEXT NOT NULL, legacy_claimed_by TEXT NOT NULL, new_state TEXT NOT NULL, new_awaiting BOOLEAN NOT NULL, rule TEXT NOT NULL, migrated_at TIMESTAMPTZ NOT NULL)`,
	// Constant defaults and nullable columns make these metadata-only; the
	// self-referencing foreign key scans tasks once, where every parent_id is
	// still NULL.
	`ALTER TABLE tasks
	   ADD COLUMN IF NOT EXISTS awaiting_decision BOOLEAN NOT NULL DEFAULT false,
	   ADD COLUMN IF NOT EXISTS parent_id BIGINT REFERENCES tasks(id) CHECK(parent_id IS NULL OR parent_id<>id),
	   ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ,
	   ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ,
	   ADD COLUMN IF NOT EXISTS canceled_at TIMESTAMPTZ,
	   ADD COLUMN IF NOT EXISTS triaged_at TIMESTAMPTZ,
	   ADD COLUMN IF NOT EXISTS started_triage_at TIMESTAMPTZ`,
	`CREATE INDEX IF NOT EXISTS tasks_parent ON tasks(parent_id, id) WHERE parent_id IS NOT NULL`,
	`ALTER TABLE tasks DROP CONSTRAINT tasks_state_check, ADD CONSTRAINT tasks_state_check CHECK(state IN ('backlog','reopened','claimed','in_progress','verifying','join','hold','needs_decision','merged','dropped','triage','unstarted','researching','developing','reviewing','deployed','duplicate','icebox')) NOT VALID`,
	// task_events gains the activity-history shape. Defaults are constants, so
	// existing rows keep empty values and the adds are metadata-only. The
	// partial unique index makes a redelivered source event change nothing.
	`ALTER TABLE task_events
	   ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '',
	   ADD COLUMN IF NOT EXISTS source_ref TEXT NOT NULL DEFAULT '',
	   ADD COLUMN IF NOT EXISTS field TEXT NOT NULL DEFAULT '',
	   ADD COLUMN IF NOT EXISTS from_value TEXT NOT NULL DEFAULT '',
	   ADD COLUMN IF NOT EXISTS to_value TEXT NOT NULL DEFAULT '',
	   ADD COLUMN IF NOT EXISTS auto BOOLEAN NOT NULL DEFAULT false,
	   ADD COLUMN IF NOT EXISTS applied BOOLEAN NOT NULL DEFAULT true`,
	`CREATE UNIQUE INDEX IF NOT EXISTS task_events_source_ref ON task_events(task_id, source, source_ref) WHERE source<>'' AND source_ref<>''`,
	// New event kinds follow the v14/v15/v18 pattern: NOT VALID, because
	// every existing row already satisfies the narrower list.
	`ALTER TABLE task_events DROP CONSTRAINT IF EXISTS task_events_kind_check, ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane','decision','project','area','state_migration','awaiting_decision','parent','relation','auto_close')) NOT VALID`,
	// A relation is stored once: blocks and duplicate_of point from src to dst
	// (src blocks dst; src is a duplicate of dst); related is symmetric and
	// kept in normalized order (src_id < dst_id). Removing an edge sets
	// archived_at and adding it again clears it, so repeated adds and removes
	// converge on one row.
	`CREATE TABLE IF NOT EXISTS task_relations (
	   id BIGSERIAL PRIMARY KEY,
	   kind TEXT NOT NULL CHECK(kind IN ('blocks','duplicate_of','related')),
	   src_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	   dst_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	   created_by TEXT NOT NULL,
	   created_at TIMESTAMPTZ NOT NULL,
	   archived_at TIMESTAMPTZ,
	   archived_by TEXT NOT NULL DEFAULT '',
	   CHECK(src_id<>dst_id),
	   CHECK(kind<>'related' OR src_id<dst_id),
	   UNIQUE(kind, src_id, dst_id))`,
	`CREATE INDEX IF NOT EXISTS task_relations_dst ON task_relations(dst_id, kind)`,
}

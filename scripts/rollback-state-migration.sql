-- Roll back the task state migration (schema version 32): map every task
-- back to the legacy state vocabulary so the previous binary can run again.
--
-- Run it once, in one transaction, against the database the server uses, then
-- restore the previous binary (docs/linear-internalize.md, "Rollout and
-- rollback"):
--
--   psql "$DB_URL" -v ON_ERROR_STOP=1 --single-transaction -f scripts/rollback-state-migration.sql
--
-- It uses no psql meta commands, so a test can run the same text over a
-- driver. schema_version: 32
--
-- 1. A task whose state and awaiting_decision flag are still what the
--    migration set gets its pre-migration state and claimed_by back from
--    task_state_migration.
-- 2. Every other task, including a task created or changed after the
--    migration, maps by the inverse table:
--      awaiting_decision set            -> needs_decision
--      triage, backlog                  -> backlog
--      unstarted                        -> reopened
--      researching, developing          -> in_progress
--      reviewing                        -> verifying
--      merged, deployed                 -> merged
--      dropped, duplicate               -> dropped
--      icebox                           -> hold
-- 3. The old state constraint returns and schema_version 32 is removed.
--
-- One state_migration event is appended per changed task; earlier task_events
-- rows are not rewritten, so events written under the new model keep their
-- new names. The added columns and tables stay in place, unused. What only the
-- new model recorded is lost: sub-state precision, triage, deployed and the
-- duplicate distinction. The ALTER TABLE statements take ACCESS EXCLUSIVE on
-- tasks; lock_timeout fails the script instead of queueing behind a reader.

SET LOCAL lock_timeout = '5s';

ALTER TABLE tasks DROP CONSTRAINT tasks_state_check, ADD CONSTRAINT tasks_state_check CHECK(state IN ('backlog','reopened','claimed','in_progress','verifying','join','hold','needs_decision','merged','dropped','triage','unstarted','researching','developing','reviewing','deployed','duplicate','icebox')) NOT VALID;

WITH plan AS (
  SELECT t.id, t.state AS cur_state, t.claimed_by AS cur_claimed_by, t.awaiting_decision AS cur_awaiting,
    (m.task_id IS NOT NULL AND t.state = m.new_state AND t.awaiting_decision = m.new_awaiting) AS restore,
    m.legacy_state, m.legacy_claimed_by
  FROM tasks t LEFT JOIN task_state_migration m ON m.task_id = t.id
), target AS (
  SELECT id, cur_state,
    CASE
      WHEN restore THEN legacy_state
      WHEN cur_awaiting THEN 'needs_decision'
      WHEN cur_state IN ('triage','backlog') THEN 'backlog'
      WHEN cur_state = 'unstarted' THEN 'reopened'
      WHEN cur_state IN ('researching','developing') THEN 'in_progress'
      WHEN cur_state = 'reviewing' THEN 'verifying'
      WHEN cur_state IN ('merged','deployed','completed') THEN 'merged'
      WHEN cur_state IN ('dropped','duplicate') THEN 'dropped'
      WHEN cur_state = 'icebox' THEN 'hold'
      ELSE cur_state
    END AS legacy_state,
    CASE WHEN restore THEN legacy_claimed_by ELSE cur_claimed_by END AS claimed_by
  FROM plan
), changed AS (
  UPDATE tasks t SET state = g.legacy_state, claimed_by = g.claimed_by, awaiting_decision = false, updated_at = now()
  FROM target g
  WHERE g.id = t.id AND (g.legacy_state IS DISTINCT FROM t.state OR g.claimed_by IS DISTINCT FROM t.claimed_by OR t.awaiting_decision)
  RETURNING t.id, g.cur_state AS from_state, g.legacy_state AS to_state, t.refs
)
INSERT INTO task_events(task_id,"from","to","by",note,refs,at,kind,no_job,source,source_ref,field,from_value,to_value,auto)
SELECT id, from_state, to_state, 'rollback', 'rollback of state migration', refs, now(), 'state_migration', '',
  'rollback', 'v32@' || to_char(now() AT TIME ZONE 'UTC', 'YYYYMMDD"T"HH24MISS.US'), 'state', from_state, to_state, true
FROM changed;

ALTER TABLE tasks DROP CONSTRAINT tasks_state_check, ADD CONSTRAINT tasks_state_check CHECK(state IN ('backlog','reopened','claimed','in_progress','verifying','join','hold','needs_decision','merged','dropped'));

DELETE FROM schema_version WHERE version = 32;

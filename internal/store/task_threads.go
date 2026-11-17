package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Discord task threads (hk 3990; hk:doc design/2026-11-11/operator-surfaces
// §1). A task in one of the configured projects gets one forum post when it
// first enters a started state, then one line per record about it: PR
// opened, review gate verdict, merged, deployed, deploy failed or rolled back,
// reverted, decision opened, decision answered, and closed. Discord is a view:
// every line is rendered from the record (task_events, change_gate_events,
// changes, deploys, decision_requests), and nothing is read back. A review
// line is one gate decision in change_gate_events, keyed by its id (hk 4000).
//
// Delivery is at most once per line. The worker claims a row (claim_key,
// claimed_at) and commits the claim before it sends, then settles the send's
// result in a second transaction. A claim older than TaskThreadClaimLease
// belongs to a worker that died between the two. A thread create whose
// outcome is unknown (such a claim, a 5xx or a timeout) leaves the row creating and
// unclaimed: its next claim is a lookup (TaskThreadItem.Lookup), due
// TaskThreadLookupDelay after the outcome, in which the poster lists the
// forum's threads (hk 4040) and adopts the one it finds, creates again only
// when it finds none, and stops the row needs_operator when it cannot tell.
// A second create would leave two threads for one task. A line whose outcome
// is unknown is counted as posted, so it is never posted twice.
//
// Lock order (lockorder.go): the worker locks one task_thread_cursors row,
// then reads task_events and tasks without locking them; it locks one
// task_thread_deliveries row FOR UPDATE SKIP LOCKED for the length of a claim
// or settle transaction, never while it sends. Nothing else writes either
// table.

// TaskThreadDestinationDiscord is the task-thread forum channel, the only
// destination so far.
const TaskThreadDestinationDiscord = "discord"

// Delivery states.
const (
	TaskThreadPending       = "pending"
	TaskThreadCreating      = "creating"
	TaskThreadActive        = "active"
	TaskThreadNeedsOperator = "needs_operator"
	TaskThreadFailed        = "failed"
)

// Item types: the thread create, then the line types.
const (
	TaskThreadCreate         = "create"
	TaskLinePROpened         = "pr_opened"
	TaskLineReview           = "review"
	TaskLineMerged           = "merged"
	TaskLineDeployed         = "deployed"
	TaskLineDeployFailed     = "deploy_failed"
	TaskLineRolledBack       = "rolled_back"
	TaskLineReverted         = "reverted"
	TaskLineDecisionOpened   = "decision_opened"
	TaskLineDecisionAnswered = "decision_answered"
	TaskLineCompleted        = "completed"
	TaskLineDropped          = "dropped"
	TaskLineDuplicate        = "duplicate"
	TaskLineIcebox           = "icebox"
)

const (
	// TaskThreadNameMaxRunes is Discord's thread name limit.
	TaskThreadNameMaxRunes = 100
	// TaskThreadClaimLease is how long a claim may stand before the worker
	// that took it is taken to have died mid-send. It is well above a send's
	// timeout.
	TaskThreadClaimLease = 2 * time.Minute
	// TaskThreadLookupDelay is how long after a create's unknown outcome the
	// poster looks for its thread: a slow create has that long to surface
	// before finding none lets it create again.
	TaskThreadLookupDelay = 2 * time.Minute
	// TaskThreadIdleAge is how long after a task settles its thread is still
	// checked for new lines.
	TaskThreadIdleAge = 30 * 24 * time.Hour
	// DefaultTaskThreadProjects is the first set of projects with threads.
	DefaultTaskThreadProjects = "panewire,handoffkeep,mergewake,rstack"

	// taskThreadScanBatch bounds how many task_events one queue pass reads
	// past the cursor; taskThreadScanOverlap is how far behind the cursor it
	// looks again, for an event whose id was taken before the cursor moved
	// but committed after.
	taskThreadScanBatch   = 2000
	taskThreadScanOverlap = 1000
	// About a day of retries with the shared backoff.
	taskThreadMaxAttempts = 32
	taskThreadErrorBytes  = 200
	taskThreadSettleWait  = 5 * time.Second
)

// TaskThreadName is a task's thread name: "#<id> <title>", on one line and
// cut to TaskThreadNameMaxRunes.
func TaskThreadName(id int64, title string) string {
	name := strings.Join(strings.Fields(fmt.Sprintf("#%d %s", id, title)), " ")
	if utf8.RuneCountInString(name) > TaskThreadNameMaxRunes {
		name = string([]rune(name)[:TaskThreadNameMaxRunes])
	}
	return name
}

// TaskThreadFindings counts a change's open review findings by severity.
type TaskThreadFindings struct {
	Critical, High, Medium, Low int
}

// TaskThreadItem is one claimed send: a thread create (Type
// TaskThreadCreate) or one line into ThreadID.
type TaskThreadItem struct {
	TaskID      int64
	Destination string
	// Key is the claim: "create", "e:<task event id>" or the key of another
	// record ("g:", "d:", "do:", "da:").
	Key        string
	Type       string
	ThreadID   string
	ThreadName string
	Attempts   int
	// Lookup is set on a create whose earlier attempt has an unknown outcome:
	// the forum is searched for that thread before anything is created.
	Lookup bool

	// Create: the task's project, kind and, when set, grade and tier.
	Project, Kind, Grade, Tier string

	// PR is the pull request the line is about (for a revert, the revert
	// PR).
	PR string
	// GateState and Open describe a review line.
	GateState string
	Open      TaskThreadFindings
	// Service and DeployID describe a deploy line.
	Service  string
	DeployID int64
	// DecisionID, and for an answer who, through which channel and when.
	DecisionID int64
	AnsweredBy string
	Channel    string
	At         time.Time
	// Mention is set for a blocked review with critical or high findings
	// open, a failed deploy and a decision opened.
	Mention bool
}

// TaskThreadOutcome is what a send did, as the worker could tell.
type TaskThreadOutcome int

const (
	// TaskThreadSent: Discord accepted it.
	TaskThreadSent TaskThreadOutcome = iota + 1
	// TaskThreadRetry: Discord certainly did not take it (a 429 answer, or
	// no connection); it is sent again later. A lookup item's retry stays a
	// lookup.
	TaskThreadRetry
	// TaskThreadRejected: Discord refused it in a way a retry cannot fix (a
	// 4xx such as a deleted thread or a missing permission); the row stops.
	TaskThreadRejected
	// TaskThreadUnknown: the request may have reached Discord without a
	// usable answer coming back (a 5xx, a timeout). A create is looked up
	// after TaskThreadLookupDelay; a line counts as posted.
	TaskThreadUnknown
	// TaskThreadAmbiguous: a create's lookup could not tell whether its
	// thread exists (the lookup failed, or found more than one). The row
	// stops needs_operator. Creates only.
	TaskThreadAmbiguous
)

// TaskThreadResult is the settled outcome of one claimed send. ThreadID is
// the created thread (a create that was Sent). RetryAfter, set on a Retry,
// is Discord's own wait (429); it replaces the backoff and costs no attempt.
type TaskThreadResult struct {
	Outcome    TaskThreadOutcome
	ThreadID   string
	RetryAfter time.Duration
	Err        error
}

// QueueTaskThreads owes a thread to every task in projects that entered a
// started state from outside the started category since the last pass. The
// first pass records the newest task event as the epoch, so tasks that
// started before the poster ran get nothing. A task gets at most one row per
// destination, so a later restart of the same task changes nothing.
func (s *Store) QueueTaskThreads(ctx context.Context, now time.Time, projects []string) (int, error) {
	now = now.UTC()
	queued := 0
	err := s.retryTx(ctx, func() error {
		var err error
		queued, err = s.queueTaskThreadsOnce(ctx, now, projects)
		return err
	})
	if err != nil {
		return 0, err
	}
	return queued, nil
}

// queueTaskThreadsOnce is one whole QueueTaskThreads transaction.
func (s *Store) queueTaskThreadsOnce(ctx context.Context, now time.Time, projects []string) (int, error) {
	mysql := s.dialect == mysqlDialect
	dest := TaskThreadDestinationDiscord
	tx, release, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, s.q(`INSERT INTO task_thread_cursors(destination,epoch_event_id,scan_event_id,updated_at)
		SELECT $1::text,coalesce(max(id),0),coalesce(max(id),0),$2::timestamptz FROM task_events ON CONFLICT(destination) DO NOTHING`,
		`INSERT INTO task_thread_cursors(destination,epoch_event_id,scan_event_id,updated_at)
		SELECT ?,COALESCE(MAX(id),0),COALESCE(MAX(id),0),? FROM task_events ON DUPLICATE KEY UPDATE destination=destination`), dest, now); err != nil {
		return 0, err
	}
	var epoch, cursor int64
	if err = tx.QueryRow(ctx, s.q(`SELECT epoch_event_id,scan_event_id FROM task_thread_cursors WHERE destination=$1 FOR UPDATE`,
		`SELECT epoch_event_id,scan_event_id FROM task_thread_cursors WHERE destination=? FOR UPDATE`), dest).Scan(&epoch, &cursor); err != nil {
		return 0, err
	}
	var upper int64
	if mysql {
		// The cursor row is locked: ids are read in id order, as on PostgreSQL.
		err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(id),?) FROM (SELECT id FROM task_events WHERE id>? ORDER BY id LIMIT ?) s`, cursor, cursor, taskThreadScanBatch).Scan(&upper)
	} else {
		err = tx.QueryRow(ctx, `SELECT coalesce(max(id),$1::bigint) FROM (SELECT id FROM task_events WHERE id>$1::bigint ORDER BY id LIMIT $2) s`, cursor, taskThreadScanBatch).Scan(&upper)
	}
	if err != nil {
		return 0, err
	}
	lower := max(epoch, cursor-taskThreadScanOverlap)
	type start struct {
		taskID, eventID int64
		at              time.Time
		title           string
	}
	scanStart := func(r row) (start, error) {
		var x start
		err := r.Scan(&x.taskID, &x.eventID, &x.at, &x.title)
		return x, err
	}
	var starts []start
	switch {
	case !mysql:
		rows, err := tx.Query(ctx, `SELECT DISTINCT ON (e.task_id) e.task_id,e.id,e.at,t.title FROM task_events e JOIN tasks t ON t.id=e.task_id
		WHERE e.id>$1 AND e.id<=$2 AND e.kind='`+TaskEventTransition+`' AND e.applied AND e."to" IN (`+startedStatesSQL+`) AND e."from" NOT IN (`+startedStatesSQL+`)
		AND t.project=ANY($3::text[])
		AND NOT EXISTS (SELECT 1 FROM task_thread_deliveries n WHERE n.task_id=e.task_id AND n.destination=$4)
		ORDER BY e.task_id,e.id`, lower, upper, projects, dest)
		if err != nil {
			return 0, err
		}
		if starts, err = collectRows(rows, scanStart); err != nil {
			return 0, err
		}
	case len(projects) > 0:
		// DISTINCT ON (task_id) ... ORDER BY task_id,id is the first event of each task.
		args := []any{lower, upper}
		for _, p := range projects {
			args = append(args, p)
		}
		args = append(args, dest)
		rows, err := tx.Query(ctx, `SELECT task_id,id,at,title FROM (
			SELECT e.task_id,e.id,e.at,t.title,ROW_NUMBER() OVER (PARTITION BY e.task_id ORDER BY e.id) AS rn FROM task_events e JOIN tasks t ON t.id=e.task_id
			WHERE e.id>? AND e.id<=? AND e.kind='`+TaskEventTransition+`' AND e.applied AND e.`+"`to`"+` IN (`+startedStatesSQL+`) AND e.`+"`from`"+` NOT IN (`+startedStatesSQL+`)
			AND t.project IN (`+placeholders(len(projects))+`)
			AND NOT EXISTS (SELECT 1 FROM task_thread_deliveries n WHERE n.task_id=e.task_id AND n.destination=?)
			) x WHERE rn=1 ORDER BY task_id`, args...)
		if err != nil {
			return 0, err
		}
		if starts, err = collectRows(rows, scanStart); err != nil {
			return 0, err
		}
	}
	queued := 0
	for _, x := range starts {
		if mysql {
			// A plain INSERT: found-rows would report a duplicate-key no-op as 1,
			// like a new row, so the duplicate key itself says "already owed".
			_, err := tx.Exec(ctx, `INSERT INTO task_thread_deliveries(task_id,destination,thread_name,start_event_id,since,last_event_id,state,next_attempt_at,created_at,updated_at)
				VALUES(?,?,?,?,?,?,'`+TaskThreadPending+`',?,?,?)`,
				x.taskID, dest, TaskThreadName(x.taskID, x.title), x.eventID, x.at, x.eventID-1, now, now, now)
			switch {
			case err == nil:
				queued++
			case !errors.Is(err, errUniqueViolation):
				return 0, err
			}
			continue
		}
		tag, err := tx.Exec(ctx, `INSERT INTO task_thread_deliveries(task_id,destination,thread_name,start_event_id,since,last_event_id,state,next_attempt_at,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$4::bigint-1,'`+TaskThreadPending+`',$6,$6,$6) ON CONFLICT(task_id,destination) DO NOTHING`,
			x.taskID, dest, TaskThreadName(x.taskID, x.title), x.eventID, x.at, now)
		if err != nil {
			return 0, err
		}
		queued += int(tag)
	}
	if upper > cursor {
		update := []any{dest, upper, now}
		if mysql {
			update = []any{upper, now, dest}
		}
		if _, err = tx.Exec(ctx, s.q(`UPDATE task_thread_cursors SET scan_event_id=$2,updated_at=$3 WHERE destination=$1`,
			`UPDATE task_thread_cursors SET scan_event_id=?,updated_at=? WHERE destination=?`), update...); err != nil {
			return 0, err
		}
	}
	return queued, tx.Commit(ctx)
}

// TaskThreadRecovery is a claim RecoverTaskThreadClaims found expired.
type TaskThreadRecovery struct {
	TaskID int64
	Key    string
	// State is the row's state afterwards: creating for a create (due for a
	// lookup), active for a line counted as posted.
	State string
}

// RecoverTaskThreadClaims settles every claim older than
// TaskThreadClaimLease: its worker died between the send and the settle, so
// the send's outcome is unknown. A create is due for a lookup at once (the
// claim is already older than TaskThreadLookupDelay); a line is counted as
// posted (no second line).
func (s *Store) RecoverTaskThreadClaims(ctx context.Context, now time.Time) ([]TaskThreadRecovery, error) {
	now = now.UTC()
	if s.dialect == mysqlDialect {
		var out []TaskThreadRecovery
		err := s.retryTx(ctx, func() error {
			var err error
			out, err = s.recoverTaskThreadClaimsMySQL(ctx, now)
			return err
		})
		return out, err
	}
	rows, err := s.db().Query(ctx, `WITH old AS (
			SELECT task_id,destination,claim_key FROM task_thread_deliveries
			WHERE destination=$1 AND claim_key<>'' AND claimed_at<$2 FOR UPDATE SKIP LOCKED)
		UPDATE task_thread_deliveries n SET
			next_attempt_at=CASE WHEN n.state='`+TaskThreadCreating+`' THEN $3 ELSE n.next_attempt_at END,
			last_event_id=CASE WHEN old.claim_key LIKE 'e:%' THEN greatest(n.last_event_id,substr(old.claim_key,3)::bigint) ELSE n.last_event_id END,
			posted=CASE WHEN n.state='`+TaskThreadActive+`' AND old.claim_key NOT LIKE 'e:%' THEN array_append(n.posted,old.claim_key) ELSE n.posted END,
			last_error=CASE WHEN n.state='`+TaskThreadCreating+`' THEN 'create outcome unknown: claim expired' ELSE 'line outcome unknown: claim expired' END,
			claim_key='',claimed_at=NULL,updated_at=$3
		FROM old WHERE n.task_id=old.task_id AND n.destination=old.destination
		RETURNING n.task_id,old.claim_key,n.state`, TaskThreadDestinationDiscord, now.Add(-TaskThreadClaimLease), now)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r row) (TaskThreadRecovery, error) {
		var x TaskThreadRecovery
		err := r.Scan(&x.TaskID, &x.Key, &x.State)
		return x, err
	})
}

// recoverTaskThreadClaimsMySQL is RecoverTaskThreadClaims as two statements in
// one transaction (MySQL has no data-modifying CTE or UPDATE ... RETURNING):
// lock the expired claims, skipping rows another worker holds, then settle each
// by its (task, destination, claim). The assignments run left to right, so
// claim_key is cleared after the columns that read it.
func (s *Store) recoverTaskThreadClaimsMySQL(ctx context.Context, now time.Time) ([]TaskThreadRecovery, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT task_id,claim_key,state FROM task_thread_deliveries
		WHERE destination=? AND claim_key<>'' AND claimed_at<? ORDER BY task_id FOR UPDATE SKIP LOCKED`, TaskThreadDestinationDiscord, now.Add(-TaskThreadClaimLease))
	if err != nil {
		return nil, err
	}
	old, err := collectRows(rows, func(r row) (TaskThreadRecovery, error) {
		var x TaskThreadRecovery
		err := r.Scan(&x.TaskID, &x.Key, &x.State)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	for _, x := range old {
		if _, err := tx.Exec(ctx, `UPDATE task_thread_deliveries SET
			next_attempt_at=CASE WHEN state='`+TaskThreadCreating+`' THEN ? ELSE next_attempt_at END,
			last_event_id=CASE WHEN claim_key LIKE 'e:%' THEN GREATEST(last_event_id,CAST(SUBSTR(claim_key,3) AS SIGNED)) ELSE last_event_id END,
			posted=CASE WHEN state='`+TaskThreadActive+`' AND claim_key NOT LIKE 'e:%' THEN JSON_ARRAY_APPEND(posted,'$',claim_key) ELSE posted END,
			last_error=CASE WHEN state='`+TaskThreadCreating+`' THEN 'create outcome unknown: claim expired' ELSE 'line outcome unknown: claim expired' END,
			claim_key='',claimed_at=NULL,updated_at=?
			WHERE task_id=? AND destination=? AND claim_key=?`, now, now, x.TaskID, TaskThreadDestinationDiscord, x.Key); err != nil {
			return nil, err
		}
	}
	return old, tx.Commit(ctx)
}

// taskThreadPostableEventSQL picks the task events that become a line, for
// the event aliased e: a PR opened or reverted (applied or not), and an
// applied move into merged, deployed or a closed state.
const taskThreadPostableEventSQL = `(e.source IN ('` + SourceMergewakePR + `','` + SourceMergewakeRevert + `')
	OR (e.kind='` + TaskEventTransition + `' AND e.applied AND e."from"<>e."to" AND e."to" IN ('merged','deployed','completed','dropped','duplicate','icebox')))`

// taskThreadItemsSQL lists the next line owed to every active, unclaimed,
// due delivery: per delivery its first postable task event past the cursor,
// and every review gate decision that changed the verdict, deploy failure or rollback, and decision
// opened or answered since the thread's start that it has not posted. $1 is
// the destination, $2 now, $3 the cutoff before which a settled task is no
// longer checked, $4 the cutoff a decision must be older than (as for the
// decisions channel, DecisionNoticePostDelay).
const taskThreadItemsSQL = `WITH d AS (
	SELECT n.task_id,n.since,n.last_event_id,n.posted FROM task_thread_deliveries n JOIN tasks t ON t.id=n.task_id
	WHERE n.destination=$1 AND n.state='` + TaskThreadActive + `' AND n.claim_key='' AND n.next_attempt_at<=$2::timestamptz
	AND NOT (t.state IN (` + settledStatesSQL + `) AND t.updated_at<$3::timestamptz)
), items AS (
	SELECT d.task_id,e.at,'e:'||e.id AS key,1 AS src,e.id AS ref FROM d CROSS JOIN LATERAL (
		SELECT e.id,e.at FROM task_events e WHERE e.task_id=d.task_id AND e.id>d.last_event_id AND ` + taskThreadPostableEventSQL + ` ORDER BY e.id LIMIT 1) e
	UNION
	SELECT d.task_id,ev.decided_at,'g:'||ev.id,2,ev.id
	FROM d JOIN change_tasks ct ON ct.task_id=d.task_id JOIN change_gate_events ev ON ev.change_id=ct.change_id
	WHERE ev.changed AND ev.state IN ('ready','blocked') AND ev.decided_at>=d.since
	AND NOT (('g:'||ev.id)=ANY(d.posted))
	UNION
	SELECT d.task_id,dp.finished_at,'d:'||dp.id,3,dp.id
	FROM d JOIN change_tasks ct ON ct.task_id=d.task_id JOIN deploy_changes dc ON dc.change_id=ct.change_id AND dc.included JOIN deploys dp ON dp.id=dc.deploy_id
	WHERE (dp.state IN ('failed','rolled_back') OR (dp.action='rollback' AND dp.state='success')) AND dp.finished_at>=d.since
	AND NOT (('d:'||dp.id)=ANY(d.posted))
	UNION
	SELECT d.task_id,r.created_at,'do:'||r.id,4,r.id FROM d JOIN decision_requests r ON r.task_id=d.task_id
	WHERE r.created_at>=d.since AND r.created_at<=$4::timestamptz
	AND NOT (r.status='` + DecisionStatusSuperseded + `' AND r.updated_at<=r.created_at+interval '1 minute')
	AND NOT (('do:'||r.id)=ANY(d.posted))
	UNION
	SELECT d.task_id,r.answered_at,'da:'||r.id,5,r.id FROM d JOIN decision_requests r ON r.task_id=d.task_id
	WHERE r.status='` + DecisionStatusAnswered + `' AND r.answered_at IS NOT NULL AND r.created_at>=d.since AND r.created_at<=$4::timestamptz
	AND NOT (('da:'||r.id)=ANY(d.posted))
)
SELECT task_id,key,src,ref,at FROM items ORDER BY at,task_id,src,key LIMIT 1`

// taskThreadPostableEventMySQL is taskThreadPostableEventSQL for MySQL: the
// reserved words from and to are quoted.
const taskThreadPostableEventMySQL = `(e.source IN ('` + SourceMergewakePR + `','` + SourceMergewakeRevert + `')
	OR (e.kind='` + TaskEventTransition + `' AND e.applied AND e.` + "`from`" + `<>e.` + "`to`" + ` AND e.` + "`to`" + ` IN ('merged','deployed','completed','dropped','duplicate','icebox')))`

// taskThreadItemsMySQL builds taskThreadItemsSQL for MySQL. The placeholders
// are positional: destination, now, idle cutoff, then the decision cutoff
// once per decision source it is used in. The event source reads only task
// tables; the others read the change, deploy and decision tables of other
// areas, and a source is left out while its tables do not exist on this
// database (it can hold no such record): see a5TablesExist. posted is a JSON
// array, so "not posted yet" is NOT JSON_CONTAINS of the quoted key.
func taskThreadItemsMySQL(gates, deploys, decisions bool) string {
	notPosted := func(prefix, id string) string {
		return `AND NOT JSON_CONTAINS(d.posted,JSON_QUOTE(CONCAT('` + prefix + `',` + id + `)))`
	}
	q := `WITH d AS (
	SELECT n.task_id,n.since,n.last_event_id,n.posted FROM task_thread_deliveries n JOIN tasks t ON t.id=n.task_id
	WHERE n.destination=? AND n.state='` + TaskThreadActive + `' AND n.claim_key='' AND n.next_attempt_at<=?
	AND NOT (t.state IN (` + settledStatesSQL + `) AND t.updated_at<?)
), items AS (
	SELECT d.task_id,pe.at AS at,CONCAT('e:',pe.id) AS ckey,1 AS src,pe.id AS ref FROM d CROSS JOIN LATERAL (
		SELECT e.id,e.at FROM task_events e WHERE e.task_id=d.task_id AND e.id>d.last_event_id AND ` + taskThreadPostableEventMySQL + ` ORDER BY e.id LIMIT 1) pe`
	if gates {
		q += `
	UNION
	SELECT d.task_id,ev.decided_at,CONCAT('g:',ev.id),2,ev.id
	FROM d JOIN change_tasks ct ON ct.task_id=d.task_id JOIN change_gate_events ev ON ev.change_id=ct.change_id
	WHERE ev.changed AND ev.state IN ('ready','blocked') AND ev.decided_at>=d.since
	` + notPosted("g:", "ev.id")
	}
	if deploys {
		q += `
	UNION
	SELECT d.task_id,dp.finished_at,CONCAT('d:',dp.id),3,dp.id
	FROM d JOIN change_tasks ct ON ct.task_id=d.task_id JOIN deploy_changes dc ON dc.change_id=ct.change_id AND dc.included JOIN deploys dp ON dp.id=dc.deploy_id
	WHERE (dp.state IN ('failed','rolled_back') OR (dp.action='rollback' AND dp.state='success')) AND dp.finished_at>=d.since
	` + notPosted("d:", "dp.id")
	}
	if decisions {
		q += `
	UNION
	SELECT d.task_id,r.created_at,CONCAT('do:',r.id),4,r.id FROM d JOIN decision_requests r ON r.task_id=d.task_id
	WHERE r.created_at>=d.since AND r.created_at<=?
	AND NOT (r.status='` + DecisionStatusSuperseded + `' AND r.updated_at<=DATE_ADD(r.created_at,INTERVAL 1 MINUTE))
	` + notPosted("do:", "r.id") + `
	UNION
	SELECT d.task_id,r.answered_at,CONCAT('da:',r.id),5,r.id FROM d JOIN decision_requests r ON r.task_id=d.task_id
	WHERE r.status='` + DecisionStatusAnswered + `' AND r.answered_at IS NOT NULL AND r.created_at>=d.since AND r.created_at<=?
	` + notPosted("da:", "r.id")
	}
	return q + `
)
SELECT task_id,ckey,src,ref,at FROM items ORDER BY at,task_id,src,ckey LIMIT 1`
}

// ClaimTaskThread claims the next send and commits the claim before the
// caller sends: a thread owed a create first (a pending row, or a creating
// row due for its lookup), else the oldest line owed to any active thread.
// found is false when nothing is due.
func (s *Store) ClaimTaskThread(ctx context.Context, now time.Time) (TaskThreadItem, bool, error) {
	now = now.UTC()
	var x TaskThreadItem
	var found bool
	err := s.retryTx(ctx, func() error {
		var err error
		x, found, err = s.claimTaskThreadOnce(ctx, now)
		return err
	})
	if err != nil {
		return TaskThreadItem{}, false, err
	}
	return x, found, nil
}

// taskThreadRefText is the text of a refs member for the create row's grade and
// tier: PostgreSQL's refs->>'k' (SQL NULL for a JSON null) on MySQL.
func taskThreadRefText(path string) string {
	v := `JSON_EXTRACT(t.refs,'` + path + `')`
	return `CASE WHEN JSON_TYPE(` + v + `)='NULL' THEN NULL ELSE JSON_UNQUOTE(` + v + `) END`
}

// claimTaskThreadOnce is one whole ClaimTaskThread transaction.
func (s *Store) claimTaskThreadOnce(ctx context.Context, now time.Time) (TaskThreadItem, bool, error) {
	mysql := s.dialect == mysqlDialect
	dest := TaskThreadDestinationDiscord
	tx, release, err := s.begin(ctx)
	if err != nil {
		return TaskThreadItem{}, false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	x := TaskThreadItem{Destination: dest, Type: TaskThreadCreate, Key: TaskThreadCreate}
	createQuery := s.q(`SELECT n.task_id,n.thread_name,n.attempts,n.state='`+TaskThreadCreating+`',coalesce(t.project,''),t.kind,coalesce(nullif(t.refs->>'grade',''),t.refs->'linear'->>'grade',''),coalesce(nullif(t.refs->>'tier',''),t.refs->'linear'->>'tier','')
		FROM task_thread_deliveries n JOIN tasks t ON t.id=n.task_id
		WHERE n.destination=$1 AND n.state IN ('`+TaskThreadPending+`','`+TaskThreadCreating+`') AND n.claim_key='' AND n.next_attempt_at<=$2::timestamptz
		ORDER BY n.next_attempt_at,n.start_event_id LIMIT 1 FOR UPDATE OF n SKIP LOCKED`,
		`SELECT n.task_id,n.thread_name,n.attempts,n.state='`+TaskThreadCreating+`',COALESCE(t.project,''),t.kind,COALESCE(NULLIF(`+taskThreadRefText("$.grade")+`,''),`+taskThreadRefText("$.linear.grade")+`,''),COALESCE(NULLIF(`+taskThreadRefText("$.tier")+`,''),`+taskThreadRefText("$.linear.tier")+`,'')
		FROM task_thread_deliveries n JOIN tasks t ON t.id=n.task_id
		WHERE n.destination=? AND n.state IN ('`+TaskThreadPending+`','`+TaskThreadCreating+`') AND n.claim_key='' AND n.next_attempt_at<=?
		ORDER BY n.next_attempt_at,n.start_event_id LIMIT 1 FOR UPDATE OF n SKIP LOCKED`)
	if mysql {
		err = claimThreadCreateMySQL(ctx, tx, createQuery, dest, now, &x)
	} else {
		err = tx.QueryRow(ctx, createQuery, dest, now).Scan(&x.TaskID, &x.ThreadName, &x.Attempts, &x.Lookup, &x.Project, &x.Kind, &x.Grade, &x.Tier)
	}
	switch {
	case err == nil:
		claim := []any{x.TaskID, dest, x.Key, now}
		if mysql {
			claim = []any{x.Key, now, now, x.TaskID, dest}
		}
		if _, err = tx.Exec(ctx, s.q(`UPDATE task_thread_deliveries SET state='`+TaskThreadCreating+`',claim_key=$3,claimed_at=$4,updated_at=$4 WHERE task_id=$1 AND destination=$2`,
			`UPDATE task_thread_deliveries SET state='`+TaskThreadCreating+`',claim_key=?,claimed_at=?,updated_at=? WHERE task_id=? AND destination=?`), claim...); err != nil {
			return TaskThreadItem{}, false, err
		}
		return x, true, tx.Commit(ctx)
	case !errors.Is(err, ErrNoRows):
		return TaskThreadItem{}, false, err
	}

	x = TaskThreadItem{Destination: dest}
	var src int
	var ref int64
	if mysql {
		query, decisionSources, err := a5TaskThreadItemsQuery(ctx, s, tx)
		if err != nil {
			return TaskThreadItem{}, false, err
		}
		// The decision cutoff is bound once per decision source in the query.
		args := []any{dest, now, now.Add(-TaskThreadIdleAge)}
		for range decisionSources {
			args = append(args, now.Add(-DecisionNoticePostDelay))
		}
		err = tx.QueryRow(ctx, query, args...).Scan(&x.TaskID, &x.Key, &src, &ref, &x.At)
		if errors.Is(err, ErrNoRows) {
			return TaskThreadItem{}, false, nil
		}
		if err != nil {
			return TaskThreadItem{}, false, err
		}
	} else {
		err = tx.QueryRow(ctx, taskThreadItemsSQL, dest, now, now.Add(-TaskThreadIdleAge), now.Add(-DecisionNoticePostDelay)).Scan(&x.TaskID, &x.Key, &src, &ref, &x.At)
		if errors.Is(err, ErrNoRows) {
			return TaskThreadItem{}, false, nil
		}
		if err != nil {
			return TaskThreadItem{}, false, err
		}
	}
	err = tx.QueryRow(ctx, s.q(`SELECT thread_id,thread_name,attempts FROM task_thread_deliveries
		WHERE task_id=$1 AND destination=$2 AND state='`+TaskThreadActive+`' AND claim_key='' FOR UPDATE SKIP LOCKED`,
		`SELECT thread_id,thread_name,attempts FROM task_thread_deliveries
		WHERE task_id=? AND destination=? AND state='`+TaskThreadActive+`' AND claim_key='' FOR UPDATE SKIP LOCKED`), x.TaskID, dest).
		Scan(&x.ThreadID, &x.ThreadName, &x.Attempts)
	if errors.Is(err, ErrNoRows) {
		return TaskThreadItem{}, false, nil
	}
	if err != nil {
		return TaskThreadItem{}, false, err
	}
	if err = loadTaskThreadLine(ctx, tx, &x, src, ref); err != nil {
		return TaskThreadItem{}, false, err
	}
	claim := []any{x.TaskID, dest, x.Key, now}
	if mysql {
		claim = []any{x.Key, now, now, x.TaskID, dest}
	}
	if _, err = tx.Exec(ctx, s.q(`UPDATE task_thread_deliveries SET claim_key=$3,claimed_at=$4,updated_at=$4 WHERE task_id=$1 AND destination=$2`,
		`UPDATE task_thread_deliveries SET claim_key=?,claimed_at=?,updated_at=? WHERE task_id=? AND destination=?`), claim...); err != nil {
		return TaskThreadItem{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

// loadTaskThreadLine fills a line's type and fields from its record. Only
// structured fields and links are read, never a comment, report or question
// body.
func loadTaskThreadLine(ctx context.Context, tx transaction, x *TaskThreadItem, src int, ref int64) error {
	switch src {
	case 1:
		var source, to, sourceRef, pr string
		if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT coalesce(source,''),"to",coalesce(source_ref,''),coalesce(refs->>'pr','') FROM task_events WHERE id=$1`,
			`SELECT COALESCE(source,''),`+"`to`"+`,COALESCE(source_ref,''),COALESCE(CASE WHEN JSON_TYPE(JSON_EXTRACT(refs,'$.pr'))='NULL' THEN NULL ELSE JSON_UNQUOTE(JSON_EXTRACT(refs,'$.pr')) END,'') FROM task_events WHERE id=?`), ref).Scan(&source, &to, &sourceRef, &pr); err != nil {
			return err
		}
		x.PR = pr
		switch {
		case source == SourceMergewakePR:
			x.Type, x.PR = TaskLinePROpened, sourceRef
		case source == SourceMergewakeRevert:
			x.Type, x.PR = TaskLineReverted, sourceRef
		case to == StateMerged:
			x.Type = TaskLineMerged
		case to == StateDeployed:
			x.Type = TaskLineDeployed
		case to == StateCompleted:
			x.Type = TaskLineCompleted
		case to == StateDropped:
			x.Type = TaskLineDropped
		case to == StateDuplicate:
			x.Type = TaskLineDuplicate
		default:
			x.Type = TaskLineIcebox
		}
	case 2:
		// ref is the gate decision (change_gate_events, hk 4000): its verdict
		// and change are the record; the key is g:<event id>.
		x.Type = TaskLineReview
		var changeID int64
		if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT change_id,state FROM change_gate_events WHERE id=$1`, `SELECT change_id,state FROM change_gate_events WHERE id=?`), ref).Scan(&changeID, &x.GateState); err != nil {
			return err
		}
		var repo string
		var number int
		f := &x.Open
		if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT c.repo,c.pr_number,
				count(*) FILTER (WHERE f.severity='critical' AND `+findingStatusSQL+`='open'),
				count(*) FILTER (WHERE f.severity='high' AND `+findingStatusSQL+`='open'),
				count(*) FILTER (WHERE f.severity='medium' AND `+findingStatusSQL+`='open'),
				count(*) FILTER (WHERE f.severity='low' AND `+findingStatusSQL+`='open')
			FROM changes c LEFT JOIN change_findings f ON f.change_id=c.id `+findingThreadJoin+`
			WHERE c.id=$1 GROUP BY c.id`, `SELECT c.repo,c.pr_number,
				CAST(COALESCE(SUM(CASE WHEN f.severity='critical' AND `+findingStatusSQL+`='open' THEN 1 ELSE 0 END),0) AS SIGNED),
				CAST(COALESCE(SUM(CASE WHEN f.severity='high' AND `+findingStatusSQL+`='open' THEN 1 ELSE 0 END),0) AS SIGNED),
				CAST(COALESCE(SUM(CASE WHEN f.severity='medium' AND `+findingStatusSQL+`='open' THEN 1 ELSE 0 END),0) AS SIGNED),
				CAST(COALESCE(SUM(CASE WHEN f.severity='low' AND `+findingStatusSQL+`='open' THEN 1 ELSE 0 END),0) AS SIGNED)
			FROM changes c LEFT JOIN change_findings f ON f.change_id=c.id `+findingThreadJoin+`
			WHERE c.id=? GROUP BY c.id`), changeID).Scan(&repo, &number, &f.Critical, &f.High, &f.Medium, &f.Low); err != nil {
			return err
		}
		x.PR = changeURL(repo, number)
		x.Mention = x.GateState == "blocked" && f.Critical+f.High > 0
	case 3:
		var action, state string
		if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT service,action,state FROM deploys WHERE id=$1`, `SELECT service,action,state FROM deploys WHERE id=?`), ref).Scan(&x.Service, &action, &state); err != nil {
			return err
		}
		x.DeployID = ref
		x.Type = TaskLineRolledBack
		if state == "failed" {
			x.Type = TaskLineDeployFailed
		}
		x.Mention = state == "failed" || state == "rolled_back"
	case 4:
		x.Type, x.DecisionID, x.Mention = TaskLineDecisionOpened, ref, true
	case 5:
		x.Type, x.DecisionID = TaskLineDecisionAnswered, ref
		var at *time.Time
		if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT coalesce(r.answered_by,''),r.answered_at,coalesce((SELECT x.channel FROM decision_resolutions x
				WHERE x.task_id=r.task_id AND (x.request_id=r.source_ref OR x.revision_request_id=r.source_ref) AND x.kind='answered' ORDER BY x.committed_at DESC,x.resolution_id DESC LIMIT 1),'')
			FROM decision_requests r WHERE r.id=$1`, `SELECT COALESCE(r.answered_by,''),r.answered_at,COALESCE((SELECT x.channel FROM decision_resolutions x
				WHERE x.task_id=r.task_id AND (x.request_id=r.source_ref OR x.revision_request_id=r.source_ref) AND x.kind='answered' ORDER BY x.committed_at DESC,x.resolution_id DESC LIMIT 1),'')
			FROM decision_requests r WHERE r.id=?`), ref).Scan(&x.AnsweredBy, &at, &x.Channel); err != nil {
			return err
		}
		if at != nil {
			x.At = at.UTC()
		}
	default:
		return fmt.Errorf("unknown task thread item source %d", src)
	}
	return nil
}

// SettleTaskThread records the outcome of the send x claimed. It writes only
// while the claim is still x's, so a claim recovered meanwhile is not
// overwritten. gaveUp reports a row that stopped (failed or needs_operator)
// with this settle. A create whose outcome is unknown stays creating,
// unclaimed and due for its lookup TaskThreadLookupDelay from now; after
// taskThreadMaxAttempts of them it stops needs_operator.
func (s *Store) SettleTaskThread(ctx context.Context, now time.Time, x TaskThreadItem, res TaskThreadResult) (gaveUp bool, err error) {
	now = now.UTC()
	// The send may have created something: record it even if ctx is canceled
	// now (shutdown), or the next start would not know.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskThreadSettleWait)
	defer cancel()
	errText := ""
	if res.Err != nil {
		errText = firstBytes(res.Err.Error(), taskThreadErrorBytes)
	}
	const key = ` WHERE task_id=$1 AND destination=$2 AND claim_key=$3`
	args := []any{x.TaskID, x.Destination, x.Key, now}
	create := x.Type == TaskThreadCreate
	// The claim's key moves last_event_id (an event line) or joins posted (any
	// other); posted is a JSON array on MySQL.
	lastEventSQL := `last_event_id=greatest(last_event_id,substr(claim_key,3)::bigint)`
	postedSQL := `posted=array_append(posted,claim_key)`
	if s.dialect == mysqlDialect {
		lastEventSQL = `last_event_id=GREATEST(last_event_id,CAST(SUBSTR(claim_key,3) AS SIGNED))`
		postedSQL = `posted=JSON_ARRAY_APPEND(posted,'$',claim_key)`
	}
	attempts := x.Attempts + 1
	var set string
	switch res.Outcome {
	case TaskThreadSent:
		switch {
		case create:
			if !validThreadID(res.ThreadID) {
				return false, errors.New("task thread create settled without a thread id")
			}
			set, args = `state='`+TaskThreadActive+`',thread_id=$5,last_event_id=start_event_id-1,attempts=0,last_error=''`, append(args, res.ThreadID)
		case strings.HasPrefix(x.Key, "e:"):
			set = lastEventSQL + `,attempts=0,last_error=''`
		default:
			set = postedSQL + `,attempts=0,last_error=''`
		}
	case TaskThreadRetry:
		wait := res.RetryAfter
		if wait > 0 {
			attempts = x.Attempts
		} else {
			wait = deliveryBackoff(x.Attempts)
		}
		switch {
		case attempts >= taskThreadMaxAttempts && create && x.Lookup:
			// The lookups never ran, so an earlier create may still have
			// made the thread: that is the operator's, not a failure.
			gaveUp, set, args = true, `state='`+TaskThreadNeedsOperator+`',attempts=$5,last_error=$6`, append(args, attempts, "create lookup: "+errText)
		case attempts >= taskThreadMaxAttempts:
			gaveUp, set, args = true, `state='`+TaskThreadFailed+`',attempts=$5,last_error=$6`, append(args, attempts, errText)
		}
		if gaveUp {
			break
		}
		state := TaskThreadActive
		switch {
		case create && x.Lookup:
			// The lookup did not run: the next claim is a lookup again.
			state = TaskThreadCreating
		case create:
			state = TaskThreadPending
		}
		set, args = `state='`+state+`',attempts=$5,last_error=$6,next_attempt_at=$7`, append(args, attempts, errText, now.Add(wait))
	case TaskThreadRejected:
		gaveUp, set, args = true, `state='`+TaskThreadFailed+`',attempts=$5,last_error=$6`, append(args, attempts, errText)
	case TaskThreadUnknown:
		switch {
		case create && attempts >= taskThreadMaxAttempts:
			gaveUp, set, args = true, `state='`+TaskThreadNeedsOperator+`',attempts=$5,last_error=$6`, append(args, attempts, "create outcome unknown: "+errText)
		case create:
			set, args = `state='`+TaskThreadCreating+`',attempts=$5,last_error=$6,next_attempt_at=$7`, append(args, attempts, "create outcome unknown: "+errText, now.Add(TaskThreadLookupDelay))
		case strings.HasPrefix(x.Key, "e:"):
			set, args = lastEventSQL+`,last_error=$5`, append(args, "line outcome unknown: "+errText)
		default:
			set, args = postedSQL+`,last_error=$5`, append(args, "line outcome unknown: "+errText)
		}
	case TaskThreadAmbiguous:
		if !create {
			return false, errors.New("only a task thread create can be ambiguous")
		}
		gaveUp, set, args = true, `state='`+TaskThreadNeedsOperator+`',last_error=$5`, append(args, "create lookup: "+errText)
	default:
		return false, fmt.Errorf("unknown task thread outcome %d", res.Outcome)
	}
	query := `UPDATE task_thread_deliveries SET ` + set + `,claim_key='',claimed_at=NULL,updated_at=$4` + key
	if s.dialect == mysqlDialect {
		// MySQL placeholders are positional: the text above numbers them for
		// PostgreSQL, so expand it with the arguments in order of appearance.
		query, args = a5Positional(query, args)
	}
	err = s.retryTx(ctx, func() error {
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		if tag == 0 {
			return fmt.Errorf("task thread claim %s on task %d is no longer held", x.Key, x.TaskID)
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return false, err
	}
	return gaveUp, nil
}

func validThreadID(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	_, err := strconv.ParseUint(id, 10, 64)
	return err == nil
}

// TaskThreadState is a task_thread_deliveries row as stored.
type TaskThreadState struct {
	TaskID        int64     `json:"task_id"`
	Destination   string    `json:"destination"`
	ThreadID      *string   `json:"thread_id"`
	ThreadName    string    `json:"thread_name"`
	StartEventID  int64     `json:"start_event_id"`
	LastEventID   int64     `json:"last_event_id"`
	Posted        []string  `json:"posted"`
	State         string    `json:"state"`
	ClaimKey      string    `json:"claim_key"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastError     string    `json:"last_error"`
}

// GetTaskThread reads the delivery of one task's thread.
func (s *Store) GetTaskThread(ctx context.Context, taskID int64) (TaskThreadState, bool, error) {
	var x TaskThreadState
	var posted any = &x.Posted
	var postedJSON []byte
	if s.dialect == mysqlDialect {
		posted = &postedJSON
	}
	err := s.db().QueryRow(ctx, s.q(`SELECT task_id,destination,thread_id,thread_name,start_event_id,last_event_id,posted,state,claim_key,attempts,next_attempt_at,last_error
		FROM task_thread_deliveries WHERE task_id=$1 AND destination=$2`, `SELECT task_id,destination,thread_id,thread_name,start_event_id,last_event_id,posted,state,claim_key,attempts,next_attempt_at,last_error
		FROM task_thread_deliveries WHERE task_id=? AND destination=?`), taskID, TaskThreadDestinationDiscord).
		Scan(&x.TaskID, &x.Destination, &x.ThreadID, &x.ThreadName, &x.StartEventID, &x.LastEventID, posted, &x.State, &x.ClaimKey, &x.Attempts, &x.NextAttemptAt, &x.LastError)
	if errors.Is(err, ErrNoRows) {
		return TaskThreadState{}, false, nil
	}
	if err == nil && s.dialect == mysqlDialect {
		// posted is a JSON array of claim keys; an empty one is [], not null.
		x.Posted = []string{}
		_, err = scanJSONB(postedJSON, &x.Posted)
	}
	return x, err == nil, err
}

// claimThreadCreateMySQL claims the first due create the way PostgreSQL's
// "ORDER BY ... LIMIT 1 FOR UPDATE SKIP LOCKED" does. On MySQL that statement
// sorts with a filesort, which locks every due row it examines before it
// returns the first, so one claimer would lock them all and the others would
// find nothing. Instead: read the candidate keys in order without a lock, then
// lock one row at a time by its key, skipping a row another claimer holds.
func claimThreadCreateMySQL(ctx context.Context, tx transaction, orderedQuery, dest string, now time.Time, x *TaskThreadItem) error {
	rows, err := tx.Query(ctx, `SELECT n.task_id FROM task_thread_deliveries n
		WHERE n.destination=? AND n.state IN ('`+TaskThreadPending+`','`+TaskThreadCreating+`') AND n.claim_key='' AND n.next_attempt_at<=?
		ORDER BY n.next_attempt_at,n.start_event_id LIMIT 64`, dest, now)
	if err != nil {
		return err
	}
	ids, err := collectRows(rows, rowTo[int64])
	if err != nil {
		return err
	}
	// The ordered statement with its ORDER BY and LIMIT replaced by a key lookup.
	byKey := strings.Replace(orderedQuery, "ORDER BY n.next_attempt_at,n.start_event_id LIMIT 1 FOR UPDATE OF n SKIP LOCKED", "AND n.task_id=? FOR UPDATE OF n SKIP LOCKED", 1)
	for _, id := range ids {
		err := tx.QueryRow(ctx, byKey, dest, now, id).Scan(&x.TaskID, &x.ThreadName, &x.Attempts, &x.Lookup, &x.Project, &x.Kind, &x.Grade, &x.Tier)
		if errors.Is(err, ErrNoRows) {
			continue
		}
		return err
	}
	return ErrNoRows
}

package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Discord task threads (hk 3990; hk:doc design/2026-11-11/operator-surfaces
// §1). A task in one of the configured projects gets one forum post when it
// first enters a started state, then one line per record about it: PR
// opened, review gate verdict, merged, deployed, deploy failed or rolled back,
// reverted, decision opened, decision answered, and closed. Discord is a view:
// every line is rendered from the record (task_events, changes, deploys,
// decision_requests), and nothing is read back.
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
	dest := TaskThreadDestinationDiscord
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO task_thread_cursors(destination,epoch_event_id,scan_event_id,updated_at)
		SELECT $1::text,coalesce(max(id),0),coalesce(max(id),0),$2::timestamptz FROM task_events ON CONFLICT(destination) DO NOTHING`, dest, now); err != nil {
		return 0, err
	}
	var epoch, cursor int64
	if err = tx.QueryRow(ctx, `SELECT epoch_event_id,scan_event_id FROM task_thread_cursors WHERE destination=$1 FOR UPDATE`, dest).Scan(&epoch, &cursor); err != nil {
		return 0, err
	}
	var upper int64
	if err = tx.QueryRow(ctx, `SELECT coalesce(max(id),$1::bigint) FROM (SELECT id FROM task_events WHERE id>$1::bigint ORDER BY id LIMIT $2) s`, cursor, taskThreadScanBatch).Scan(&upper); err != nil {
		return 0, err
	}
	lower := max(epoch, cursor-taskThreadScanOverlap)
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (e.task_id) e.task_id,e.id,e.at,t.title FROM task_events e JOIN tasks t ON t.id=e.task_id
		WHERE e.id>$1 AND e.id<=$2 AND e.kind='`+TaskEventTransition+`' AND e.applied AND e."to" IN (`+startedStatesSQL+`) AND e."from" NOT IN (`+startedStatesSQL+`)
		AND t.project=ANY($3::text[])
		AND NOT EXISTS (SELECT 1 FROM task_thread_deliveries n WHERE n.task_id=e.task_id AND n.destination=$4)
		ORDER BY e.task_id,e.id`, lower, upper, projects, dest)
	if err != nil {
		return 0, err
	}
	type start struct {
		taskID, eventID int64
		at              time.Time
		title           string
	}
	starts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (start, error) {
		var x start
		err := r.Scan(&x.taskID, &x.eventID, &x.at, &x.title)
		return x, err
	})
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, x := range starts {
		tag, err := tx.Exec(ctx, `INSERT INTO task_thread_deliveries(task_id,destination,thread_name,start_event_id,since,last_event_id,state,next_attempt_at,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$4::bigint-1,'`+TaskThreadPending+`',$6,$6,$6) ON CONFLICT(task_id,destination) DO NOTHING`,
			x.taskID, dest, TaskThreadName(x.taskID, x.title), x.eventID, x.at, now)
		if err != nil {
			return 0, err
		}
		queued += int(tag.RowsAffected())
	}
	if upper > cursor {
		if _, err = tx.Exec(ctx, `UPDATE task_thread_cursors SET scan_event_id=$2,updated_at=$3 WHERE destination=$1`, dest, upper, now); err != nil {
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
	rows, err := s.pool.Query(ctx, `WITH old AS (
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
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TaskThreadRecovery, error) {
		var x TaskThreadRecovery
		err := r.Scan(&x.TaskID, &x.Key, &x.State)
		return x, err
	})
}

// taskThreadPostableEventSQL picks the task events that become a line, for
// the event aliased e: a PR opened or reverted (applied or not), and an
// applied move into merged, deployed or a closed state.
const taskThreadPostableEventSQL = `(e.source IN ('` + SourceMergewakePR + `','` + SourceMergewakeRevert + `')
	OR (e.kind='` + TaskEventTransition + `' AND e.applied AND e."from"<>e."to" AND e."to" IN ('merged','deployed','completed','dropped','duplicate','icebox')))`

// taskThreadItemsSQL lists the next line owed to every active, unclaimed,
// due delivery: per delivery its first postable task event past the cursor,
// and every review gate verdict, deploy failure or rollback, and decision
// opened or answered since the thread's start that it has not posted. $1 is
// the destination, $2 now, $3 the cutoff before which a settled task is no
// longer checked, $4 the cutoff a decision must be older than (as for the
// decisions channel, so the Linear path can supersede it first).
const taskThreadItemsSQL = `WITH d AS (
	SELECT n.task_id,n.since,n.last_event_id,n.posted FROM task_thread_deliveries n JOIN tasks t ON t.id=n.task_id
	WHERE n.destination=$1 AND n.state='` + TaskThreadActive + `' AND n.claim_key='' AND n.next_attempt_at<=$2::timestamptz
	AND NOT (t.state IN (` + settledStatesSQL + `) AND t.updated_at<$3::timestamptz)
), items AS (
	SELECT d.task_id,e.at,'e:'||e.id AS key,1 AS src,e.id AS ref FROM d CROSS JOIN LATERAL (
		SELECT e.id,e.at FROM task_events e WHERE e.task_id=d.task_id AND e.id>d.last_event_id AND ` + taskThreadPostableEventSQL + ` ORDER BY e.id LIMIT 1) e
	UNION
	SELECT d.task_id,c.gate_decided_at,'g:'||c.id||':'||c.gate_head||':'||c.gate_state,2,c.id
	FROM d JOIN change_tasks ct ON ct.task_id=d.task_id JOIN changes c ON c.id=ct.change_id
	WHERE c.gate_state IN ('ready','blocked') AND c.gate_head IS NOT NULL AND c.gate_decided_at>=d.since
	AND NOT (('g:'||c.id||':'||c.gate_head||':'||c.gate_state)=ANY(d.posted))
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

// ClaimTaskThread claims the next send and commits the claim before the
// caller sends: a thread owed a create first (a pending row, or a creating
// row due for its lookup), else the oldest line owed to any active thread.
// found is false when nothing is due.
func (s *Store) ClaimTaskThread(ctx context.Context, now time.Time) (TaskThreadItem, bool, error) {
	now = now.UTC()
	dest := TaskThreadDestinationDiscord
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TaskThreadItem{}, false, err
	}
	defer tx.Rollback(ctx)
	x := TaskThreadItem{Destination: dest, Type: TaskThreadCreate, Key: TaskThreadCreate}
	err = tx.QueryRow(ctx, `SELECT n.task_id,n.thread_name,n.attempts,n.state='`+TaskThreadCreating+`',coalesce(t.project,''),t.kind,coalesce(t.refs->'linear'->>'grade',''),coalesce(t.refs->'linear'->>'tier','')
		FROM task_thread_deliveries n JOIN tasks t ON t.id=n.task_id
		WHERE n.destination=$1 AND n.state IN ('`+TaskThreadPending+`','`+TaskThreadCreating+`') AND n.claim_key='' AND n.next_attempt_at<=$2::timestamptz
		ORDER BY n.next_attempt_at,n.start_event_id LIMIT 1 FOR UPDATE OF n SKIP LOCKED`, dest, now).
		Scan(&x.TaskID, &x.ThreadName, &x.Attempts, &x.Lookup, &x.Project, &x.Kind, &x.Grade, &x.Tier)
	switch {
	case err == nil:
		if _, err = tx.Exec(ctx, `UPDATE task_thread_deliveries SET state='`+TaskThreadCreating+`',claim_key=$3,claimed_at=$4,updated_at=$4 WHERE task_id=$1 AND destination=$2`,
			x.TaskID, dest, x.Key, now); err != nil {
			return TaskThreadItem{}, false, err
		}
		return x, true, tx.Commit(ctx)
	case !errors.Is(err, pgx.ErrNoRows):
		return TaskThreadItem{}, false, err
	}

	x = TaskThreadItem{Destination: dest}
	var src int
	var ref int64
	err = tx.QueryRow(ctx, taskThreadItemsSQL, dest, now, now.Add(-TaskThreadIdleAge), now.Add(-DecisionNoticePostDelay)).Scan(&x.TaskID, &x.Key, &src, &ref, &x.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskThreadItem{}, false, nil
	}
	if err != nil {
		return TaskThreadItem{}, false, err
	}
	err = tx.QueryRow(ctx, `SELECT thread_id,thread_name,attempts FROM task_thread_deliveries
		WHERE task_id=$1 AND destination=$2 AND state='`+TaskThreadActive+`' AND claim_key='' FOR UPDATE SKIP LOCKED`, x.TaskID, dest).
		Scan(&x.ThreadID, &x.ThreadName, &x.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskThreadItem{}, false, nil
	}
	if err != nil {
		return TaskThreadItem{}, false, err
	}
	if err = loadTaskThreadLine(ctx, tx, &x, src, ref); err != nil {
		return TaskThreadItem{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE task_thread_deliveries SET claim_key=$3,claimed_at=$4,updated_at=$4 WHERE task_id=$1 AND destination=$2`, x.TaskID, dest, x.Key, now); err != nil {
		return TaskThreadItem{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

// loadTaskThreadLine fills a line's type and fields from its record. Only
// structured fields and links are read, never a comment, report or question
// body.
func loadTaskThreadLine(ctx context.Context, tx pgx.Tx, x *TaskThreadItem, src int, ref int64) error {
	switch src {
	case 1:
		var source, to, sourceRef, pr string
		if err := tx.QueryRow(ctx, `SELECT coalesce(source,''),"to",coalesce(source_ref,''),coalesce(refs->>'pr','') FROM task_events WHERE id=$1`, ref).Scan(&source, &to, &sourceRef, &pr); err != nil {
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
		// The key carries the verdict the line reports: g:<change>:<head>:<state>.
		x.Type = TaskLineReview
		x.GateState = x.Key[strings.LastIndex(x.Key, ":")+1:]
		var repo string
		var number int
		f := &x.Open
		if err := tx.QueryRow(ctx, `SELECT c.repo,c.pr_number,
				count(*) FILTER (WHERE f.severity='critical' AND `+findingStatusSQL+`='open'),
				count(*) FILTER (WHERE f.severity='high' AND `+findingStatusSQL+`='open'),
				count(*) FILTER (WHERE f.severity='medium' AND `+findingStatusSQL+`='open'),
				count(*) FILTER (WHERE f.severity='low' AND `+findingStatusSQL+`='open')
			FROM changes c LEFT JOIN change_findings f ON f.change_id=c.id `+findingThreadJoin+`
			WHERE c.id=$1 GROUP BY c.id`, ref).Scan(&repo, &number, &f.Critical, &f.High, &f.Medium, &f.Low); err != nil {
			return err
		}
		x.PR = changeURL(repo, number)
		x.Mention = x.GateState == "blocked" && f.Critical+f.High > 0
	case 3:
		var action, state string
		if err := tx.QueryRow(ctx, `SELECT service,action,state FROM deploys WHERE id=$1`, ref).Scan(&x.Service, &action, &state); err != nil {
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
		if err := tx.QueryRow(ctx, `SELECT coalesce(r.answered_by,''),r.answered_at,coalesce((SELECT x.channel FROM decision_resolutions x
				WHERE x.task_id=r.task_id AND (x.request_id=r.source_ref OR x.revision_request_id=r.source_ref) AND x.kind='answered' ORDER BY x.committed_at DESC,x.resolution_id DESC LIMIT 1),'')
			FROM decision_requests r WHERE r.id=$1`, ref).Scan(&x.AnsweredBy, &at, &x.Channel); err != nil {
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
			set = `last_event_id=greatest(last_event_id,substr(claim_key,3)::bigint),attempts=0,last_error=''`
		default:
			set = `posted=array_append(posted,claim_key),attempts=0,last_error=''`
		}
	case TaskThreadRetry:
		wait := res.RetryAfter
		if wait > 0 {
			attempts = x.Attempts
		} else {
			wait = linearDecisionOpBackoff(x.Attempts)
		}
		if attempts >= taskThreadMaxAttempts {
			gaveUp, set, args = true, `state='`+TaskThreadFailed+`',attempts=$5,last_error=$6`, append(args, attempts, errText)
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
			set, args = `last_event_id=greatest(last_event_id,substr(claim_key,3)::bigint),last_error=$5`, append(args, "line outcome unknown: "+errText)
		default:
			set, args = `posted=array_append(posted,claim_key),last_error=$5`, append(args, "line outcome unknown: "+errText)
		}
	case TaskThreadAmbiguous:
		if !create {
			return false, errors.New("only a task thread create can be ambiguous")
		}
		gaveUp, set, args = true, `state='`+TaskThreadNeedsOperator+`',last_error=$5`, append(args, "create lookup: "+errText)
	default:
		return false, fmt.Errorf("unknown task thread outcome %d", res.Outcome)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE task_thread_deliveries SET `+set+`,claim_key='',claimed_at=NULL,updated_at=$4`+key, args...)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, fmt.Errorf("task thread claim %s on task %d is no longer held", x.Key, x.TaskID)
	}
	return gaveUp, tx.Commit(ctx)
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
	err := s.pool.QueryRow(ctx, `SELECT task_id,destination,thread_id,thread_name,start_event_id,last_event_id,posted,state,claim_key,attempts,next_attempt_at,last_error
		FROM task_thread_deliveries WHERE task_id=$1 AND destination=$2`, taskID, TaskThreadDestinationDiscord).
		Scan(&x.TaskID, &x.Destination, &x.ThreadID, &x.ThreadName, &x.StartEventID, &x.LastEventID, &x.Posted, &x.State, &x.ClaimKey, &x.Attempts, &x.NextAttemptAt, &x.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskThreadState{}, false, nil
	}
	return x, err == nil, err
}

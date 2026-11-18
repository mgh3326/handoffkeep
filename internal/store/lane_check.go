package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// The per-lane wake check (operator decision 418, hk 5201; design
// design/2026-11-18/hk5112-track-b §10). Once per tick the check finds, for
// each design lane, the state that needs a look and queues one lane_check
// notice through the hub. It judges nothing and starts nothing: the design
// session decides what to do on the wake.
//
// Findings are ids only:
//
//	order_unacked   an order to this lane is not acknowledged
//	blocker_landed  a backlog or unstarted task's every blocker is settled,
//	                and the last of them settled after the task last moved
//	kept_backlog    a priority 1 or 2 backlog task older than 24h, with no
//	                open wait and no claim
//	room_to_run     the lane has no started task with a live job while
//	                kept_backlog or blocker_landed is non-empty
//	wait_cleared    a task whose wait cleared since the lane last looked
//
// A lane whose last look carries the current findings hash is skipped, so
// only a change of state re-notices it. The notice's event id carries the
// same hash and a 6-hour bucket, so the hub refuses a repeat inside a bucket
// and an unchanged state is noticed again after it.

// The finding names, in the order a notice lists them.
const (
	LaneFindingOrderUnacked  = "order_unacked"
	LaneFindingBlockerLanded = "blocker_landed"
	LaneFindingKeptBacklog   = "kept_backlog"
	LaneFindingRoomToRun     = "room_to_run"
	LaneFindingWaitCleared   = "wait_cleared"
)

const (
	// laneCheckBucketSeconds is the re-notice interval of one unchanged,
	// unlooked state: 6 hours.
	laneCheckBucketSeconds = 6 * 60 * 60
	// laneCheckNoticeMaxIDs is how many task ids one finding lists in the
	// notice text; the rest are a count.
	laneCheckNoticeMaxIDs = 10
	// laneCheckKeptBacklogAge is how long a priority 1 or 2 backlog task may
	// sit untouched before it is a kept_backlog finding.
	laneCheckKeptBacklogAge = 24 * time.Hour
)

// ErrLaneLookInvalid is a malformed look write: a lane name, a notice event
// id that is not this lane's lane_check id, or a note the store refuses.
var ErrLaneLookInvalid = errors.New("lane_look_invalid")

// LaneCheckFinding is one finding and the task ids it names.
type LaneCheckFinding struct {
	Name    string  `json:"name"`
	TaskIDs []int64 `json:"task_ids"`
}

// LaneCheck is one lane's findings and the identity of its notice.
type LaneCheck struct {
	Lane          string             `json:"lane"`
	Findings      []LaneCheckFinding `json:"findings"`
	FindingsHash  string             `json:"findings_hash"`
	NoticeEventID string             `json:"notice_event_id"`
}

// LaneLook is one recorded look.
type LaneLook struct {
	ID            int64     `json:"id"`
	Lane          string    `json:"lane"`
	NoticeEventID string    `json:"notice_event_id"`
	FindingsHash  string    `json:"findings_hash"`
	Note          string    `json:"note,omitempty"`
	Actor         string    `json:"actor"`
	CreatedAt     time.Time `json:"created_at"`
}

var laneCheckEventRE = regexp.MustCompile(`^lanecheck-(.+)-([0-9a-f]{8})-([0-9]+)$`)

// LaneCheckEventID is the deterministic notice id of one lane's state in one
// 6-hour bucket. The same findings inside a bucket give the same id, which the
// hub refuses as a duplicate.
func LaneCheckEventID(lane, hash string, now time.Time) string {
	return fmt.Sprintf("lanecheck-%s-%s-%d", lane, hash, now.UTC().Unix()/laneCheckBucketSeconds)
}

// ParseLaneCheckEventID splits a lane_check notice id into its lane and
// findings hash. It is how a look binds the id it was given to the state the
// lane actually looked at.
func ParseLaneCheckEventID(eventID string) (lane, hash string, ok bool) {
	m := laneCheckEventRE.FindStringSubmatch(eventID)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// laneCheckHash is the sha8 of the sorted finding tokens ("name:id", and the
// finding name alone when it names no task). A change in any finding or id
// changes it.
func laneCheckHash(findings []LaneCheckFinding) string {
	var parts []string
	for _, f := range findings {
		if len(f.TaskIDs) == 0 {
			parts = append(parts, f.Name)
			continue
		}
		for _, id := range f.TaskIDs {
			parts = append(parts, f.Name+":"+strconv.FormatInt(id, 10))
		}
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:8]
}

// LaneCheckNoticeText is the lane notice body. It lists each finding with up
// to ten task ids plus a count, and says how the lane records its look.
func LaneCheckNoticeText(c LaneCheck) string {
	var b strings.Builder
	fmt.Fprintf(&b, "lane %s needs a look", c.Lane)
	for _, f := range c.Findings {
		fmt.Fprintf(&b, "; %s:", f.Name)
		if len(f.TaskIDs) == 0 {
			b.WriteString(" yes")
			continue
		}
		n := len(f.TaskIDs)
		shown := f.TaskIDs
		if len(shown) > laneCheckNoticeMaxIDs {
			shown = shown[:laneCheckNoticeMaxIDs]
		}
		ids := make([]string, len(shown))
		for i, id := range shown {
			ids[i] = strconv.FormatInt(id, 10)
		}
		fmt.Fprintf(&b, " %s (%d)", strings.Join(ids, ", "), n)
	}
	fmt.Fprintf(&b, " | look: handoffkeep lanes look %s --notice %s", c.Lane, c.NoticeEventID)
	return b.String()
}

// LaneChecks computes every active design lane's findings at now. A lane is a
// project's design_lane; operator-desk is never checked. Only lanes with at
// least one finding are returned, in lane order.
func (s *Store) LaneChecks(ctx context.Context, now time.Time) ([]LaneCheck, error) {
	now = now.UTC()
	lanes, err := s.laneCheckLanes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]LaneCheck, 0, len(lanes))
	for _, lane := range lanes {
		c, err := s.laneCheck(ctx, lane, now)
		if err != nil {
			return nil, err
		}
		if len(c.Findings) > 0 {
			out = append(out, c)
		}
	}
	return out, nil
}

// laneCheckLanes reads the design lanes of active projects, never
// operator-desk.
func (s *Store) laneCheckLanes(ctx context.Context) ([]string, error) {
	rows, err := s.db().Query(ctx, `SELECT DISTINCT design_lane FROM task_projects WHERE design_lane <> '' AND design_lane <> 'operator-desk' ORDER BY design_lane`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lanes []string
	for rows.Next() {
		var lane string
		if err := rows.Scan(&lane); err != nil {
			return nil, err
		}
		lanes = append(lanes, lane)
	}
	return lanes, rows.Err()
}

// laneCheck computes one lane's findings. The store never decides "do this".
func (s *Store) laneCheck(ctx context.Context, lane string, now time.Time) (LaneCheck, error) {
	findings := make([]LaneCheckFinding, 0, 5)
	add := func(name string, ids []int64) {
		findings = append(findings, LaneCheckFinding{Name: name, TaskIDs: ids})
	}

	orderUnacked, err := s.laneCheckIDs(ctx, laneCheckOrderUnackedSQL, lane)
	if err != nil {
		return LaneCheck{}, fmt.Errorf("lane check %s: %w", LaneFindingOrderUnacked, err)
	}
	add(LaneFindingOrderUnacked, orderUnacked)

	blockerLanded, err := s.laneCheckIDs(ctx, laneCheckBlockerLandedSQL, lane)
	if err != nil {
		return LaneCheck{}, fmt.Errorf("lane check %s: %w", LaneFindingBlockerLanded, err)
	}
	add(LaneFindingBlockerLanded, blockerLanded)

	keptBacklog, err := s.laneCheckIDs(ctx, laneCheckKeptBacklogSQL, lane, now.Add(-laneCheckKeptBacklogAge))
	if err != nil {
		return LaneCheck{}, fmt.Errorf("lane check %s: %w", LaneFindingKeptBacklog, err)
	}
	add(LaneFindingKeptBacklog, keptBacklog)

	live, err := s.laneCheckLive(ctx, lane)
	if err != nil {
		return LaneCheck{}, fmt.Errorf("lane check %s: %w", LaneFindingRoomToRun, err)
	}
	if !live && len(keptBacklog)+len(blockerLanded) > 0 {
		// The room names the work that could start, so a notice that says
		// room_to_run lists those tasks.
		room := append(append([]int64{}, blockerLanded...), keptBacklog...)
		sort.Slice(room, func(i, j int) bool { return room[i] < room[j] })
		add(LaneFindingRoomToRun, room)
	}

	// wait_cleared needs the lane's last look as its baseline: a first notice
	// cannot say what was cleared since a look that never happened.
	lastLook, looked, err := s.laneCheckLastLook(ctx, lane)
	if err != nil {
		return LaneCheck{}, fmt.Errorf("lane check %s: %w", LaneFindingWaitCleared, err)
	}
	var waitCleared []int64
	if looked {
		waitCleared, err = s.laneCheckIDs(ctx, laneCheckWaitClearedSQL, lane, lastLook)
		if err != nil {
			return LaneCheck{}, fmt.Errorf("lane check %s: %w", LaneFindingWaitCleared, err)
		}
	}
	add(LaneFindingWaitCleared, waitCleared)

	// Drop the empty findings so the hash and the text name only real state.
	kept := findings[:0]
	for _, f := range findings {
		if len(f.TaskIDs) > 0 {
			kept = append(kept, f)
		}
	}
	hash := laneCheckHash(kept)
	return LaneCheck{
		Lane:          lane,
		Findings:      kept,
		FindingsHash:  hash,
		NoticeEventID: LaneCheckEventID(lane, hash, now),
	}, nil
}

// laneCheckIDs runs one finding query, binding lane plus any extra arguments.
func (s *Store) laneCheckIDs(ctx context.Context, query, lane string, extra ...any) ([]int64, error) {
	args := append([]any{lane}, extra...)
	rows, err := s.db().Query(ctx, s.q(query, mysqlPlaceholders(query)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// mysqlPlaceholders turns a PostgreSQL statement's $n parameters into MySQL's
// ? markers, in order. The finding statements use $n once each.
func mysqlPlaceholders(query string) string {
	return placeholderRE.ReplaceAllString(query, "?")
}

const laneCheckOrderUnackedSQL = `SELECT o.task_id FROM task_orders o
WHERE o.target_lane=$1 AND o.acked_at IS NULL AND o.withdrawn_at IS NULL
ORDER BY o.created_at, o.id`

// laneCheckBlockerLandedSQL: a backlog or unstarted task whose every live
// blocks source is settled, and whose newest settled source moved after the
// task last moved.
const laneCheckBlockerLandedSQL = `SELECT t.id FROM tasks t
WHERE t.lane=$1 AND t.state IN ('backlog','unstarted')
  AND EXISTS (SELECT 1 FROM task_relations r WHERE r.kind='blocks' AND r.dst_id=t.id AND r.archived_at IS NULL)
  AND NOT EXISTS (SELECT 1 FROM task_relations r JOIN tasks b ON b.id=r.src_id
                  WHERE r.kind='blocks' AND r.dst_id=t.id AND r.archived_at IS NULL AND b.state NOT IN (` + settledStatesSQL + `))
  AND EXISTS (SELECT 1 FROM task_relations r JOIN tasks b ON b.id=r.src_id
              WHERE r.kind='blocks' AND r.dst_id=t.id AND r.archived_at IS NULL
                AND b.updated_at > COALESCE((SELECT max(e.at) FROM task_events e WHERE e.task_id=t.id AND e.kind='transition'), t.created_at))
ORDER BY t.id`

// laneCheckKeptBacklogSQL: a priority 1 or 2 backlog task older than the
// second argument (an instant), with no open hand wait and no claim.
const laneCheckKeptBacklogSQL = `SELECT t.id FROM tasks t
WHERE t.lane=$1 AND t.state='backlog' AND t.priority IN (1,2)
  AND t.created_at < $2 AND t.claimed_by=''
  AND NOT EXISTS (SELECT 1 FROM task_waits w WHERE w.task_id=t.id AND w.cleared_at IS NULL)
ORDER BY t.priority, t.created_at, t.id`

const laneCheckWaitClearedSQL = `SELECT w.task_id FROM task_waits w JOIN tasks t ON t.id=w.task_id
WHERE t.lane=$1 AND w.cleared_at IS NOT NULL AND w.cleared_at > $2
ORDER BY w.cleared_at DESC, w.id DESC`

// laneCheckLive reports whether the lane has a task in developing or
// reviewing with a live job.
func (s *Store) laneCheckLive(ctx context.Context, lane string) (bool, error) {
	var live bool
	err := s.db().QueryRow(ctx, s.q(`SELECT EXISTS(SELECT 1 FROM tasks t JOIN jobs j ON j.task_id=t.id
		WHERE t.lane=$1 AND t.state IN ('developing','reviewing') AND j.state IN ('claimed','spawned'))`,
		`SELECT EXISTS(SELECT 1 FROM tasks t JOIN jobs j ON j.task_id=t.id
		WHERE t.lane=? AND t.state IN ('developing','reviewing') AND j.state IN ('claimed','spawned'))`), lane).Scan(&live)
	return live, err
}

// laneCheckLastLook returns the lane's newest look time, and whether one
// exists.
func (s *Store) laneCheckLastLook(ctx context.Context, lane string) (time.Time, bool, error) {
	var at time.Time
	err := s.db().QueryRow(ctx, s.q(
		`SELECT created_at FROM lane_looks WHERE lane=$1 ORDER BY id DESC LIMIT 1`,
		`SELECT created_at FROM lane_looks WHERE lane=? ORDER BY id DESC LIMIT 1`), lane).Scan(&at)
	if errors.Is(err, ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return at.UTC(), true, nil
}

// laneCheckLooked reports whether the lane's last look carries hash.
func (s *Store) laneCheckLooked(ctx context.Context, q rowQuerier, lane, hash string) (bool, error) {
	var last string
	err := q.QueryRow(ctx, s.q(
		`SELECT findings_hash FROM lane_looks WHERE lane=$1 ORDER BY id DESC LIMIT 1`,
		`SELECT findings_hash FROM lane_looks WHERE lane=? ORDER BY id DESC LIMIT 1`), lane).Scan(&last)
	if errors.Is(err, ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return last == hash, nil
}

// RunLaneCheck computes every lane's findings and queues one lane_check
// notice per lane that has findings and whose last look does not carry the
// current hash. Event ids are deterministic per lane, hash and bucket, so a
// repeated run inside a bucket writes nothing new. It returns how many rows
// this call wrote.
func (s *Store) RunLaneCheck(ctx context.Context, now time.Time) (int, error) {
	now = now.UTC()
	checks, err := s.LaneChecks(ctx, now)
	if err != nil {
		return 0, err
	}
	if len(checks) == 0 {
		return 0, nil
	}
	tx, release, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if s.dialect == mysqlDialect {
		if _, err = mysqlTransactionLock(ctx, tx, mysqlLockKey("text", "lane_check_notices"), false); err != nil {
			return 0, err
		}
	} else if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('lane_check_notices', 0))`); err != nil {
		return 0, err
	}
	wrote := 0
	for _, c := range checks {
		looked, err := s.laneCheckLooked(ctx, tx, c.Lane, c.FindingsHash)
		if err != nil {
			return 0, err
		}
		if looked {
			continue
		}
		ok, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindLaneCheck, c.Lane, c.NoticeEventID, LaneCheckNoticeText(c), now)
		if err != nil {
			return 0, err
		}
		if ok {
			wrote++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return wrote, nil
}

const laneLookColumns = `id,lane,notice_event_id,findings_hash,note,actor,created_at`

// RecordLaneLook records that a lane looked at the state its notice names
// (`lanes look <lane> --notice <event_id>`). The findings hash comes from the
// notice id, which is this lane's lane_check id; the next check skips the lane
// while its findings still hash to that value. A repeat of the same
// (lane, notice) changes nothing.
func (s *Store) RecordLaneLook(ctx context.Context, lane, noticeEventID, note, actor string, now time.Time) (LaneLook, bool, error) {
	lane = strings.TrimSpace(lane)
	noticeEventID = strings.TrimSpace(noticeEventID)
	note = strings.TrimSpace(note)
	if !validName(lane) {
		return LaneLook{}, false, fmt.Errorf("%w: lane must be a name", ErrLaneLookInvalid)
	}
	parsedLane, hash, ok := ParseLaneCheckEventID(noticeEventID)
	if !ok || parsedLane != lane {
		return LaneLook{}, false, fmt.Errorf("%w: notice must be lanecheck-%s-<hash>-<bucket>", ErrLaneLookInvalid, lane)
	}
	if actor == "" || !validText(actor, 128) {
		return LaneLook{}, false, fmt.Errorf("%w: actor is required", ErrLaneLookInvalid)
	}
	if !validText(note, MaxBytes) {
		return LaneLook{}, false, fmt.Errorf("%w: note must be at most %d bytes", ErrLaneLookInvalid, MaxBytes)
	}
	if err := guard.Reject(note); err != nil {
		return LaneLook{}, false, err
	}
	now = now.UTC()
	tx, release, err := s.begin(ctx)
	if err != nil {
		return LaneLook{}, false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	var look LaneLook
	if s.dialect == mysqlDialect {
		_, err = tx.Exec(ctx, `INSERT INTO lane_looks(lane,notice_event_id,findings_hash,note,actor,created_at) VALUES(?,?,?,?,?,?)`,
			lane, noticeEventID, hash, note, actor, now)
		if errors.Is(err, errUniqueViolation) {
			err = scanLaneLook(tx.QueryRow(ctx, `SELECT `+laneLookColumns+` FROM lane_looks WHERE lane=? AND notice_event_id=?`, lane, noticeEventID), &look)
			if err != nil {
				return LaneLook{}, false, err
			}
			return look, false, nil
		}
		if err != nil {
			return LaneLook{}, false, err
		}
		if err = scanLaneLook(tx.QueryRow(ctx, `SELECT `+laneLookColumns+` FROM lane_looks WHERE lane=? AND notice_event_id=?`, lane, noticeEventID), &look); err != nil {
			return LaneLook{}, false, err
		}
	} else {
		err = scanLaneLook(tx.QueryRow(ctx, `INSERT INTO lane_looks(lane,notice_event_id,findings_hash,note,actor,created_at)
			VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (lane,notice_event_id) DO NOTHING
			RETURNING `+laneLookColumns, lane, noticeEventID, hash, note, actor, now), &look)
		if errors.Is(err, ErrNoRows) {
			if err = scanLaneLook(tx.QueryRow(ctx, `SELECT `+laneLookColumns+` FROM lane_looks WHERE lane=$1 AND notice_event_id=$2`, lane, noticeEventID), &look); err != nil {
				return LaneLook{}, false, err
			}
			return look, false, nil
		}
		if err != nil {
			return LaneLook{}, false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return LaneLook{}, false, err
	}
	return look, true, nil
}

func scanLaneLook(r row, x *LaneLook) error {
	if err := r.Scan(&x.ID, &x.Lane, &x.NoticeEventID, &x.FindingsHash, &x.Note, &x.Actor, &x.CreatedAt); err != nil {
		return err
	}
	x.CreatedAt = x.CreatedAt.UTC()
	return nil
}

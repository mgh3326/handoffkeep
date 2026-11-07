package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// dots is an external research AI that answers on Linear issues labelled
// "dots" with comments starting "[dots] <KIND>". These records are written by
// two independent paths, the Linear webhook and an hourly reconciling poller
// that keeps its own seen-set. The key, the body and the event ids below must
// stay byte-identical to the poller's: the document key is what makes a second
// write a no-op, and the shared event id is what lets the hub dedupe the lane
// notification when both paths notify the same lane (it keys on lane and id).

const (
	// DotsLabel is the Linear label that puts an issue in dots' scope.
	DotsLabel = "dots"
	// DotsLane is the session of every dots document and the lane a
	// notification goes to when its request names no other (dotsRequesterLane).
	DotsLane = "operator-desk"

	dotsCreatedBy = "linear-webhook"
)

// dotsResultKinds maps a comment kind that is recorded to the outbox kind that
// notifies about it. Everything else ("START", unknown words) is not recorded.
var dotsResultKinds = map[string]string{
	"RESULT":   OutboxKindDotsReport,
	"BLOCKED":  OutboxKindDotsDecision,
	"QUESTION": OutboxKindDotsDecision,
}

var (
	dotsIdentifierRE = regexp.MustCompile(`^[A-Za-z0-9_]+-[0-9]+$`)
	dotsCommentIDRE  = regexp.MustCompile(`^[0-9A-Za-z-]{8,64}$`)
	// dotsLaneDeclRE finds the first line that declares a lane; dotsLaneLineRE
	// judges it, its group being nameRE.
	dotsLaneDeclRE = regexp.MustCompile(`(?m)^hk-lane:.*$`)
	dotsLaneLineRE = regexp.MustCompile(`^hk-lane:[ \t]*([A-Za-z0-9._-]{1,128})[ \t]*\r?$`)
)

// dotsRequesterLane picks the lane owed a dots notification: the lane named
// on the first "hk-lane:" line of the description, else the lane of the one
// task linked to the issue, else DotsLane. Only the first "hk-lane:" line is
// read: when it names no valid lane, the next rule applies, not a later line.
func dotsRequesterLane(description string, linkedTask *Task) string {
	if m := dotsLaneLineRE.FindStringSubmatch(dotsLaneDeclRE.FindString(description)); m != nil {
		return m[1]
	}
	if linkedTask != nil && validName(linkedTask.Lane) {
		return linkedTask.Lane
	}
	return DotsLane
}

// DotsIssue is the part of a Linear issue a dots record quotes. IssueUUID is
// Linear's durable id, the external_id of a task origin; Description is read
// for the hk-lane line only.
type DotsIssue struct {
	Identifier  string
	Title       string
	URL         string
	IssueUUID   string
	Description string
}

// DotsRequestInput describes a request filed directly in hk.
type DotsRequestInput struct {
	Title         string `json:"title"`
	Body          string `json:"body"`
	RequesterLane string `json:"requester_lane,omitempty"`
	Project       string `json:"project,omitempty"`
}

// CreateDotsRequest creates a standalone request and its body in one transaction.
func (s *Store) CreateDotsRequest(ctx context.Context, by string, in DotsRequestInput) (Task, error) {
	lane := in.RequesterLane
	if lane == "" {
		lane = by
	}
	if !validName(lane) || by == "" || !validText(by, 128) || strings.TrimSpace(in.Body) == "" {
		return Task{}, errors.New("invalid dots request")
	}
	var project *string
	if in.Project != "" {
		project = &in.Project
	}
	item := triageItem{Title: in.Title, Body: in.Body, Kind: "verify", Project: project}
	if err := checkTriageItem(item); err != nil {
		return Task{}, err
	}
	var key [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		return Task{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, err
	}
	x, err := insertTriageTaskTx(ctx, tx, item, "dots", hex.EncodeToString(key[:]), "dots", nil, by, time.Now().UTC())
	if err != nil {
		return Task{}, err
	}
	err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET state=$2,parent_lane=$3,started_triage_at=NULL WHERE id=$1 RETURNING `+taskColumns, x.ID, StateBacklog, lane), &x)
	if err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}

// DotsInbox lists open hk requests in creation order after a task cursor.
func (s *Store) DotsInbox(ctx context.Context, after int64, limit int) ([]Task, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("invalid dots inbox cursor or limit")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE lane='dots' AND source='dots' AND state NOT IN (`+settledStatesSQL+`) AND ($1::bigint=0 OR (created_at,id) > (SELECT created_at,id FROM tasks WHERE id=$1 AND lane='dots' AND source='dots')) ORDER BY created_at,id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var task Task
		if err := scanTask(rows, &task); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// DotsReplyInput is a structured reply to one hk request.
type DotsReplyInput struct {
	TaskID         int64  `json:"task_id"`
	Kind           string `json:"kind"`
	Body           string `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`
	AskOperator    bool   `json:"ask_operator,omitempty"`
}

type DotsReplyResult struct {
	Task        Task   `json:"task"`
	DocumentKey string `json:"document_key,omitempty"`
	Duplicate   bool   `json:"duplicate"`
}

// ReplyDots writes the reply, its state change, and its recorded outputs together.
func (s *Store) ReplyDots(ctx context.Context, actor, channel string, in DotsReplyInput) (DotsReplyResult, error) {
	if in.TaskID < 1 || !validText(actor, 128) || actor == "" || channel != "chatgpt" || !validSourceRef(in.IdempotencyKey) || (in.AskOperator && in.Kind != "QUESTION") {
		return DotsReplyResult{}, errors.New("invalid dots reply")
	}
	if in.Kind != "START" && !DotsKindRecorded(in.Kind) {
		return DotsReplyResult{}, errors.New("invalid dots reply kind")
	}
	if err := CheckTaskCommentBody(in.Body); err != nil {
		return DotsReplyResult{}, err
	}
	if err := guard.Reject(in.IdempotencyKey); err != nil {
		return DotsReplyResult{}, err
	}
	keyHash := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", in.TaskID, in.IdempotencyKey)))
	commentID := hex.EncodeToString(keyHash[:])
	payloadHash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%t\x00%s\x00%s", in.Kind, in.Body, in.AskOperator, actor, channel)))
	payload := hex.EncodeToString(payloadHash[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DotsReplyResult{}, err
	}
	defer tx.Rollback(ctx)
	// Comment ids must become visible in id order; take its lock before any
	// task lock, as required by the comment writer and lockorder.go.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, taskCommentInsertLock); err != nil {
		return DotsReplyResult{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return DotsReplyResult{}, err
	}
	var task Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, in.TaskID), &task); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DotsReplyResult{}, ErrTaskNotFound
		}
		return DotsReplyResult{}, err
	}
	if task.Lane != "dots" || task.Source != "dots" {
		return DotsReplyResult{}, errors.New("invalid dots request task")
	}
	var previous string
	err = tx.QueryRow(ctx, `SELECT note FROM task_events WHERE task_id=$1 AND source='dots-hk' AND source_ref=$2`, in.TaskID, commentID).Scan(&previous)
	if err == nil {
		if previous != payload {
			return DotsReplyResult{}, errors.New("dots idempotency key reused with different reply")
		}
		var docKey string
		err = tx.QueryRow(ctx, `SELECT document_key FROM dots_results WHERE comment_id=$1`, commentID).Scan(&docKey)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return DotsReplyResult{}, err
		}
		return DotsReplyResult{Task: task, DocumentKey: docKey, Duplicate: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DotsReplyResult{}, err
	}
	now := time.Now().UTC()
	if _, err = tx.Exec(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,$2,$3,$4)`, in.TaskID, in.Body, actor, now); err != nil {
		return DotsReplyResult{}, err
	}
	var docKey string
	if in.Kind != "START" {
		issue := DotsIssue{Identifier: "hk-" + strconv.FormatInt(task.ID, 10), Title: task.Title, URL: fmt.Sprintf("hk:task/%d", task.ID), IssueUUID: fmt.Sprintf("hk:%d", task.ID)}
		comment := DotsComment{ID: commentID, CreatedAt: now.Format(time.RFC3339Nano), Body: "[dots] " + in.Kind + "\n" + in.Body}
		if _, ok := DotsKind(comment.Body); !ok {
			return DotsReplyResult{}, errors.New("invalid dots reply")
		}
		docKey, _, err = recordDotsResultTx(ctx, tx, in.Kind, issue, comment, &task, task.ParentLane, "hk-mcp")
		if err != nil {
			return DotsReplyResult{}, err
		}
	}
	switch in.Kind {
	case "START":
		task, err = s.claimTaskTx(ctx, tx, task.ID, "dots", "", "dots request")
	case "BLOCKED":
		task, err = s.transitionTaskTx(ctx, tx, task.ID, TransitionInput{To: "hold", By: actor, Note: in.Body}, false)
	case "RESULT":
		task, err = s.transitionTaskTx(ctx, tx, task.ID, TransitionInput{To: StateCompleted, By: actor, Evidence: "hk:doc " + docKey}, false)
	case "QUESTION":
		if in.AskOperator {
			var decision DecisionRequestResult
			decision, err = s.recordDecisionRequestTx(ctx, tx, task.ID, actor, DecisionRequestInput{Question: in.Body, Options: DecisionOptions{Options: []DecisionOption{{Key: "A", Label: "Provide an answer"}}, AllowFree: true}, DefaultAction: "Wait for an answer", Block: true})
			task = decision.Task
		}
	}
	if err != nil {
		return DotsReplyResult{}, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: task.ID, Kind: "source_event", By: actor, Note: payload, At: now, Source: "dots-hk", SourceRef: commentID}); err != nil {
		return DotsReplyResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return DotsReplyResult{}, err
	}
	return DotsReplyResult{Task: task, DocumentKey: docKey}, nil
}

// DotsComment is one Linear comment. CreatedAt keeps Linear's own ISO-8601
// text: the document key takes its date from that text, not from a parsed and
// re-rendered time.
type DotsComment struct {
	ID        string
	CreatedAt string
	Body      string
}

// DotsKind classifies a comment. dots is false when the comment does not start
// with "[dots]"; a bare "[dots]" heading is kind "?", which is never recorded.
// The kind word ends at a colon, so "[dots] RESULT: <verdict>" is a RESULT
// (ROB-1497 on 2026-11-05 was dropped as kind "RESULT:").
func DotsKind(body string) (kind string, dots bool) {
	word, _, dots := dotsHeading(body)
	if !dots {
		return "", false
	}
	if word == "" {
		return "?", true
	}
	return strings.ToUpper(word), true
}

// dotsHeading splits the first line after "[dots]" into the kind word and,
// when that word ends at a colon, the verdict that follows it on the line.
func dotsHeading(body string) (word, verdict string, dots bool) {
	head, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	rest, ok := strings.CutPrefix(head, "[dots]")
	if !ok {
		return "", "", false
	}
	rest = strings.TrimSpace(rest)
	words := strings.Fields(rest)
	if len(words) == 0 {
		return "", "", true
	}
	word = words[0]
	if i := strings.IndexAny(word, ":："); i >= 0 {
		_, size := utf8.DecodeRuneInString(word[i:])
		verdict = strings.TrimSpace(rest[i+size:])
		word = word[:i]
	}
	return word, verdict, true
}

// DotsKindRecorded reports whether a comment of this kind becomes a document.
func DotsKindRecorded(kind string) bool {
	_, ok := dotsResultKinds[kind]
	return ok
}

// firstRunes is Python's s[:n]: n code points, not n bytes.
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// dotsSummaryLine is the verdict on a "[dots] KIND: <verdict>" heading, else
// the first substantive line after the heading, skipping a "결론" heading and
// table rows.
func dotsSummaryLine(body string) string {
	if _, verdict, _ := dotsHeading(body); verdict != "" {
		return firstRunes(verdict, 220)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	for _, line := range lines[1:] {
		s := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if s != "" && s != "결론" && !strings.HasPrefix(s, "|") {
			return firstRunes(s, 220)
		}
	}
	return ""
}

func dotsResultKey(issue DotsIssue, c DotsComment) string {
	return "dots/results/" + issue.Identifier + "/" + c.CreatedAt[:10] + "-" + c.ID[:8]
}

func dotsResultBody(kind string, issue DotsIssue, c DotsComment) string {
	return "# [dots] " + kind + " " + issue.Identifier + " — " + issue.Title + "\n\n" + issue.URL + "\n\n" + c.Body + "\n"
}

// dotsLaneText makes a notification drainable: the hub rejects C0/C1 control
// characters and text over the lane-event byte cap.
func dotsLaneText(text string) string {
	return assistantLaneText("", strings.Map(func(r rune) rune {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, text))
}

func dotsResultText(kind, key string, issue DotsIssue, c DotsComment) string {
	return dotsLaneText("[dots] " + kind + " " + issue.Identifier + " " + firstRunes(issue.Title, 60) + " :: " + dotsSummaryLine(c.Body) + " → hk:doc " + key + " · " + issue.URL)
}

func dotsFollowupText(issue DotsIssue) string {
	return dotsLaneText("[dots-watch] " + issue.Identifier + " is closed but got a new comment after closing; dots will not pick it up. Reopen it if it is a request. " + issue.URL)
}

func validDotsIssue(issue DotsIssue, commentID string) bool {
	return dotsIdentifierRE.MatchString(issue.Identifier) && dotsCommentIDRE.MatchString(commentID) && originUUIDRE.MatchString(issue.IssueUUID)
}

// dotsLinkedTask returns the one task whose Linear origin is the issue, or
// nil when none or several are: an ambiguous link is unknown, not a guess.
// The caller's later write that names the task takes KEY SHARE on its row.
func dotsLinkedTask(ctx context.Context, tx pgx.Tx, issueUUID string) (*Task, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT o.task_id, t.lane FROM task_origins o JOIN tasks t ON t.id = o.task_id WHERE o.provider = $1 AND o.external_id = $2 ORDER BY o.task_id LIMIT 2`, OriginLinear, issueUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Lane); err != nil {
			return nil, err
		}
		found = append(found, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(found) != 1 {
		return nil, nil
	}
	return &found[0], nil
}

// RecordDotsResult writes the document for one dots answer, its dots_results
// row and the lane notification owed for it, in one transaction. The first
// writer of a key wins: a redelivery, or a poller that got there first, keeps
// the document body and queues nothing (that poller notified on its own), but
// the dots_results row is still written and a NULL task link filled in.
// created reports whether this call wrote the document.
//
// Locks (lockorder.go, level 4): the documents row and the dots_results row,
// each insert or update taking KEY SHARE on the one linked task row.
func (s *Store) RecordDotsResult(ctx context.Context, kind string, issue DotsIssue, c DotsComment) (key string, created bool, err error) {
	if !validDotsIssue(issue, c.ID) {
		return "", false, errors.New("invalid dots result")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)
	task, err := dotsLinkedTask(ctx, tx, issue.IssueUUID)
	if err != nil {
		return "", false, err
	}
	key, created, err = recordDotsResultTx(ctx, tx, kind, issue, c, task, dotsRequesterLane(issue.Description, task), dotsCreatedBy)
	if err != nil {
		return "", false, err
	}
	return key, created, tx.Commit(ctx)
}

func recordDotsResultTx(ctx context.Context, tx pgx.Tx, kind string, issue DotsIssue, c DotsComment, task *Task, lane, createdBy string) (key string, created bool, err error) {
	outboxKind, recorded := dotsResultKinds[kind]
	if !recorded {
		return "", false, errors.New("invalid dots result")
	}
	if _, err := time.Parse(time.RFC3339, c.CreatedAt); err != nil {
		return "", false, errors.New("invalid dots result")
	}
	key = dotsResultKey(issue, c)
	body := dotsResultBody(kind, issue, c)
	if !validDocKey(key) || !validText(body, DocumentMaxBytes) {
		return "", false, errors.New("invalid dots result")
	}
	if err := guard.Reject(key + "\n" + body); err != nil {
		return "", false, err
	}
	sum := sha256.Sum256([]byte(body))
	artefact := sha256.Sum256([]byte(c.Body))
	verdict := nullIfEmpty(dotsSummaryLine(c.Body))
	now := time.Now().UTC()
	var taskID *int64
	if task != nil {
		taskID = &task.ID
	}
	tag, err := tx.Exec(ctx, `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at,origin_class,task_id) VALUES($1,'note',$2,'',$3,$4,$5,$6,$6,'import',$7) ON CONFLICT(key) DO NOTHING`, key, DotsLane, body, hex.EncodeToString(sum[:]), createdBy, now, taskID)
	if err != nil {
		return "", false, err
	}
	created = tag.RowsAffected() == 1
	if !created && taskID != nil {
		if _, err := tx.Exec(ctx, `UPDATE documents SET task_id=$1 WHERE key=$2 AND task_id IS NULL`, *taskID, key); err != nil {
			return "", false, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO dots_results(document_key,comment_id,issue_uuid,issue_identifier,kind,verdict,artefact_sha256,task_id,routed_lane,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		ON CONFLICT(comment_id) DO UPDATE SET task_id=EXCLUDED.task_id, updated_at=EXCLUDED.updated_at
		WHERE dots_results.task_id IS NULL AND EXCLUDED.task_id IS NOT NULL`,
		key, c.ID, issue.IssueUUID, issue.Identifier, kind, verdict, hex.EncodeToString(artefact[:]), taskID, lane, now); err != nil {
		return "", false, err
	}
	if created {
		if err := insertNotificationOutbox(ctx, tx, outboxKind, lane, "dots-"+c.ID, dotsResultText(kind, key, issue, c), now); err != nil {
			return "", false, err
		}
	}
	return key, created, nil
}

// RecordDotsFollowup records the flag for a non-dots comment added after an
// issue closed: dots ignores closed issues, so the comment would otherwise be
// lost. created is false when this comment was already flagged.
func (s *Store) RecordDotsFollowup(ctx context.Context, issue DotsIssue, commentID string) (created bool, err error) {
	if !validDotsIssue(issue, commentID) {
		return false, errors.New("invalid dots followup")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	task, err := dotsLinkedTask(ctx, tx, issue.IssueUUID)
	if err != nil {
		return false, err
	}
	created, err = insertNotificationOutboxOnce(ctx, tx, OutboxKindDotsFollowup, dotsRequesterLane(issue.Description, task), "dots-followup-"+commentID, dotsFollowupText(issue), time.Now().UTC())
	if err != nil {
		return false, err
	}
	return created, tx.Commit(ctx)
}

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

// The desk asks the operator through a Linear issue; the operator answers there
// with a short comment or by moving the issue to a mapped state. Each request
// has at most one issue, bound by ver, a hash of what was asked: revising the
// question makes a new request, a new ver and a new issue. Answers land in the
// existing decision model (refs.decision_request plus its task_events snapshot),
// so the audit record needs no table of its own.

const (
	// LinearDecisionLabel puts an issue in the decision flow.
	LinearDecisionLabel = "decision"
	// LinearDecisionLane is the lane told about every answer.
	LinearDecisionLane = "operator-desk"

	linearDecisionRecorder  = "linear-webhook"
	linearDecisionResponder = "operator"

	// LinearDecisionOpComment posts a comment, LinearDecisionOpClose moves an
	// issue to a state, and LinearDecisionOpDiscord posts to the decisions channel.
	LinearDecisionOpComment = "comment"
	LinearDecisionOpClose   = "close"
	LinearDecisionOpDiscord = "discord"

	// LinearDecisionSourceComment and LinearDecisionSourceState name how an
	// answer arrived.
	LinearDecisionSourceComment = "comment"
	LinearDecisionSourceState   = "state"

	// With these, an op keeps trying for about two days, which outlasts a Linear
	// or Discord outage. What still fails then needs a person.
	linearDecisionOpMaxAttempts = 56
	linearDecisionOpBaseBackoff = 30 * time.Second
	linearDecisionOpMaxBackoff  = time.Hour
	linearDecisionErrorMaxBytes = 500
)

// ErrLinearDecisionOpSkipped is what a sender returns for an op it is configured
// not to send. The row is settled, so turning the sender on later does not replay
// old messages.
var ErrLinearDecisionOpSkipped = errors.New("linear_decision_op_skipped")

// ErrLinearDecisionOpGaveUp marks the failure that used an op's last attempt, so
// the worker can log it apart from a failure that will be retried.
var ErrLinearDecisionOpGaveUp = errors.New("linear_decision_op_gave_up")

// linearDecisionOpBackoff is the wait after a failed attempt, given the attempts
// made before it.
func linearDecisionOpBackoff(attempts int) time.Duration {
	return min(linearDecisionOpBaseBackoff<<min(attempts, 8), linearDecisionOpMaxBackoff)
}

// DecisionLinearAudit is the record of one answer given on Linear. DeliveryID is
// Linear's id for the webhook delivery that carried it.
type DecisionLinearAudit struct {
	DecisionID string `json:"decision_id"`
	Ver        string `json:"ver"`
	Choice     string `json:"choice"`
	RawText    string `json:"raw_text"`
	UserID     string `json:"user_id"`
	Source     string `json:"source"`
	CommentID  string `json:"comment_id,omitempty"`
	IssueID    string `json:"issue_id"`
	DeliveryID string `json:"delivery_id,omitempty"`
	EventAt    string `json:"event_at,omitempty"`
}

// LinearDecisionLink binds one decision request to its Linear issue.
// SupersededBy names the request that replaced it, once one has.
type LinearDecisionLink struct {
	RequestID    string    `json:"request_id"`
	TaskID       int64     `json:"task_id"`
	Ver          string    `json:"ver"`
	IssueID      string    `json:"issue_id"`
	Identifier   string    `json:"identifier"`
	URL          string    `json:"url"`
	SupersededBy string    `json:"superseded_by,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// DecisionAskResult is what asking through Linear returns. Issue is nil when the
// request was already closed before it ever got an issue.
type DecisionAskResult struct {
	Task         Task                `json:"task"`
	Request      DecisionRequest     `json:"request"`
	DecisionRef  string              `json:"decision_ref"`
	Ver          string              `json:"ver"`
	Issue        *LinearDecisionLink `json:"issue,omitempty"`
	Duplicate    bool                `json:"duplicate"`
	IssueCreated bool                `json:"issue_created"`
}

// LinearDecisionOp is one write still owed to Linear.
type LinearDecisionOp struct {
	ID        int64  `json:"id"`
	DedupeKey string `json:"dedupe_key"`
	Op        string `json:"op"`
	IssueID   string `json:"issue_id"`
	Body      string `json:"body"`
	StateID   string `json:"state_id"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
}

// DecisionRef is the id the operator sees: dr-2615-1 reads D-2615-1.
func DecisionRef(requestID string) string {
	return "D-" + strings.TrimPrefix(requestID, "dr-")
}

// DecisionRequestVer is the first 8 hex digits of the SHA-256 of what was asked.
// The JSON encoding keeps field boundaries, so moving text between the question
// and an option changes the hash.
func DecisionRequestVer(question string, options DecisionOptions, doc string, refs []string) string {
	allRefs := []string{}
	if doc != "" {
		allRefs = append(allRefs, doc)
	}
	allRefs = append(allRefs, refs...)
	encoded, err := json.Marshal(struct {
		Question string           `json:"question"`
		Options  []DecisionOption `json:"options"`
		Refs     []string         `json:"refs"`
	}{question, options.Options, allRefs})
	if err != nil {
		// Strings and plain structs always marshal.
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])[:8]
}

// LinearDecisionUUID derives a UUID v4 shaped id from key. Linear accepts a
// client-supplied id on create, so a retried create of the same key cannot make
// a second issue or comment.
func LinearDecisionUUID(key string) string {
	sum := sha256.Sum256([]byte("handoffkeep-linear-decision:" + key))
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	h := hex.EncodeToString(sum[:16])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// LinearDecisionComment is the parsed first line of an answer comment.
type LinearDecisionComment struct {
	Ref    string
	Ver    string
	Choice string
}

// The first line is `<choice>`, optionally preceded by `D-<id>` and a ver:
// `1`, `A`, `D-2615-1 1`, `D-2615-1 ab12cd3e 1`, `1 ver=ab12cd3e`. Nothing else
// may share the line, so "a good idea" stays an ordinary comment. Notes go on
// later lines and are kept in the audit's raw text.
var linearDecisionCommentRE = regexp.MustCompile(`^(?i:(D-\d+(?:-\d+)?)\s+)?(?:(?:ver=)?([0-9a-f]{8})\s+)?(\d|[A-Fa-f])(?:\s+ver=(\S+))?$`)

// ParseLinearDecisionComment reads an answer from a comment body. ok is false
// for an ordinary comment, which the flow leaves alone.
func ParseLinearDecisionComment(body string) (LinearDecisionComment, bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	m := linearDecisionCommentRE.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return LinearDecisionComment{}, false
	}
	ver := m[2]
	if ver == "" {
		ver = m[4]
	}
	return LinearDecisionComment{Ref: m[1], Ver: ver, Choice: m[3]}, true
}

// decisionOptionForChoice resolves a choice, a 1-based number or an option key,
// against the options as asked.
func decisionOptionForChoice(options *DecisionOptions, choice string) (DecisionOption, int, bool) {
	if options == nil {
		return DecisionOption{}, 0, false
	}
	if n, err := strconv.Atoi(choice); err == nil {
		if n < 1 || n > len(options.Options) {
			return DecisionOption{}, 0, false
		}
		return options.Options[n-1], n, true
	}
	for i, option := range options.Options {
		if strings.EqualFold(option.Key, choice) {
			return option, i + 1, true
		}
	}
	return DecisionOption{}, 0, false
}

// LinearDecisionAnswer is one candidate answer the webhook authenticated.
type LinearDecisionAnswer struct {
	IssueID     string
	Source      string
	UserID      string
	CommentID   string
	DeliveryID  string
	EventAt     string
	RawText     string
	Ref         string
	Ver         string
	Choice      string
	IssueClosed bool
	// StateOptions lists the option numbers that have a mapped state. It is set
	// on a state answer, which counts only if every option of the request has
	// one: a move must not pick option 1 out of a list the mapping never covered.
	StateOptions map[int]bool
}

const (
	LinearDecisionRecorded        = "recorded"
	LinearDecisionDuplicate       = "duplicate"
	LinearDecisionAlreadyResolved = "already_resolved"
	LinearDecisionSuperseded      = "superseded"
	LinearDecisionRejected        = "rejected"
	LinearDecisionNotLinked       = "ignored"
)

// linearDecisionVerdict is what one answer does to one request.
type linearDecisionVerdict struct {
	outcome string
	option  DecisionOption
	number  int
	reply   string
}

func sameLinearAnswer(audit *DecisionLinearAudit, in LinearDecisionAnswer) bool {
	if audit == nil || audit.Source != in.Source {
		return false
	}
	if in.Source == LinearDecisionSourceComment {
		return audit.CommentID == in.CommentID
	}
	return audit.IssueID == in.IssueID && audit.EventAt == in.EventAt
}

func resolvedSummary(request DecisionRequest, options *DecisionOptions) string {
	if request.Resolution == nil {
		return request.Status
	}
	if request.Resolution.Kind != DecisionRequestAnswered {
		return request.Resolution.Kind
	}
	if option, number, ok := decisionOptionForChoice(options, request.Resolution.Option); ok {
		return fmt.Sprintf("%d (%s: %s)", number, option.Key, option.Label)
	}
	return request.Status
}

// decideLinearAnswer is the whole rule for one answer, with no I/O. A reply is
// set only where the operator would otherwise not know the answer went nowhere.
// A state move on a superseded issue stays silent: closing it is hk's own write.
func decideLinearAnswer(link LinearDecisionLink, taskState string, request *DecisionRequest, options *DecisionOptions, in LinearDecisionAnswer) linearDecisionVerdict {
	ref := DecisionRef(link.RequestID)
	isComment := in.Source == LinearDecisionSourceComment
	if link.SupersededBy != "" || request == nil || request.ID != link.RequestID ||
		options == nil || DecisionRequestVer(request.Question, *options, request.Doc, request.Refs) != link.Ver {
		v := linearDecisionVerdict{outcome: LinearDecisionSuperseded}
		if isComment {
			v.reply = "⚠️ not recorded: " + ref + " was superseded. Answer on the newer issue."
		}
		return v
	}
	if request.Status != DecisionRequestOpen {
		if request.Resolution != nil && sameLinearAnswer(request.Resolution.Linear, in) {
			return linearDecisionVerdict{outcome: LinearDecisionDuplicate}
		}
		return linearDecisionVerdict{outcome: LinearDecisionAlreadyResolved, reply: "already resolved: " + ref + " = " + resolvedSummary(*request, options)}
	}
	reject := func(reason string) linearDecisionVerdict {
		return linearDecisionVerdict{outcome: LinearDecisionRejected, reply: "⚠️ not recorded: " + reason}
	}
	if in.Ref != "" && !strings.EqualFold(in.Ref, ref) {
		return reject("this issue is " + ref + ", not " + in.Ref + ".")
	}
	if in.Ver != "" && in.Ver != link.Ver {
		return reject("ver " + in.Ver + " does not match ver=" + link.Ver + ".")
	}
	if isTerminalTaskState(taskState) {
		return reject("the task is already " + taskState + ".")
	}
	if isComment && in.IssueClosed {
		return reject("this issue is closed. Reopen it to answer.")
	}
	option, number, ok := decisionOptionForChoice(options, in.Choice)
	if !ok {
		return reject(in.Choice + " is not one of the options.")
	}
	if !isComment {
		for n := 1; n <= len(options.Options); n++ {
			if !in.StateOptions[n] {
				return reject("not every option has a state. Answer with a comment.")
			}
		}
	}
	return linearDecisionVerdict{outcome: LinearDecisionRecorded, option: option, number: number, reply: fmt.Sprintf("✅ recorded: %s = %d (%s: %s), ver=%s", ref, number, option.Key, option.Label, link.Ver)}
}

// linearAnswerReplyKey allows one reply per event and outcome. A repeated delivery
// with the same outcome queues nothing, but one whose outcome changed, say refused
// on a closed issue and then recorded after a reopen, still gets its own reply.
func linearAnswerReplyKey(in LinearDecisionAnswer, outcome string) string {
	if in.Source == LinearDecisionSourceComment {
		return "reply:comment:" + in.CommentID + ":" + outcome
	}
	return "reply:state:" + in.IssueID + ":" + in.EventAt + ":" + outcome
}

// firstBytes cuts s to at most max bytes on a rune boundary.
func firstBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

const linearDecisionLinkColumns = `request_id,task_id,ver,issue_id,identifier,url,superseded_by,created_at,updated_at`

func scanLinearDecisionLink(row pgx.Row, x *LinearDecisionLink) error {
	return row.Scan(&x.RequestID, &x.TaskID, &x.Ver, &x.IssueID, &x.Identifier, &x.URL, &x.SupersededBy, &x.CreatedAt, &x.UpdatedAt)
}

func (s *Store) linearDecisionLink(ctx context.Context, where string, arg string) (LinearDecisionLink, bool, error) {
	var x LinearDecisionLink
	err := scanLinearDecisionLink(s.pool.QueryRow(ctx, `SELECT `+linearDecisionLinkColumns+` FROM linear_decision_issues WHERE `+where+`=$1`, arg), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return LinearDecisionLink{}, false, nil
	}
	if err != nil {
		return LinearDecisionLink{}, false, err
	}
	return x, true, nil
}

// LinearDecisionLinkByRequest reads the issue bound to a request.
func (s *Store) LinearDecisionLinkByRequest(ctx context.Context, requestID string) (LinearDecisionLink, bool, error) {
	return s.linearDecisionLink(ctx, "request_id", requestID)
}

// LinearDecisionLinkByIssue reads the request bound to a Linear issue.
func (s *Store) LinearDecisionLinkByIssue(ctx context.Context, issueID string) (LinearDecisionLink, bool, error) {
	return s.linearDecisionLink(ctx, "issue_id", issueID)
}

func insertLinearDecisionOp(ctx context.Context, tx pgx.Tx, key, op, issueID, body, stateID string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO linear_decision_ops(dedupe_key,op,issue_id,body,state_id,next_attempt_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6,$6) ON CONFLICT(dedupe_key) DO NOTHING`, key, op, issueID, body, stateID, at)
	return err
}

// RecordLinearDecisionIssue binds a request to its new issue. When the request
// supersedes another with an issue, the old link is marked and the comment and
// close it is owed are queued in the same transaction. announce is the channel
// message for the new issue. created is false when the request already had an
// issue, which is returned unchanged.
func (s *Store) RecordLinearDecisionIssue(ctx context.Context, link LinearDecisionLink, supersedes, closeStateID, announce string) (LinearDecisionLink, bool, error) {
	now := time.Now().UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LinearDecisionLink{}, false, err
	}
	defer tx.Rollback(ctx)
	// The insert's foreign key check locks the task row and the supersede below
	// locks the old link, the reverse of AnswerLinearDecision. This lock, taken
	// first by every decision writer, keeps the two from deadlocking.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return LinearDecisionLink{}, false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO linear_decision_issues(request_id,task_id,ver,issue_id,identifier,url,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$7) ON CONFLICT(request_id) DO NOTHING`, link.RequestID, link.TaskID, link.Ver, link.IssueID, link.Identifier, link.URL, now)
	if err != nil {
		return LinearDecisionLink{}, false, err
	}
	created := tag.RowsAffected() == 1
	if created {
		if err := insertLinearDecisionOp(ctx, tx, "discord:asked:"+link.RequestID, LinearDecisionOpDiscord, link.IssueID, announce, "", now); err != nil {
			return LinearDecisionLink{}, false, err
		}
	}
	if created && supersedes != "" {
		var oldIssue string
		err := tx.QueryRow(ctx, `UPDATE linear_decision_issues SET superseded_by=$2,updated_at=$3 WHERE request_id=$1 AND superseded_by='' RETURNING issue_id`, supersedes, link.RequestID, now).Scan(&oldIssue)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return LinearDecisionLink{}, false, err
		default:
			body := "Superseded by " + link.Identifier + " (" + DecisionRef(link.RequestID) + ", ver=" + link.Ver + "). Answer there: " + link.URL
			if err := insertLinearDecisionOp(ctx, tx, "superseded:comment:"+supersedes, LinearDecisionOpComment, oldIssue, body, "", now); err != nil {
				return LinearDecisionLink{}, false, err
			}
			if err := insertLinearDecisionOp(ctx, tx, "superseded:close:"+supersedes, LinearDecisionOpClose, oldIssue, "", closeStateID, now); err != nil {
				return LinearDecisionLink{}, false, err
			}
			channel := "♻️ " + DecisionRef(supersedes) + " superseded by " + DecisionRef(link.RequestID) + ": " + link.URL
			if err := insertLinearDecisionOp(ctx, tx, "discord:superseded:"+supersedes, LinearDecisionOpDiscord, oldIssue, channel, "", now); err != nil {
				return LinearDecisionLink{}, false, err
			}
		}
	}
	if created {
		if err := mirrorLinearDecisionTx(ctx, tx, link, now); err != nil {
			return LinearDecisionLink{}, false, err
		}
	}
	var out LinearDecisionLink
	if err := scanLinearDecisionLink(tx.QueryRow(ctx, `SELECT `+linearDecisionLinkColumns+` FROM linear_decision_issues WHERE request_id=$1`, link.RequestID), &out); err != nil {
		return LinearDecisionLink{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LinearDecisionLink{}, false, err
	}
	return out, created, nil
}

// queueDecisionClosedPost tells the decisions channel a request closed, whichever
// path closed it, so the channel never shows an answered question as open. A
// request with no Linear issue was never posted and queues nothing.
func queueDecisionClosedPost(ctx context.Context, tx pgx.Tx, request DecisionRequest, options *DecisionOptions, at time.Time) error {
	var link LinearDecisionLink
	err := scanLinearDecisionLink(tx.QueryRow(ctx, `SELECT `+linearDecisionLinkColumns+` FROM linear_decision_issues WHERE request_id=$1`, request.ID), &link)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	mark := "✅"
	if request.Status != DecisionRequestAnswered {
		mark = "⏹️"
	}
	body := mark + " " + DecisionRef(request.ID) + " = " + resolvedSummary(request, options) + ", ver=" + link.Ver + ": " + link.URL
	return insertLinearDecisionOp(ctx, tx, "discord:resolved:"+request.ID, LinearDecisionOpDiscord, link.IssueID, body, "", at)
}

func linearDecisionAnsweredText(link LinearDecisionLink, v linearDecisionVerdict) string {
	return dotsLaneText(fmt.Sprintf("[decision] %s answered %d (%s: %s) by operator on Linear, ver=%s, task #%d · %s %s", DecisionRef(link.RequestID), v.number, v.option.Key, v.option.Label, link.Ver, link.TaskID, link.Identifier, link.URL))
}

// AnswerLinearDecision applies one authenticated answer to the issue's request.
// The first valid answer wins. It resolves the request, queues the desk
// notification and queues the reply in one transaction; every other outcome
// changes the request not at all. A repeated delivery with the same outcome queues
// no second reply. Outcome LinearDecisionNotLinked
// means the issue carries no request.
func (s *Store) AnswerLinearDecision(ctx context.Context, in LinearDecisionAnswer) (string, error) {
	if in.IssueID == "" || in.UserID == "" || (in.Source != LinearDecisionSourceComment && in.Source != LinearDecisionSourceState) ||
		(in.Source == LinearDecisionSourceComment && in.CommentID == "") ||
		// EventAt identifies a state event: without it, every later move on the
		// issue would read as a repeat of the first.
		(in.Source == LinearDecisionSourceState && in.EventAt == "") {
		return "", errors.New("invalid linear decision answer")
	}
	raw := firstBytes(in.RawText, DecisionOperatorQuoteMaxBytes)
	if err := guard.Reject(raw); err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return "", err
	}
	var link LinearDecisionLink
	if err = scanLinearDecisionLink(tx.QueryRow(ctx, `SELECT `+linearDecisionLinkColumns+` FROM linear_decision_issues WHERE issue_id=$1 FOR UPDATE`, in.IssueID), &link); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LinearDecisionNotLinked, nil
		}
		return "", err
	}
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, link.TaskID), &x); err != nil {
		return "", err
	}
	decisionBefore := decisionBeforeOf(x)
	v := decideLinearAnswer(link, x.State, x.Refs.DecisionRequest, x.Refs.DecisionOptions, in)
	now := time.Now().UTC()
	if v.outcome == LinearDecisionRecorded {
		// A webhook holds no wrk receipt, so a task under an activated wrk job
		// refuses this write like any other receiptless one. Say so on the issue:
		// Linear cannot act on the refusal, and the operator would see nothing.
		if err := s.wrkTaskFenceTx(ctx, tx, x, "claimed", false); err != nil {
			var refused wrkrecords.Error
			if !errors.As(err, &refused) {
				return "", err
			}
			v = linearDecisionVerdict{outcome: LinearDecisionRejected, reply: "⚠️ not recorded: the task is under a protected job (" + string(refused) + "). Answer in the console."}
		}
	}
	if v.outcome == LinearDecisionRecorded {
		resolved := *x.Refs.DecisionRequest
		resolved.Status = DecisionRequestAnswered
		resolved.Resolution = &DecisionResolution{
			Kind: DecisionRequestAnswered, Option: v.option.Key, Responder: linearDecisionResponder, By: linearDecisionRecorder, At: now,
			Linear: &DecisionLinearAudit{
				DecisionID: DecisionRef(link.RequestID), Ver: link.Ver, Choice: in.Choice, RawText: raw, UserID: in.UserID,
				Source: in.Source, CommentID: in.CommentID, IssueID: in.IssueID, DeliveryID: in.DeliveryID, EventAt: in.EventAt,
			},
		}
		x.Refs.DecisionRequest = &resolved
		encoded, err := json.Marshal(x.Refs)
		if err != nil {
			return "", err
		}
		if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET refs=$2::jsonb,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, x.ID, string(encoded), now), &x); err != nil {
			return "", err
		}
		if err = insertNotificationOutbox(ctx, tx, OutboxKindDecisionAnswered, LinearDecisionLane, decisionAnsweredEventID(resolved.ID), linearDecisionAnsweredText(link, v), now); err != nil {
			return "", err
		}
		note := "decision-answered " + resolved.ID + " " + v.option.Key + " (operator on Linear " + link.Identifier + ", ver=" + link.Ver + ")"
		if err = insertDecisionEvent(ctx, tx, x.ID, x.State, linearDecisionRecorder, note, x.Refs, now); err != nil {
			return "", err
		}
		if err = mirrorTaskDecisionsTx(ctx, tx, decisionBefore, x, linearDecisionRecorder, note, 0, now); err != nil {
			return "", err
		}
		if err = queueDecisionClosedPost(ctx, tx, resolved, x.Refs.DecisionOptions, now); err != nil {
			return "", err
		}
	}
	if v.reply != "" {
		if err = insertLinearDecisionOp(ctx, tx, linearAnswerReplyKey(in, v.outcome), LinearDecisionOpComment, in.IssueID, v.reply, "", now); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return v.outcome, nil
}

const linearDecisionOpColumns = `id,dedupe_key,op,issue_id,body,state_id,state,attempts,last_error`

func scanLinearDecisionOp(row pgx.Row, x *LinearDecisionOp) error {
	return row.Scan(&x.ID, &x.DedupeKey, &x.Op, &x.IssueID, &x.Body, &x.StateID, &x.State, &x.Attempts, &x.LastError)
}

// ProcessNextLinearDecisionOp runs one due op through process and records the
// result. A failure retries with exponential backoff until the attempts run out,
// then the row stays as failed. found is false when nothing is due. The row lock
// is held through process, as in ProcessNextLinearOutbox.
func (s *Store) ProcessNextLinearDecisionOp(ctx context.Context, process func(context.Context, LinearDecisionOp) error) (found bool, opErr error, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback(ctx)
	var op LinearDecisionOp
	err = scanLinearDecisionOp(tx.QueryRow(ctx, `SELECT `+linearDecisionOpColumns+` FROM linear_decision_ops WHERE state='pending' AND next_attempt_at<=now() ORDER BY next_attempt_at ASC,id ASC FOR UPDATE SKIP LOCKED LIMIT 1`), &op)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	opErr = process(ctx, op)
	now := time.Now().UTC()
	attempts := op.Attempts + 1
	state, lastError, next := "sent", "", now
	if errors.Is(opErr, ErrLinearDecisionOpSkipped) {
		state, opErr = "skipped", nil
	}
	if opErr != nil {
		lastError = firstBytes(opErr.Error(), linearDecisionErrorMaxBytes)
		state = "pending"
		next = now.Add(linearDecisionOpBackoff(op.Attempts))
		if attempts >= linearDecisionOpMaxAttempts {
			state = "failed"
			opErr = fmt.Errorf("%w: %s %s: %w", ErrLinearDecisionOpGaveUp, op.Op, op.DedupeKey, opErr)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE linear_decision_ops SET state=$2,attempts=$3,next_attempt_at=$4,last_error=$5,updated_at=$6 WHERE id=$1`, op.ID, state, attempts, next, lastError, now); err != nil {
		return true, opErr, err
	}
	return true, opErr, tx.Commit(ctx)
}

// ListLinearDecisionOps reads the ops for one issue in queue order.
func (s *Store) ListLinearDecisionOps(ctx context.Context, issueID string) ([]LinearDecisionOp, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+linearDecisionOpColumns+` FROM linear_decision_ops WHERE issue_id=$1 ORDER BY id ASC`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LinearDecisionOp{}
	for rows.Next() {
		var x LinearDecisionOp
		if err := scanLinearDecisionOp(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

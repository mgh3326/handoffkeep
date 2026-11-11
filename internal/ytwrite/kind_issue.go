package ytwrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The issue_create kind (hk 3898, docs/youtrack-write-protocol.md "Kind
// issue_create"): hk creates a task's YouTrack issue itself, at most once per
// task (key IssueKey), or adopts the one issue that already carries the task's
// hk ID. The op's lane is the task ("task", decimal id) and its target
// (YTTargetID) is the YouTrack project the issue is created in. When it is
// applied, the store publishes the task's binding in the same transaction.

func init() {
	registerKind(KindIssueCreate, func(Config) Kind { return issueCreate{} })
}

// IssueSummaryLimit is the most runes an issue summary carries.
const IssueSummaryLimit = 250

// IssueCreatePayload is the immutable outbound payload of an issue_create op.
// The task id is the op's EntityID and the YouTrack project id its
// YTTargetID.
type IssueCreatePayload struct {
	Title string `json:"title"`
	// Project is the task's hk project; YTProjectShort the short name of the
	// YouTrack project it maps to (the search scope).
	Project        string `json:"project"`
	YTProjectShort string `json:"yt_project_short"`
}

// IssueKey is the idempotency key of a task's issue_create op: one create per
// task.
func IssueKey(taskID int64) string { return "task-issue:" + strconv.FormatInt(taskID, 10) }

// IssueRequest is one task whose issue hk creates.
type IssueRequest struct {
	Principal string
	// Actor is who the provenance line names (the task's creator).
	Actor  string
	TaskID int64
	Title  string
	// Project is the hk project; YTProjectID and YTProjectShort its
	// youtrack_projects row; Tenant the YouTrack host.
	Project        string
	YTProjectID    string
	YTProjectShort string
	Tenant         string
}

// CheckIssue explains why an issue request cannot be sent. A store that must
// record such a task builds the op with IssueOp and rejects it in the same
// transaction.
func CheckIssue(req IssueRequest) error {
	if !principalRE.MatchString(req.Principal) {
		return fmt.Errorf("%w: principal is empty or malformed", ErrInvalid)
	}
	if err := checkActor(req.Actor); err != nil {
		return err
	}
	if req.TaskID <= 0 {
		return fmt.Errorf("%w: task id must be positive", ErrInvalid)
	}
	if !issueIDRE.MatchString(req.YTProjectID) || !issueIDRE.MatchString(req.YTProjectShort) {
		return fmt.Errorf("%w: youtrack project id or short name is empty or malformed", ErrInvalid)
	}
	if req.Tenant == "" {
		return fmt.Errorf("%w: tenant is empty", ErrInvalid)
	}
	if err := checkLine("project", req.Project); err != nil {
		return err
	}
	if err := checkBody(req.Title); err != nil {
		return fmt.Errorf("title: %w", err)
	}
	return nil
}

// checkLine refuses a value that must stay on its provenance line.
func checkLine(name, v string) error {
	if !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) || strings.Contains(v, markerPrefix) {
		return fmt.Errorf("%w: %s must be one line of text without the marker", ErrInvalid, name)
	}
	return nil
}

// IssueOp is the op an issue request stores. Like CommentOp it does not
// validate the request.
func IssueOp(req IssueRequest) (NewOp, error) {
	payload, err := json.Marshal(IssueCreatePayload{Title: req.Title, Project: req.Project, YTProjectShort: req.YTProjectShort})
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.TaskID, 10)
	return NewOp{
		Principal: req.Principal,
		Kind:      KindIssueCreate,
		Key:       IssueKey(req.TaskID),
		// The body slot carries the whole payload: the title, the hk project
		// and the short name are what the write means.
		PayloadSHA: payloadSHA(KindIssueCreate, "task", entityID, req.YTProjectID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload,
		EntityKind: "task",
		EntityID:   entityID,
		YTTargetID: req.YTProjectID,
		Tenant:     req.Tenant,
		Actor:      req.Actor,
	}, nil
}

// IssueText is what an issue hk creates says about where it came from: the
// provenance (actor, time, op), the task and its hk project, and the task's
// title as it was. RenderIssue writes it; ParseIssue reads it back.
type IssueText struct {
	Actor   string
	At      time.Time
	OpID    int64
	TaskID  int64
	Project string
	Title   string
}

const (
	issueTaskPrefix  = "hk task: #"
	issueTitlePrefix = "제목(원문): "
)

// RenderIssue is the summary and description of the issue for x. The summary
// is "#<task> <title>" on one line, cut to IssueSummaryLimit runes; the
// description is
//
//	작성: <actor> · <UTC> · hk op <op>
//	hk task: #<task> · project: <hk project>
//	제목(원문): <title>
//
//	<!-- hk-op:<op> -->
func RenderIssue(x IssueText) (summary, description string) {
	summary = truncateRunes("#"+strconv.FormatInt(x.TaskID, 10)+" "+strings.Join(strings.Fields(x.Title), " "), IssueSummaryLimit)
	description = ProvenanceLine(x.Actor, x.At, x.OpID) + "\n" +
		issueTaskPrefix + strconv.FormatInt(x.TaskID, 10) + " · project: " + x.Project + "\n" +
		issueTitlePrefix + x.Title + "\n\n" + OpMarker(x.OpID)
	return summary, description
}

// ParseIssue reads a description in exactly the form RenderIssue writes; ok
// is false for anything else. The time is read at second precision, as it is
// written.
func ParseIssue(description string) (x IssueText, ok bool) {
	prov, rest, found := strings.Cut(description, "\n")
	if !found {
		return IssueText{}, false
	}
	actor, at, opID, ok := ParseProvenance(prov)
	if !ok {
		return IssueText{}, false
	}
	taskLine, rest, found := strings.Cut(rest, "\n")
	if !found {
		return IssueText{}, false
	}
	idText, project, found := strings.Cut(strings.TrimPrefix(taskLine, issueTaskPrefix), " · project: ")
	if !found || !strings.HasPrefix(taskLine, issueTaskPrefix) {
		return IssueText{}, false
	}
	taskID, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || taskID <= 0 || strconv.FormatInt(taskID, 10) != idText {
		return IssueText{}, false
	}
	body, found := strings.CutSuffix(rest, "\n\n"+OpMarker(opID))
	if !found || !strings.HasPrefix(body, issueTitlePrefix) {
		return IssueText{}, false
	}
	return IssueText{Actor: actor, At: at, OpID: opID, TaskID: taskID, Project: project, Title: strings.TrimPrefix(body, issueTitlePrefix)}, true
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// issueCreate is the issue_create kind. Its text (Render) is the description,
// which carries the marker; Send derives the summary from the same op.
type issueCreate struct{}

func issueTextOf(op Op) (IssueText, IssueCreatePayload, error) {
	var p IssueCreatePayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return IssueText{}, p, fmt.Errorf("%w: payload of op %d: %v", ErrInvalid, op.ID, err)
	}
	taskID, err := strconv.ParseInt(op.EntityID, 10, 64)
	if err != nil || taskID <= 0 || op.EntityKind != "task" {
		return IssueText{}, p, fmt.Errorf("%w: op %d is not on a task lane", ErrInvalid, op.ID)
	}
	return IssueText{Actor: op.Actor, At: op.CreatedAt.UTC().Truncate(time.Second), OpID: op.ID, TaskID: taskID, Project: p.Project, Title: p.Title}, p, nil
}

func (issueCreate) Render(op Op) (string, error) {
	x, _, err := issueTextOf(op)
	if err != nil {
		return "", err
	}
	_, description := RenderIssue(x)
	return description, nil
}

// Send checks the project's fields, then looks for the task's issue and
// writes only when there is none:
// exactly one issue with the task's hk ID whose summary names the task is
// adopted (no POST); any other hit is ambiguous_issue (no POST); no hit is
// exactly one create with the summary, the description and only the hk ID
// and hk project fields.
func (issueCreate) Send(ctx context.Context, remote Remote, op Op, text string) (string, error) {
	ir, ok := remote.(IssueRemote)
	if !ok {
		return "", &RefusedError{Reason: ReasonUnsupportedKind, Detail: "the remote cannot create issues"}
	}
	x, p, err := issueTextOf(op)
	if err != nil {
		return "", &RefusedError{Reason: ReasonInvalidIssue, Detail: err.Error()}
	}
	// A project without the fields the search and the create use would
	// refuse with a 4xx and reject the op for good: hold it instead until
	// the schema is repaired.
	fields, err := ir.CheckIssueFields(ctx, op.YTTargetID, p.Project)
	if err != nil {
		return "", &NotSentError{Err: err}
	}
	hits, err := ir.FindTaskIssues(ctx, p.YTProjectShort, op.YTTargetID, x.TaskID)
	if err != nil {
		return "", &NotSentError{Err: err}
	}
	switch {
	case len(hits) == 1 && SummaryNamesTask(hits[0].Summary, x.TaskID):
		return "", &AdoptedError{ID: hits[0].ID}
	case len(hits) > 0:
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		return "", &RefusedError{Reason: ReasonAmbiguousIssue, Detail: fmt.Sprintf("project %s has %d issues with hk ID %d and none can be adopted: %s", op.YTTargetID, len(hits), x.TaskID, strings.Join(ids, ","))}
	}
	summary, _ := RenderIssue(x)
	return ir.CreateIssue(ctx, IssueCreate{ProjectID: op.YTTargetID, Summary: summary, Description: text, TaskID: x.TaskID, HKProject: p.Project, HKProjectText: fields.ProjectText})
}

// Evidence lists the project's issues with the task's hk ID. Exactly one,
// whose description is exactly the rendered text (provenance, task, title and
// marker), is the match; a quoted or copied marker in other text is not
// ownership. Every other issue with that hk ID counts as a mismatch, so the
// shared rule keeps the op remote_unknown unless that one issue is all there
// is.
func (issueCreate) Evidence(ctx context.Context, remote Remote, op Op, text string) (Evidence, error) {
	var ev Evidence
	ir, ok := remote.(IssueRemote)
	if !ok {
		return ev, errors.New("the remote cannot list issues")
	}
	x, p, err := issueTextOf(op)
	if err != nil {
		return ev, err
	}
	hits, err := ir.FindTaskIssues(ctx, p.YTProjectShort, op.YTTargetID, x.TaskID)
	if err != nil {
		var re *RemoteError
		if errors.As(err, &re) && re.Status == http.StatusTooManyRequests {
			ev.RetryAfter = re.RetryAfter
		}
		return ev, err
	}
	for _, h := range hits {
		if h.Description == text {
			ev.Matches = append(ev.Matches, h.ID)
		} else {
			ev.Mismatched++
		}
	}
	return ev, nil
}

func (issueCreate) needsProvenance() bool { return true }

// MapRows maps the task to its issue, with the YouTrack project as parent, as
// the comment path anchors a task's map row.
func (issueCreate) MapRows(op Op, ytID string) []MapRow {
	return []MapRow{{EntityKind: "task", HKKey: op.EntityID, YTID: ytID, YTParentID: op.YTTargetID, OpID: op.ID}}
}

// SummaryNamesTask reports whether an issue summary names the task: "#<id>"
// then a space or the end. "#37" does not name task 3, and "#037" or "#3739"
// do not name task 37.
func SummaryNamesTask(summary string, taskID int64) bool {
	rest, ok := strings.CutPrefix(summary, "#"+strconv.FormatInt(taskID, 10))
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsSpace(r)
}

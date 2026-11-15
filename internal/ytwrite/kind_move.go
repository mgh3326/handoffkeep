package ytwrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// The issue_move kind (hk 4055, docs/youtrack-write-protocol.md "Kind
// issue_move"): a task's hk project changed, so its bound issue moves to the
// YouTrack project that hk project maps to. The store's task_fields consumer
// prepares one op per move (prepareYTSnapshotTx) and the task's snapshot right
// after it, on the task's lane (task:<id>), so the snapshot is sent once the
// issue is in its project. The op's target (YTTargetID) is the bound issue; its
// payload is the project it moves to. Its Send is one POST and its Evidence is
// the issue's project, so a lost reply is settled by where the issue is now.

func init() {
	registerKind(KindIssueMove, func(Config) Kind { return issueMove{} })
}

// KindIssueMove moves a task's bound issue to another YouTrack project.
const KindIssueMove = "issue_move"

// ReasonInvalidMove: the op's payload or target is not a move (never sent).
const ReasonInvalidMove = "invalid_move"

// IssueMovePayload is the immutable payload of an issue_move op: the YouTrack
// project the issue moves to.
type IssueMovePayload struct {
	ProjectID string `json:"project_id"`
}

// MoveRequest is one move of a task's bound issue to the YouTrack project of
// its hk project.
type MoveRequest struct {
	Principal string
	Actor     string
	// Tenant and IssueID are the task's binding; ProjectID is the target.
	Tenant    string
	IssueID   string
	ProjectID string
	TaskID    int64
}

// CheckMove explains why a move request cannot be stored.
func CheckMove(req MoveRequest) error {
	if !principalRE.MatchString(req.Principal) {
		return fmt.Errorf("%w: principal is empty or malformed", ErrInvalid)
	}
	if err := checkActor(req.Actor); err != nil {
		return err
	}
	if req.TaskID <= 0 {
		return fmt.Errorf("%w: task id must be positive", ErrInvalid)
	}
	if !issueIDRE.MatchString(req.IssueID) || !issueIDRE.MatchString(req.ProjectID) {
		return fmt.Errorf("%w: youtrack issue or project id is empty or malformed", ErrInvalid)
	}
	if req.Tenant == "" {
		return fmt.Errorf("%w: tenant is empty", ErrInvalid)
	}
	return nil
}

// MoveOp is the op a move request stores, with no Key: the store's snapshot
// helper sets it (SnapshotKey) from PayloadSHA, which covers the target
// project, the issue and the tenant.
func MoveOp(req MoveRequest) (NewOp, error) {
	if err := CheckMove(req); err != nil {
		return NewOp{}, err
	}
	payload, err := json.Marshal(IssueMovePayload{ProjectID: req.ProjectID})
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.TaskID, 10)
	return NewOp{
		Principal:  req.Principal,
		Kind:       KindIssueMove,
		PayloadSHA: payloadSHA(KindIssueMove, "task", entityID, req.IssueID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload,
		EntityKind: "task",
		EntityID:   entityID,
		YTTargetID: req.IssueID,
		Tenant:     req.Tenant,
		Actor:      req.Actor,
	}, nil
}

// IssueMoveProject is the project an issue_move op moves its issue to. It is
// ErrInvalid for an op that is not a move.
func IssueMoveProject(op Op) (string, error) {
	if op.EntityKind != "task" || !issueIDRE.MatchString(op.YTTargetID) {
		return "", fmt.Errorf("%w: op %d is not a task issue move", ErrInvalid, op.ID)
	}
	var p IssueMovePayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return "", fmt.Errorf("%w: payload of op %d: %v", ErrInvalid, op.ID, err)
	}
	if !issueIDRE.MatchString(p.ProjectID) {
		return "", fmt.Errorf("%w: op %d moves to no project", ErrInvalid, op.ID)
	}
	return p.ProjectID, nil
}

// IssueMoveRemote is the YouTrack surface of the issue_move kind. A Remote
// that does not implement it cannot send that kind.
type IssueMoveRemote interface {
	// MoveIssueToProject sets the issue's project in exactly one POST and
	// returns the issue's id from the reply. An empty id (a 2xx without one,
	// or a reply that does not name projectID) is uncertain. Its errors
	// follow CreateComment's.
	MoveIssueToProject(ctx context.Context, issueID, projectID string) (id string, err error)
	// IssueProjectID reads the id of the project the issue is in now. A 429
	// is a *RemoteError with the server's RetryAfter.
	IssueProjectID(ctx context.Context, issueID string) (projectID string, err error)
}

// issueMove is the issue_move kind. It has no text: Render is empty, and the
// move itself is the request.
type issueMove struct{}

func (issueMove) Render(Op) (string, error) { return "", nil }

func (issueMove) Send(ctx context.Context, remote Remote, op Op, _ string) (string, error) {
	mr, ok := remote.(IssueMoveRemote)
	if !ok {
		return "", &RefusedError{Reason: ReasonUnsupportedKind, Detail: "the remote cannot move issues"}
	}
	project, err := IssueMoveProject(op)
	if err != nil {
		return "", &RefusedError{Reason: ReasonInvalidMove, Detail: err.Error()}
	}
	return mr.MoveIssueToProject(ctx, op.YTTargetID, project)
}

// Evidence reads the issue's project: when it is the op's target the move
// happened, which is the one match; any other project is no evidence, so the
// op stays remote_unknown for the operator.
func (issueMove) Evidence(ctx context.Context, remote Remote, op Op, _ string) (Evidence, error) {
	var ev Evidence
	mr, ok := remote.(IssueMoveRemote)
	if !ok {
		return ev, errors.New("the remote cannot read issue projects")
	}
	project, err := IssueMoveProject(op)
	if err != nil {
		return ev, err
	}
	got, err := mr.IssueProjectID(ctx, op.YTTargetID)
	if err != nil {
		var re *RemoteError
		if errors.As(err, &re) && re.Status == http.StatusTooManyRequests {
			ev.RetryAfter = re.RetryAfter
		}
		return ev, err
	}
	if got == project {
		ev.Matches = []string{op.YTTargetID}
	}
	return ev, nil
}

// MapRows: a move creates nothing to map. The task's map row keeps the project
// it was made with; only the unbind of the binding it anchors deletes it
// (UnbindYTTaskBinding, hk 4536).
func (issueMove) MapRows(Op, string) []MapRow { return nil }

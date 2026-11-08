// Package remote adapts the authenticated HTTP API for local CLI and stdio MCP.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/client"
	"github.com/mgh3326/handoffkeep/internal/hktransport"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// Client is the hk CLI's /v1 client, with the store-typed methods the CLI and
// the stdio MCP call. It has package client's fields, so the calls client
// already makes go through client.Client(c); every other call builds and
// sends its request through hk's module-internal transport,
// hktransport.Client(c), which holds each request to the configured origin.
type Client hktransport.Client

// UserAgent is stamped on every hk request (hktransport.UserAgent):
// Cloudflare Access can be configured to block the default Go user agent
// before a request reaches the service, and a distinct UA keeps hk calls
// identifiable in Access logs.
var UserAgent = hktransport.UserAgent

// CFAccessPairError rejects a half-configured Cloudflare Access service
// token: one key without the other is a config error that names the missing
// key and never prints a value.
func CFAccessPairError(id, secret string) error {
	id, secret = strings.TrimSpace(id), strings.TrimSpace(secret)
	switch {
	case id != "" && secret == "":
		return errors.New("cf_access_config_incomplete: HANDOFFKEEP_CF_ACCESS_CLIENT_ID is set but HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET is missing")
	case id == "" && secret != "":
		return errors.New("cf_access_config_incomplete: HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET is set but HANDOFFKEEP_CF_ACCESS_CLIENT_ID is missing")
	}
	return nil
}

// ErrTooManyRedirects is the transport's own redirect cap
// (hktransport.ErrTooManyRedirects); callers recognize it with errors.Is.
var ErrTooManyRedirects = hktransport.ErrTooManyRedirects

// HTTPError is a response the server actually sent with a non-2xx status
// (hktransport.HTTPError). Any other error from a call — transport failure,
// timeout, an undecodable 2xx body — means the outcome of a write is unknown:
// the server may have committed it.
type HTTPError = hktransport.HTTPError

// ErrorCode returns the server's error code ("not_found", "queue_empty", ...)
// when err is an *HTTPError, and "" otherwise. Match codes with it, never by
// comparing err.Error() text.
func ErrorCode(err error) string { return hktransport.ErrorCode(err) }

// SecretPattern reports the pattern the server named when it refused a write
// as secret_like_content.
func SecretPattern(err error) (string, bool) { return hktransport.SecretPattern(err) }

// call is hktransport.Client.Call, except that a change-feed reader's expired
// cursor (410 {"reset":true,"min_sync_id":N}) surfaces as *store.SyncReset,
// as the store's own feed reports it.
func (c Client) call(ctx context.Context, method, path string, input, output any) error {
	err := hktransport.Client(c).Call(ctx, method, path, input, output)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusGone && he.Reset {
		return &store.SyncReset{MinSyncID: he.MinSyncID}
	}
	return err
}

func esc(x string) string { return url.PathEscape(x) }
func (c Client) Checkpoint(ctx context.Context, _ string, x store.Checkpoint) (store.Checkpoint, error) {
	var out store.Checkpoint
	e := c.call(ctx, "POST", "/v1/checkpoints", x, &out)
	return out, e
}
func (c Client) Recent(ctx context.Context, session, kind string, limit int) ([]store.Checkpoint, error) {
	var out struct {
		Checkpoints []store.Checkpoint `json:"checkpoints"`
	}
	e := c.call(ctx, "GET", "/v1/checkpoints?session="+url.QueryEscape(session)+"&kind="+url.QueryEscape(kind)+fmt.Sprintf("&limit=%d", limit), nil, &out)
	return out.Checkpoints, e
}
func (c Client) PutMemory(ctx context.Context, _ string, x store.Memory) (store.Memory, error) {
	var out store.Memory
	e := c.call(ctx, "PUT", "/v1/memory/"+esc(x.Agent)+"/"+esc(x.Name), x, &out)
	return out, e
}
func (c Client) GetMemory(ctx context.Context, agent, name string) (store.Memory, bool, error) {
	var out store.Memory
	e := c.call(ctx, "GET", "/v1/memory/"+esc(agent)+"/"+esc(name), nil, &out)
	if ErrorCode(e) == "not_found" {
		return out, false, nil
	}
	return out, e == nil, e
}
func (c Client) ListMemory(ctx context.Context, agent string, content bool) ([]store.Memory, error) {
	var out struct {
		Memory []store.Memory `json:"memory"`
	}
	e := c.call(ctx, "GET", "/v1/memory/"+esc(agent), nil, &out)
	if e != nil || !content {
		return out.Memory, e
	}
	for i := range out.Memory {
		item, found, err := c.GetMemory(ctx, agent, out.Memory[i].Name)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("memory disappeared during pull")
		}
		out.Memory[i] = item
	}
	return out.Memory, nil
}
func (c Client) PutDocument(ctx context.Context, _ string, x store.Document) (store.Document, bool, error) {
	var out struct {
		Document store.Document `json:"document"`
		Changed  bool           `json:"changed"`
	}
	e := c.call(ctx, "PUT", "/v1/documents/"+esc(x.Key), x, &out)
	return out.Document, out.Changed, e
}
func (c Client) GetDocument(ctx context.Context, key string) (store.Document, bool, error) {
	var out store.Document
	e := c.call(ctx, "GET", "/v1/documents/"+esc(key), nil, &out)
	if ErrorCode(e) == "not_found" {
		return out, false, nil
	}
	return out, e == nil, e
}
func (c Client) GetDocumentByID(ctx context.Context, id int64) (store.Document, bool, error) {
	var out store.Document
	e := c.call(ctx, "GET", "/v1/documents?id="+fmt.Sprint(id), nil, &out)
	if ErrorCode(e) == "not_found" {
		return out, false, nil
	}
	if e != nil {
		return out, false, e
	}
	// A server older than this CLI ignores ?id= and returns a documents list,
	// which decodes to a zero Document — never report that as found. Any other
	// id mismatch means the server answered a different document.
	if out.ID != id {
		return out, false, fmt.Errorf("server returned document id %d for requested id %d: server may not support ?id= lookup (older than this CLI)", out.ID, id)
	}
	return out, true, nil
}

// ListDocuments lists documents; task above zero keeps only the documents
// that name that task. A server older than the task filter ignores it.
func (c Client) ListDocuments(ctx context.Context, prefix, kind, session string, task int64, limit int) ([]store.Document, error) {
	var out struct {
		Documents []store.Document `json:"documents"`
	}
	p := url.Values{"prefix": {prefix}, "kind": {kind}, "session": {session}, "limit": {fmt.Sprint(limit)}}
	if task > 0 {
		p.Set("task", fmt.Sprint(task))
	}
	e := c.call(ctx, "GET", "/v1/documents?"+p.Encode(), nil, &out)
	if e == nil && task > 0 {
		// A server older than the filter returns every document; keep the
		// ones that name the task so the answer is never wider than asked.
		kept := out.Documents[:0]
		for _, d := range out.Documents {
			if d.TaskID != nil && *d.TaskID == task {
				kept = append(kept, d)
			}
		}
		out.Documents = kept
	}
	return out.Documents, e
}

// searchWireCap is the largest limit the /v1/search API accepts
// (queryLimit def=20 max=100); requesting beyond it is a 400.
const searchWireCap = 100

func (c Client) Search(ctx context.Context, q, scope, session string, limit int) ([]store.SearchResult, error) {
	var out struct {
		Results []store.SearchResult `json:"results"`
	}
	// Request one extra row so a cut page stays detectable against a server
	// that predates per-row truncated markers: len(results) > limit means more
	// rows exist. The API rejects limits above its 100 cap, so the probe only
	// applies strictly below it — at the cap the page cannot be probed.
	// limit < 1 is passed through unchanged.
	req := limit
	if req >= 1 && req < searchWireCap {
		req++
	}
	p := url.Values{"q": {q}, "scope": {scope}, "session": {session}, "limit": {fmt.Sprint(req)}}
	e := c.call(ctx, "GET", "/v1/search?"+p.Encode(), nil, &out)
	if e != nil {
		return nil, e
	}
	xs := out.Results
	if limit >= 1 && len(xs) > limit {
		xs = xs[:limit]
		for i := range xs {
			xs[i].Truncated = true
		}
	}
	return xs, nil
}
func (c Client) PutAttachment(ctx context.Context, _ string, name, mime, ref string, body []byte) (store.Attachment, bool, error) {
	x, created, e := client.Client(c).PutAttachment(ctx, name, mime, ref, body)
	return store.Attachment(x), created, e
}
func (c Client) ListAttachments(ctx context.Context, ref string, limit int) ([]store.Attachment, error) {
	var x struct {
		Attachments []store.Attachment `json:"attachments"`
	}
	e := c.call(ctx, "GET", "/v1/attachments?"+url.Values{"ref": {ref}, "limit": {fmt.Sprint(limit)}}.Encode(), nil, &x)
	return x.Attachments, e
}
func (c Client) AttachmentURL(ctx context.Context, sha string) (string, error) {
	return client.Client(c).AttachmentURL(ctx, sha)
}
func (c Client) GetAttachment(ctx context.Context, sha string) (store.Attachment, io.ReadCloser, error) {
	x, body, e := client.Client(c).GetAttachment(ctx, sha)
	return store.Attachment(x), body, e
}
func (c Client) AttachmentUsage(ctx context.Context) (store.AttachmentUsage, error) {
	var x struct {
		Usage store.AttachmentUsage `json:"usage"`
	}
	e := c.call(ctx, "GET", "/v1/usage", nil, &x)
	return x.Usage, e
}

// createTaskBody is store.Task on the wire with area omitted when unset. The
// server decodes with unknown fields refused, so a new client that always
// sent "area":null would break against a server that predates the column.
// The outer Area shadows the embedded one for encoding/json.
// Link and New answer the duplicate check and are sent only when set, for
// the same reason.
type createTaskBody struct {
	store.Task
	Area *string `json:"area,omitempty"`
	Link int64   `json:"link,omitempty"`
	New  bool    `json:"new,omitempty"`
}

// CreateTask files a task with the duplicate check unanswered: a similar open
// task refuses it with 409 similar_tasks (HTTPError.SimilarTasks).
func (c Client) CreateTask(ctx context.Context, x store.Task) (store.Task, error) {
	return c.CreateTaskChecked(ctx, x, store.DuplicateCheck{})
}

// CreateTaskChecked files a task answering the duplicate check: check.Link
// relates it to an existing task, check.New files it without looking.
func (c Client) CreateTaskChecked(ctx context.Context, x store.Task, check store.DuplicateCheck) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", "/v1/tasks", createTaskBody{Task: x, Area: x.Area, Link: check.Link, New: check.New}, &out)
	if err != nil {
		// The server's code and reason surface unchanged — invalid_context on
		// create is a validation failure (missing lane, unknown kind), not a
		// version-skew verdict.
		return out, err
	}
	// A server that predates the project column could answer 200 while
	// silently dropping the field — same silent-drop defense as job_id.
	if x.Project != nil && (out.Project == nil || *out.Project != *x.Project) {
		return out, fmt.Errorf("create_project_not_recorded: server response has project unset — it likely predates the task project field")
	}
	if x.Area != nil && (out.Area == nil || *out.Area != *x.Area) {
		return out, fmt.Errorf("create_area_not_recorded: server response has area unset — it likely predates the task area field")
	}
	return out, nil
}
func (c Client) ClaimTask(ctx context.Context, id int64, claimedBy, jobID, noJob string) (store.Task, error) {
	var out store.Task
	body := map[string]string{"claimed_by": claimedBy}
	if jobID != "" {
		body["job_id"] = jobID
	}
	if noJob != "" {
		body["no_job"] = noJob
	}
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/claim", id), body, &out)
	if err != nil {
		// Servers whose decoder rejects unknown fields answer the new body
		// with the generic invalid_context rather than naming job_id/no_job.
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" {
			if jobID != "" {
				return out, fmt.Errorf("claim_job_id_rejected: server refused the claim with invalid_context (job_id=%q) — it likely predates job_id claim support", jobID)
			}
			if noJob != "" {
				return out, fmt.Errorf("claim_no_job_rejected: server refused the claim with invalid_context — it likely predates no_job claim support")
			}
		}
		return out, err
	}
	// A server that predates job_id claims can also answer 200 while
	// silently dropping the field. Treat that as failure: the task is
	// claimed but unlinked, and reporting success here would falsify the
	// job link.
	// A same-claimant replay on an already-active row answers with that
	// row's state, so every active state is an honest response. A server that
	// predates the eleven-state model answers claimed or in_progress; a current
	// one answers researching or developing (reviewing on a replay).
	if !activeTaskState(out.State) {
		return out, fmt.Errorf("claim_not_applied: server response has state=%q, want a started state (developing or researching; old servers: claimed)", out.State)
	}
	if out.ClaimedBy != claimedBy {
		return out, fmt.Errorf("claim_claimant_not_recorded: server response has claimed_by=%q, want %q", out.ClaimedBy, claimedBy)
	}
	if jobID != "" && out.Refs.JobID != jobID {
		return out, fmt.Errorf("claim_job_id_not_recorded: server response has refs.job_id=%q, want %q (server predates job_id claims)", out.Refs.JobID, jobID)
	}
	return out, nil
}

// RebindTaskJob changes the job link on an active task during a builder
// takeover. Older servers reject the route, so report that version skew with
// the missing endpoint and the minimum feature version.
func (c Client) RebindTaskJob(ctx context.Context, id int64, fromJob, toJob, by, reason string) (store.Task, error) {
	var out store.Task
	body := map[string]string{"from_job": fromJob, "to_job": toJob, "by": by, "reason": reason}
	path := fmt.Sprintf("/v1/tasks/%d/rebind", id)
	err := c.call(ctx, "POST", path, body, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "" && (he.Status == http.StatusNotFound || he.Status == http.StatusMethodNotAllowed) {
			return out, fmt.Errorf("task_rebind_route_missing: server does not provide POST /v1/tasks/{id}/rebind; minimum server version is a handoffkeep build from PR 1751 or newer")
		}
		return out, err
	}
	if !activeTaskState(out.State) || out.ClaimedBy != by || out.Refs.JobID != toJob {
		return out, fmt.Errorf("task_rebind_not_recorded: server response has state=%q claimed_by=%q refs.job_id=%q; want an active task claimed by %q with job_id=%q", out.State, out.ClaimedBy, out.Refs.JobID, by, toJob)
	}
	return out, nil
}

func (c Client) NextTask(ctx context.Context, lane, claimedBy, jobID, noJob string) (store.Task, error) {
	var out store.Task
	body := map[string]string{"lane": lane, "claimed_by": claimedBy}
	if jobID != "" {
		body["job_id"] = jobID
	}
	if noJob != "" {
		body["no_job"] = noJob
	}
	err := c.call(ctx, "POST", "/v1/tasks/next", body, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" && noJob != "" {
			return out, fmt.Errorf("next_no_job_rejected: server refused the claim with invalid_context — it likely predates no_job support")
		}
		return out, err
	}
	if !activeTaskState(out.State) {
		return out, fmt.Errorf("next_not_applied: server response has state=%q, want a started state (developing or researching; old servers: claimed)", out.State)
	}
	if out.ClaimedBy != claimedBy {
		return out, fmt.Errorf("next_claimant_not_recorded: server response has claimed_by=%q, want %q", out.ClaimedBy, claimedBy)
	}
	if jobID != "" && out.Refs.JobID != jobID {
		return out, fmt.Errorf("next_job_id_not_recorded: server response has refs.job_id=%q, want %q (server predates job_id claims)", out.Refs.JobID, jobID)
	}
	return out, nil
}
func (c Client) TransitionTask(ctx context.Context, id int64, to, note string, refs *store.TaskRefs, noJob string) (store.Task, error) {
	return c.TransitionTaskWith(ctx, id, store.TransitionInput{To: to, Note: note, Refs: refs, NoJob: noJob})
}

// TransitionTaskWith is TransitionTask with the options a plain call cannot
// carry: DuplicateOf names the task a duplicate transition points at.
func (c Client) TransitionTaskWith(ctx context.Context, id int64, in store.TransitionInput) (store.Task, error) {
	to, note, refs, noJob := in.To, in.Note, in.Refs, in.NoJob
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/transition", id), struct {
		To          string          `json:"to"`
		Closure     string          `json:"closure,omitempty"`
		Evidence    string          `json:"evidence,omitempty"`
		Note        string          `json:"note,omitempty"`
		Refs        *store.TaskRefs `json:"refs,omitempty"`
		NoJob       string          `json:"no_job,omitempty"`
		DuplicateOf int64           `json:"duplicate_of,omitempty"`
		Priority    *int            `json:"priority,omitempty"`
		Kind        string          `json:"kind,omitempty"`
		Project     *string         `json:"project,omitempty"`
		Set         bool            `json:"set,omitempty"`
	}{to, in.Closure, in.Evidence, note, refs, noJob, in.DuplicateOf, in.Priority, in.Kind, in.Project, in.Set}, &out)
	if err != nil && noJob != "" {
		// Same strict-decoder translation as claims: a server that predates
		// the no_job field answers invalid_context rather than naming it.
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" {
			return out, fmt.Errorf("transition_no_job_rejected: server refused the transition with invalid_context — it likely predates no_job support")
		}
	}
	if err != nil {
		return out, err
	}
	// A 200 that does not reflect the request is a lie, not a success: the
	// state must be the one asked for, and a job_id sent for linkage must be
	// recorded (same silent-drop defense as claims).
	if !transitionApplied(to, out) {
		return out, fmt.Errorf("transition_not_applied: server response has state=%q awaiting_decision=%t, want %q", out.State, out.AwaitingDecision, to)
	}
	// Landing in an active state without the exception means the response
	// must show the accountable pair — a pre-guard or lying server can 200
	// while leaving the row unlinked.
	if noJob == "" && enteringActive(to) && activeTaskState(out.State) && (out.ClaimedBy == "" || out.Refs.JobID == "") {
		return out, fmt.Errorf("transition_linkage_missing: server response has claimed_by=%q refs.job_id=%q for an active transition", out.ClaimedBy, out.Refs.JobID)
	}
	if refs != nil && refs.JobID != "" && out.Refs.JobID != refs.JobID {
		return out, fmt.Errorf("transition_job_id_not_recorded: server response has refs.job_id=%q, want %q", out.Refs.JobID, refs.JobID)
	}
	return out, nil
}

// SetTaskParent sets (parent non-nil) or clears a task's single parent.
func (c Client) SetTaskParent(ctx context.Context, id int64, parent *int64) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/parent", id), struct {
		ParentID *int64 `json:"parent_id"`
	}{parent}, &out)
	if err != nil {
		return out, unsupportedRoute(err, "task_parent_unsupported", "the task parent route")
	}
	if (parent == nil) != (out.ParentID == nil) || (parent != nil && out.ParentID != nil && *parent != *out.ParentID) {
		return out, fmt.Errorf("parent_not_applied: server response has parent_id unchanged — it likely predates task hierarchy")
	}
	return out, nil
}

// TaskRelationResult is the answer to a relation edit: whether the edge
// changed, and the task as it reads afterwards.
type TaskRelationResult struct {
	Changed bool       `json:"changed"`
	Task    store.Task `json:"task"`
}

// AddTaskRelation adds a blocks, duplicate_of or related edge from the task
// to other. Repeating it changes nothing.
func (c Client) AddTaskRelation(ctx context.Context, id int64, kind string, other int64) (TaskRelationResult, error) {
	return c.taskRelation(ctx, id, "relations", kind, other)
}

// RemoveTaskRelation archives an edge; removing an absent one changes nothing.
func (c Client) RemoveTaskRelation(ctx context.Context, id int64, kind string, other int64) (TaskRelationResult, error) {
	return c.taskRelation(ctx, id, "relations/remove", kind, other)
}

func (c Client) taskRelation(ctx context.Context, id int64, route, kind string, other int64) (TaskRelationResult, error) {
	var out TaskRelationResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/%s", id, route), struct {
		Kind  string `json:"kind"`
		Other int64  `json:"other"`
	}{kind, other}, &out)
	if err != nil {
		return out, unsupportedRoute(err, "task_relation_unsupported", "the task relations route")
	}
	return out, nil
}

// unsupportedRoute names a missing route as version skew; other errors pass
// through unchanged.
func unsupportedRoute(err error, code, what string) error {
	var he *HTTPError
	if errors.As(err, &he) && he.Code == "" && (he.Status == http.StatusNotFound || he.Status == http.StatusMethodNotAllowed) {
		return fmt.Errorf("%s: server does not provide %s", code, what)
	}
	return err
}

// activeTaskState reports whether state is an active (started) task state in
// either vocabulary: claimed and in_progress from a server that predates the
// eleven-state model, researching, developing and reviewing from a current one.
func activeTaskState(state string) bool {
	switch state {
	case "claimed", "in_progress", store.StateResearching, store.StateDeveloping, store.StateReviewing:
		return true
	}
	return false
}

// enteringActive reports whether a transition target is one a task enters
// with the claimed_by plus job_id linkage: the old claimed and in_progress
// names or a started sub-state a worker picks up.
func enteringActive(to string) bool {
	switch to {
	case "claimed", "in_progress", store.StateResearching, store.StateDeveloping:
		return true
	}
	return false
}

// transitionApplied judges a transition response against the target the
// caller asked for, in either vocabulary. An old name maps the way the
// server maps it: in_progress and claimed ask for an active task (or, on a
// flagged task, the cleared flag), verifying and join for reviewing, hold for
// icebox, and needs_decision for the awaiting_decision flag. A response
// without a category comes from a server that predates the model, which
// answers with the old name itself.
func transitionApplied(to string, out store.Task) bool {
	if out.State == to {
		return true
	}
	newServer := out.Category != ""
	switch to {
	case "claimed", "in_progress":
		return activeTaskState(out.State) || (newServer && !out.AwaitingDecision)
	case "verifying", "join":
		return out.State == store.StateReviewing
	case "hold":
		return out.State == store.StateIcebox
	case "needs_decision":
		return newServer && out.AwaitingDecision
	case "reopened":
		return out.State == store.StateUnstarted
	}
	return false
}

// RecordDecisionRequest records a structured decision request on a task.
func (c Client) RecordDecisionRequest(ctx context.Context, id int64, in store.DecisionRequestInput) (store.DecisionRequestResult, error) {
	var out store.DecisionRequestResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/decision-request", id), in, &out)
	return out, err
}

// ResolveDecisionRequest closes a task's current decision request.
func (c Client) ResolveDecisionRequest(ctx context.Context, id int64, in store.DecisionResolveInput) (store.DecisionRequestResult, error) {
	var out store.DecisionRequestResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/decision-request/resolve", id), in, &out)
	return out, err
}

// DecisionChannelResolveInput is the resolve body with an answer channel:
// "cli" or "session", and for session the relayed claim.
type DecisionChannelResolveInput struct {
	store.DecisionResolveInput
	Channel       string `json:"channel,omitempty"`
	Provenance    string `json:"provenance,omitempty"`
	OperatorQuote string `json:"operator_quote,omitempty"`
	DecisionRef   string `json:"decision_ref,omitempty"`
}

// ResolveDecisionChannel answers a task's current decision request on a
// named channel.
func (c Client) ResolveDecisionChannel(ctx context.Context, id int64, in DecisionChannelResolveInput) (store.DecisionRequestResult, error) {
	var out store.DecisionRequestResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/decision-request/resolve", id), in, &out)
	return out, err
}

// AskDecision records a decision request and gives it its Linear issue.
func (c Client) AskDecision(ctx context.Context, id int64, in store.DecisionRequestInput) (store.DecisionAskResult, error) {
	var out store.DecisionAskResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/decision-request/ask", id), in, &out)
	return out, err
}

// AssistantPending fetches the assistant surface's pending work list
// (MGH-36): open decision requests on live tasks, pending chat questions
// with their revision, and the server's clock.
func (c Client) AssistantPending(ctx context.Context) (store.AssistantPending, error) {
	var out store.AssistantPending
	err := c.call(ctx, "GET", "/v1/assistant/pending", nil, &out)
	return out, err
}

// AssistantResolve submits the assistant's answer to an open decision
// request. Kind is empty unless the caller means "answered"; the server
// rejects anything else. No responder or by is ever sent — the server
// derives both from the token and its own constant.
func (c Client) AssistantResolve(ctx context.Context, in store.DecisionAssistantResolveInput) (store.DecisionRequestResult, error) {
	var out store.DecisionRequestResult
	err := c.call(ctx, "POST", "/v1/assistant/decisions/resolve", in, &out)
	return out, err
}

// ListUnsentNotifications reads the outbox rows still owed to the hub. A
// limit under 1 omits the parameter so the server's default applies — the
// handler rejects an explicit 0.
func (c Client) ListUnsentNotifications(ctx context.Context, limit int) ([]store.NotificationOutbox, error) {
	var out struct {
		Notifications []store.NotificationOutbox `json:"notifications"`
	}
	path := "/v1/assistant/outbox"
	if limit >= 1 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	err := c.call(ctx, "GET", path, nil, &out)
	return out.Notifications, err
}

// MarkNotificationSent reports the hub row id a drained outbox event landed
// as. The mark is idempotent per event id; hub_row_id must be the persisted
// hub row — a duplicate reply carrying id 0 is not a receipt.
func (c Client) MarkNotificationSent(ctx context.Context, eventID string, hubRowID int64) (store.NotificationOutbox, error) {
	var out store.NotificationOutbox
	err := c.call(ctx, "POST", "/v1/assistant/outbox/sent", struct {
		EventID  string `json:"event_id"`
		HubRowID int64  `json:"hub_row_id"`
	}{eventID, hubRowID}, &out)
	return out, err
}

// RelaneResult is one item's outcome in a RelaneTasks response. The batch
// continues past item failures, so callers must inspect every entry.
type RelaneResult struct {
	ID      int64       `json:"id"`
	OK      bool        `json:"ok"`
	Changed bool        `json:"changed"`
	Task    *store.Task `json:"task,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// RelaneBatch is the full relane response: per-item results plus the moved /
// unchanged / failed tallies the server counted.
type RelaneBatch struct {
	Results   []RelaneResult `json:"results"`
	Moved     int            `json:"moved"`
	Unchanged int            `json:"unchanged"`
	Failed    int            `json:"failed"`
}

// RelaneTasks moves tasks between lanes. A pre-relane server has no route:
// its mux matches GET /v1/tasks/{id} on the path and answers 405, which
// call surfaces as http_405 via its status fallback.
func (c Client) RelaneTasks(ctx context.Context, ids []int64, to, note string, allowNewLane bool) (RelaneBatch, error) {
	var out RelaneBatch
	err := c.call(ctx, "POST", "/v1/tasks/relane", struct {
		IDs          []int64 `json:"ids"`
		To           string  `json:"to"`
		Note         string  `json:"note"`
		AllowNewLane bool    `json:"allow_new_lane,omitempty"`
	}{ids, to, note, allowNewLane}, &out)
	return out, err
}
func (c Client) CreateDisposition(ctx context.Context, x store.DispositionInput) (store.Task, bool, error) {
	var out struct {
		Task    store.Task `json:"task"`
		Created bool       `json:"created"`
	}
	err := c.call(ctx, "POST", "/v1/tasks/dispositions", x, &out)
	if err != nil && x.Project != "" {
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" {
			return out.Task, false, fmt.Errorf("disposition_project_rejected: server refused the disposition with invalid_context — it likely predates the task project field")
		}
	}
	return out.Task, out.Created, err
}
func (c Client) DispositionSummary(ctx context.Context, asOf string) (store.DispositionSummary, error) {
	var out store.DispositionSummary
	path := "/v1/tasks/dispositions/summary"
	if asOf != "" {
		path += "?" + url.Values{"as_of": {asOf}}.Encode()
	}
	err := c.call(ctx, "GET", path, nil, &out)
	return out, err
}
func (c Client) ApplyDisposition(ctx context.Context, id int64, note string) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/dispositions/%d/apply", id), map[string]string{"note": note}, &out)
	return out, err
}

// taskProjectFilterOK reports the rows a project-filtered list may contain.
// A server that predates the project column silently ignores the parameter,
// so every returned row must match the requested filter — nil project never
// equals a named filter.
func taskProjectFilterOK(xs []store.Task, project *string) error {
	if project == nil {
		return nil
	}
	for _, t := range xs {
		if *project == "" {
			if t.Project != nil {
				return fmt.Errorf("list_project_filter_ignored: task %d has project %q — the server predates --project filtering", t.ID, *t.Project)
			}
			continue
		}
		if t.Project == nil || *t.Project != *project {
			return fmt.Errorf("list_project_filter_ignored: task %d does not match project %q — the server predates --project filtering", t.ID, *project)
		}
	}
	return nil
}

// taskProjectEchoOK requires the list response to echo the requested filter.
// A server that predates the project column silently drops the parameter and
// cannot echo it — and an empty page has no rows for taskProjectFilterOK to
// inspect, so the echo is the only reliable old-server signal there.
func taskProjectEchoOK(echo *string, project *string) error {
	if project == nil {
		return nil
	}
	if echo == nil || *echo != *project {
		return fmt.Errorf("list_project_filter_ignored: server response lacks the project echo — it predates --project filtering")
	}
	return nil
}

// taskAreaListOK is the area twin of the project echo and row checks. A
// server that predates the area column drops ?area and answers unfiltered, so
// the response must echo the requested filter and every row must match it.
func taskAreaListOK(xs []store.Task, echo, area *string) error {
	if area == nil {
		return nil
	}
	if echo == nil || *echo != *area {
		return fmt.Errorf("list_area_filter_ignored: server response lacks the area echo — it predates --area filtering")
	}
	for _, t := range xs {
		if *area == "" {
			if t.Area != nil {
				return fmt.Errorf("list_area_filter_ignored: task %d has area %q — the server ignored the no-area filter", t.ID, *t.Area)
			}
			continue
		}
		if t.Area == nil || *t.Area != *area {
			return fmt.Errorf("list_area_filter_ignored: task %d does not match area %q — the server ignored the filter", t.ID, *area)
		}
	}
	return nil
}

func (c Client) ListTasks(ctx context.Context, lane, state, parentLane string, project, area *string, limit int) ([]store.Task, error) {
	var out struct {
		Tasks   []store.Task `json:"tasks"`
		Project *string      `json:"project"`
		Area    *string      `json:"area"`
	}
	q := url.Values{"lane": {lane}, "state": {state}, "parent_lane": {parentLane}, "limit": {fmt.Sprint(limit)}}
	if project != nil {
		q.Set("project", *project)
	}
	if area != nil {
		q.Set("area", *area)
	}
	err := c.call(ctx, "GET", "/v1/tasks?"+q.Encode(), nil, &out)
	if err != nil {
		return nil, err
	}
	if err := taskProjectEchoOK(out.Project, project); err != nil {
		return nil, err
	}
	if err := taskProjectFilterOK(out.Tasks, project); err != nil {
		return nil, err
	}
	if err := taskAreaListOK(out.Tasks, out.Area, area); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}
func (c Client) ListTasksPage(ctx context.Context, lane, state, parentLane string, project, area *string, afterID int64, limit int) ([]store.Task, error) {
	var out struct {
		Tasks   []store.Task `json:"tasks"`
		Project *string      `json:"project"`
		Area    *string      `json:"area"`
	}
	q := url.Values{
		"lane":        {lane},
		"state":       {state},
		"parent_lane": {parentLane},
		"after_id":    {fmt.Sprint(afterID)},
		"limit":       {fmt.Sprint(limit)},
	}
	if project != nil {
		q.Set("project", *project)
	}
	if area != nil {
		q.Set("area", *area)
	}
	err := c.call(ctx, "GET", "/v1/tasks?"+q.Encode(), nil, &out)
	if err != nil {
		return nil, err
	}
	if err := taskProjectEchoOK(out.Project, project); err != nil {
		return nil, err
	}
	if err := taskProjectFilterOK(out.Tasks, project); err != nil {
		return nil, err
	}
	if err := taskAreaListOK(out.Tasks, out.Area, area); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// ListTaskProjects answers the server-configured project vocabulary. A
// server that predates the endpoint either 404/405s the route or — for GET —
// lands on /v1/tasks/{id} with id="projects" and answers invalid_context.
func (c Client) ListTaskProjects(ctx context.Context) ([]string, error) {
	var out struct {
		Projects []string `json:"projects"`
	}
	err := c.call(ctx, "GET", "/v1/tasks/projects", nil, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 405 || he.Code == "invalid_context") {
			return nil, fmt.Errorf("task_projects_unsupported: server predates the task project vocabulary endpoint")
		}
		return nil, err
	}
	return out.Projects, nil
}

// AddTaskProject extends the server-configured project vocabulary. created
// is false when the name already exists. A server that predates the route
// answers 404/405 — an invalid name is a real invalid_context rejection and
// is not translated.
func (c Client) AddTaskProject(ctx context.Context, name string) (bool, error) {
	var out struct {
		Created bool `json:"created"`
	}
	err := c.call(ctx, "POST", "/v1/tasks/projects", map[string]string{"name": name}, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) {
			return false, fmt.Errorf("task_projects_unsupported: server predates the task project vocabulary endpoint")
		}
		return false, err
	}
	return out.Created, nil
}

// SetTaskProject reclassifies one task inside the server's configured
// project vocabulary and returns the updated row. A server that predates the
// route answers 404/405, surfaced as task_project_unsupported.
func (c Client) SetTaskProject(ctx context.Context, id int64, project, note string) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/project", id), map[string]string{"project": project, "note": note}, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) {
			return out, fmt.Errorf("task_project_unsupported: server predates the task project route")
		}
		return out, err
	}
	if out.Project == nil || *out.Project != project {
		return out, fmt.Errorf("project_not_applied: server response has project unset — it likely predates the task project field")
	}
	return out, nil
}

// SetTaskArea tags one task from the server's area vocabulary and returns the
// updated row; a nil area clears the tag (the body carries an explicit null).
// A server that predates the route answers 404/405, surfaced as
// task_area_unsupported.
func (c Client) SetTaskArea(ctx context.Context, id int64, area *string, note string) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/area", id), map[string]any{"area": area, "note": note}, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) {
			return out, fmt.Errorf("task_area_unsupported: server predates the task area route")
		}
		return out, err
	}
	if (area == nil) != (out.Area == nil) || (area != nil && *out.Area != *area) {
		return out, fmt.Errorf("area_not_applied: server response does not carry the requested area — it likely predates the task area field")
	}
	return out, nil
}

// ListTaskAreas answers the server's closed area vocabulary in order. A
// server that predates the endpoint 404s it.
func (c Client) ListTaskAreas(ctx context.Context) ([]string, error) {
	var out struct {
		Areas []string `json:"areas"`
	}
	err := c.call(ctx, "GET", "/v1/task-areas", nil, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) {
			return nil, fmt.Errorf("task_areas_unsupported: server predates the task area vocabulary endpoint")
		}
		return nil, err
	}
	return out.Areas, nil
}

// ListBenchAreaExceptions answers the routing exception list. The area
// filter narrows to one vocabulary area; includeRetired asks for retired
// rows too. A server that predates the route 404/405s it, surfaced as
// bench_area_exceptions_unsupported.
func (c Client) ListBenchAreaExceptions(ctx context.Context, area string, includeRetired bool) ([]store.BenchAreaException, error) {
	q := url.Values{}
	if area != "" {
		q.Set("area", area)
	}
	if includeRetired {
		q.Set("include_retired", "1")
	}
	path := "/v1/bench/area-exceptions"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out struct {
		Exceptions []store.BenchAreaException `json:"exceptions"`
	}
	err := c.call(ctx, "GET", path, nil, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) {
			return nil, fmt.Errorf("bench_area_exceptions_unsupported: server predates the area exceptions endpoint")
		}
		return nil, err
	}
	return out.Exceptions, nil
}

// PutBenchAreaExceptions upserts exception rows under the operator
// credential and returns the count written. A non-operator client fails
// closed with the server's own operator_required 403. A server that
// predates the route 404/405s it, surfaced as
// bench_area_exceptions_unsupported.
func (c Client) PutBenchAreaExceptions(ctx context.Context, xs []store.BenchAreaException) (int, error) {
	var out struct {
		Upserted int `json:"upserted"`
	}
	err := c.call(ctx, "PUT", "/v1/bench/area-exceptions", map[string]any{"exceptions": xs}, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) {
			return 0, fmt.Errorf("bench_area_exceptions_unsupported: server predates the area exceptions endpoint")
		}
		return 0, err
	}
	return out.Upserted, nil
}

// ExportTasks fetches one consistent task snapshot and returns the exact
// response body so callers print the evidence document byte-for-byte. A
// limit below 1 omits the parameter so the server default applies.
func (c Client) ExportTasks(ctx context.Context, lane, state, parentLane string, project *string, limit int) ([]byte, error) {
	q := url.Values{"lane": {lane}, "state": {state}, "parent_lane": {parentLane}}
	if project != nil {
		q.Set("project", *project)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	t := hktransport.Client(c)
	r, e := t.NewRequest(ctx, "GET", "/v1/tasks/export?"+q.Encode(), nil)
	if e != nil {
		return nil, e
	}
	resp, e := t.Do(r)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if hktransport.IsRedirectStatus(resp.StatusCode) {
		return nil, t.RedirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return nil, &HTTPError{Status: resp.StatusCode, Code: x.Error}
	}
	if e := hktransport.HTMLBodyError(resp); e != nil {
		return nil, e
	}
	body, e := io.ReadAll(resp.Body)
	if e != nil {
		return nil, e
	}
	// A requested project filter must echo back inside scope. A server that
	// predates the field silently ignores the parameter and returns an
	// unfiltered export — the byte-for-byte document would carry no signal.
	if project != nil {
		var probe struct {
			Scope struct {
				Project *string `json:"project"`
			} `json:"scope"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			return nil, fmt.Errorf("export_project_filter_unchecked: server export body did not parse — cannot confirm the project filter was applied: %w", err)
		}
		if probe.Scope.Project == nil || *probe.Scope.Project != *project {
			return nil, fmt.Errorf("export_project_filter_ignored: server export lacks scope.project=%q — it predates --project filtering", *project)
		}
	}
	return body, nil
}
func (c Client) GetTask(ctx context.Context, id int64) (store.Task, bool, error) {
	var out store.Task
	err := c.call(ctx, "GET", fmt.Sprintf("/v1/tasks/%d", id), nil, &out)
	if ErrorCode(err) == "not_found" {
		return out, false, nil
	}
	return out, err == nil, err
}

// ResolveDecision closes an already-handled decision through the bearer API.
func (c Client) ResolveDecision(ctx context.Context, kind string, id int64, by, answer, note string, noInject bool, noJob string) (store.RelayEvent, error) {
	var out struct {
		Event store.RelayEvent `json:"event"`
	}
	err := c.call(ctx, "POST", "/v1/decisions/resolve", struct {
		Type     string `json:"type"`
		ID       int64  `json:"id"`
		By       string `json:"by"`
		Answer   string `json:"answer"`
		Note     string `json:"note,omitempty"`
		NoInject bool   `json:"no_inject,omitempty"`
		NoJob    string `json:"no_job,omitempty"`
	}{kind, id, by, answer, note, noInject, noJob}, &out)
	if err != nil && noJob != "" {
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" {
			return out.Event, fmt.Errorf("resolve_no_job_rejected: server refused the resolve with invalid_context — it likely predates no_job support")
		}
	}
	return out.Event, err
}

func (c Client) ReopenTask(ctx context.Context, id int64, reason string) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/reopen", id), struct {
		Reason string `json:"reason"`
	}{reason}, &out)
	return out, err
}

// TaskOriginResult is the answer to AddTaskOrigin: the stored origin and
// whether this call created it (false: the task already had it).
type TaskOriginResult struct {
	Origin  store.TaskOrigin `json:"origin"`
	Created bool             `json:"created"`
}

// AddTaskOrigin records where a task came from, keyed by the provider's
// durable id. Repeating it refreshes the display number and title and makes
// no second row.
func (c Client) AddTaskOrigin(ctx context.Context, id int64, in store.TaskOriginInput) (TaskOriginResult, error) {
	var out TaskOriginResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/origins", id), in, &out)
	if err != nil {
		return out, unsupportedRoute(err, "task_origin_unsupported", "the task origins route")
	}
	return out, nil
}

// ListTaskOrigins lists a task's origins.
func (c Client) ListTaskOrigins(ctx context.Context, id int64) ([]store.TaskOrigin, error) {
	var out struct {
		Origins []store.TaskOrigin `json:"origins"`
	}
	err := c.call(ctx, "GET", fmt.Sprintf("/v1/tasks/%d/origins", id), nil, &out)
	if err != nil {
		return nil, unsupportedRoute(err, "task_origin_unsupported", "the task origins route")
	}
	return out.Origins, nil
}

// FindTaskOrigins looks origins up by provider and durable external id; each
// item names its task.
func (c Client) FindTaskOrigins(ctx context.Context, provider, externalID string) ([]store.TaskOrigin, error) {
	var out struct {
		Origins []store.TaskOrigin `json:"origins"`
	}
	p := url.Values{"provider": {provider}, "external_id": {externalID}}
	err := c.call(ctx, "GET", "/v1/task-origins?"+p.Encode(), nil, &out)
	if err != nil {
		return nil, unsupportedRoute(err, "task_origin_unsupported", "the task origins route")
	}
	return out.Origins, nil
}

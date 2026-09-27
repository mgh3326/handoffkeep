// Package remote adapts the authenticated HTTP API for local CLI and stdio MCP.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type Client struct {
	URL, Token string
	HTTP       *http.Client
}

func (c Client) call(ctx context.Context, method, path string, input, output any) error {
	var body *bytes.Reader
	if input != nil {
		b, e := json.Marshal(input)
		if e != nil {
			return e
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	r, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if e != nil {
		return e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, e := h.Do(r)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct{ Error, Pattern, Reason string }
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return &HTTPError{Status: resp.StatusCode, Code: x.Error, Pattern: x.Pattern, Reason: x.Reason}
	}
	if output != nil {
		return json.NewDecoder(resp.Body).Decode(output)
	}
	return nil
}

// HTTPError is a response the server actually sent with a non-2xx status.
// Its text is unchanged from the untyped errors it replaces, so callers that
// compare err.Error() keep working; callers that must know whether a write
// may have happened check the status instead. Any other error from call —
// transport failure, timeout, an undecodable 2xx body — means the outcome of
// a write is unknown: the server may have committed it.
type HTTPError struct {
	Status  int
	Code    string
	Pattern string
	Reason  string
}

func (e *HTTPError) Error() string {
	if e.Pattern != "" {
		return fmt.Sprintf("%s:%s", e.Code, e.Pattern)
	}
	if e.Reason != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Reason)
	}
	if e.Code == "" {
		return fmt.Sprintf("http_%d", e.Status)
	}
	return e.Code
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
	if e != nil && e.Error() == "not_found" {
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
	if e != nil && e.Error() == "not_found" {
		return out, false, nil
	}
	return out, e == nil, e
}
func (c Client) GetDocumentByID(ctx context.Context, id int64) (store.Document, bool, error) {
	var out store.Document
	e := c.call(ctx, "GET", "/v1/documents?id="+fmt.Sprint(id), nil, &out)
	if e != nil && e.Error() == "not_found" {
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
func (c Client) ListDocuments(ctx context.Context, prefix, kind, session string, limit int) ([]store.Document, error) {
	var out struct {
		Documents []store.Document `json:"documents"`
	}
	p := url.Values{"prefix": {prefix}, "kind": {kind}, "session": {session}, "limit": {fmt.Sprint(limit)}}
	e := c.call(ctx, "GET", "/v1/documents?"+p.Encode(), nil, &out)
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
	r, e := http.NewRequestWithContext(ctx, "PUT", strings.TrimRight(c.URL, "/")+"/v1/attachments", bytes.NewReader(body))
	if e != nil {
		return store.Attachment{}, false, e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.Header.Set("X-HK-Name", name)
	r.Header.Set("Content-Type", mime)
	r.Header.Set("X-HK-Ref", ref)
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, e := h.Do(r)
	if e != nil {
		return store.Attachment{}, false, e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return store.Attachment{}, false, errors.New(x.Error)
	}
	var x struct {
		Attachment store.Attachment `json:"attachment"`
		Created    bool             `json:"created"`
	}
	e = json.NewDecoder(resp.Body).Decode(&x)
	return x.Attachment, x.Created, e
}
func (c Client) ListAttachments(ctx context.Context, ref string, limit int) ([]store.Attachment, error) {
	var x struct {
		Attachments []store.Attachment `json:"attachments"`
	}
	e := c.call(ctx, "GET", "/v1/attachments?"+url.Values{"ref": {ref}, "limit": {fmt.Sprint(limit)}}.Encode(), nil, &x)
	return x.Attachments, e
}
func (c Client) AttachmentURL(ctx context.Context, sha string) (string, error) {
	r, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.URL, "/")+"/v1/attachments/"+esc(sha)+"?presign=1", nil)
	if e != nil {
		return "", e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	h := c.HTTP
	if h == nil {
		h = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, e := h.Do(r)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return "", errors.New("attachment_url_failed")
	}
	return resp.Header.Get("Location"), nil
}
func (c Client) GetAttachment(ctx context.Context, sha string) (store.Attachment, io.ReadCloser, error) {
	r, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.URL, "/")+"/v1/attachments/"+esc(sha), nil)
	if e != nil {
		return store.Attachment{}, nil, e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, e := h.Do(r)
	if e != nil {
		return store.Attachment{}, nil, e
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		var x struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return store.Attachment{}, nil, errors.New(x.Error)
	}
	return store.Attachment{SHA256: sha, MIME: resp.Header.Get("Content-Type")}, resp.Body, nil
}
func (c Client) AttachmentUsage(ctx context.Context) (store.AttachmentUsage, error) {
	var x struct {
		Usage store.AttachmentUsage `json:"usage"`
	}
	e := c.call(ctx, "GET", "/v1/usage", nil, &x)
	return x.Usage, e
}

func (c Client) CreateTask(ctx context.Context, x store.Task) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", "/v1/tasks", x, &out)
	if err != nil {
		// Servers whose decoder rejects unknown fields answer a body carrying
		// project with the generic invalid_context rather than naming it.
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "invalid_context" && x.Project != nil {
			return out, fmt.Errorf("create_project_rejected: server refused the task with invalid_context — it likely predates the task project field")
		}
		return out, err
	}
	// A server that predates the project column could answer 200 while
	// silently dropping the field — same silent-drop defense as job_id.
	if x.Project != nil && (out.Project == nil || *out.Project != *x.Project) {
		return out, fmt.Errorf("create_project_not_recorded: server response has project unset — it likely predates the task project field")
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
	// row's state (claimed or in_progress), so both are honest responses.
	if out.State != "claimed" && out.State != "in_progress" {
		return out, fmt.Errorf("claim_not_applied: server response has state=%q, want claimed", out.State)
	}
	if out.ClaimedBy != claimedBy {
		return out, fmt.Errorf("claim_claimant_not_recorded: server response has claimed_by=%q, want %q", out.ClaimedBy, claimedBy)
	}
	if jobID != "" && out.Refs.JobID != jobID {
		return out, fmt.Errorf("claim_job_id_not_recorded: server response has refs.job_id=%q, want %q (server predates job_id claims)", out.Refs.JobID, jobID)
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
	if out.State != "claimed" {
		return out, fmt.Errorf("next_not_applied: server response has state=%q, want claimed", out.State)
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
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/transition", id), struct {
		To    string          `json:"to"`
		Note  string          `json:"note"`
		Refs  *store.TaskRefs `json:"refs,omitempty"`
		NoJob string          `json:"no_job,omitempty"`
	}{to, note, refs, noJob}, &out)
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
	if out.State != to {
		return out, fmt.Errorf("transition_not_applied: server response has state=%q, want %q", out.State, to)
	}
	// Landing in an active state without the exception means the response
	// must show the accountable pair — a pre-guard or lying server can 200
	// while leaving the row unlinked.
	if noJob == "" && (to == "claimed" || to == "in_progress") && (out.ClaimedBy == "" || out.Refs.JobID == "") {
		return out, fmt.Errorf("transition_linkage_missing: server response has claimed_by=%q refs.job_id=%q for an active transition", out.ClaimedBy, out.Refs.JobID)
	}
	if refs != nil && refs.JobID != "" && out.Refs.JobID != refs.JobID {
		return out, fmt.Errorf("transition_job_id_not_recorded: server response has refs.job_id=%q, want %q", out.Refs.JobID, refs.JobID)
	}
	return out, nil
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

func (c Client) ListTasks(ctx context.Context, lane, state, parentLane string, project *string, limit int) ([]store.Task, error) {
	var out struct {
		Tasks []store.Task `json:"tasks"`
	}
	q := url.Values{"lane": {lane}, "state": {state}, "parent_lane": {parentLane}, "limit": {fmt.Sprint(limit)}}
	if project != nil {
		q.Set("project", *project)
	}
	err := c.call(ctx, "GET", "/v1/tasks?"+q.Encode(), nil, &out)
	if err != nil {
		return nil, err
	}
	if err := taskProjectFilterOK(out.Tasks, project); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}
func (c Client) ListTasksPage(ctx context.Context, lane, state, parentLane string, project *string, afterID int64, limit int) ([]store.Task, error) {
	var out struct {
		Tasks []store.Task `json:"tasks"`
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
	err := c.call(ctx, "GET", "/v1/tasks?"+q.Encode(), nil, &out)
	if err != nil {
		return nil, err
	}
	if err := taskProjectFilterOK(out.Tasks, project); err != nil {
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
	r, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.URL, "/")+"/v1/tasks/export?"+q.Encode(), nil)
	if e != nil {
		return nil, e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, e := h.Do(r)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var x struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&x)
		return nil, errors.New(x.Error)
	}
	return io.ReadAll(resp.Body)
}
func (c Client) GetTask(ctx context.Context, id int64) (store.Task, bool, error) {
	var out store.Task
	err := c.call(ctx, "GET", fmt.Sprintf("/v1/tasks/%d", id), nil, &out)
	if err != nil && err.Error() == "not_found" {
		return out, false, nil
	}
	return out, err == nil, err
}

func (c Client) LinearOutboxStatus(ctx context.Context) (store.LinearOutboxStatus, error) {
	var out store.LinearOutboxStatus
	err := c.call(ctx, "GET", "/v1/linear/status", nil, &out)
	return out, err
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

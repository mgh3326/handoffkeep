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
		var x struct{ Error, Pattern string }
		_ = json.NewDecoder(resp.Body).Decode(&x)
		if x.Pattern != "" {
			return fmt.Errorf("%s:%s", x.Error, x.Pattern)
		}
		if x.Error == "" {
			return fmt.Errorf("http_%d", resp.StatusCode)
		}
		return errors.New(x.Error)
	}
	if output != nil {
		return json.NewDecoder(resp.Body).Decode(output)
	}
	return nil
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
	return out, err
}
func (c Client) ClaimTask(ctx context.Context, id int64, claimedBy string) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/claim", id), map[string]string{"claimed_by": claimedBy}, &out)
	return out, err
}
func (c Client) NextTask(ctx context.Context, lane, claimedBy string) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", "/v1/tasks/next", map[string]string{"lane": lane, "claimed_by": claimedBy}, &out)
	return out, err
}
func (c Client) TransitionTask(ctx context.Context, id int64, to, note string, refs *store.TaskRefs) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/transition", id), struct {
		To   string          `json:"to"`
		Note string          `json:"note"`
		Refs *store.TaskRefs `json:"refs,omitempty"`
	}{to, note, refs}, &out)
	return out, err
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
func (c Client) ListTasks(ctx context.Context, lane, state, parentLane string, limit int) ([]store.Task, error) {
	var out struct {
		Tasks []store.Task `json:"tasks"`
	}
	q := url.Values{"lane": {lane}, "state": {state}, "parent_lane": {parentLane}, "limit": {fmt.Sprint(limit)}}
	err := c.call(ctx, "GET", "/v1/tasks?"+q.Encode(), nil, &out)
	return out.Tasks, err
}
func (c Client) ListTasksPage(ctx context.Context, lane, state, parentLane string, afterID int64, limit int) ([]store.Task, error) {
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
	err := c.call(ctx, "GET", "/v1/tasks?"+q.Encode(), nil, &out)
	return out.Tasks, err
}

// ExportTasks fetches one consistent task snapshot and returns the exact
// response body so callers print the evidence document byte-for-byte. A
// limit below 1 omits the parameter so the server default applies.
func (c Client) ExportTasks(ctx context.Context, lane, state, parentLane string, limit int) ([]byte, error) {
	q := url.Values{"lane": {lane}, "state": {state}, "parent_lane": {parentLane}}
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
func (c Client) ResolveDecision(ctx context.Context, kind string, id int64, by, answer, note string, noInject bool) (store.RelayEvent, error) {
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
	}{kind, id, by, answer, note, noInject}, &out)
	return out.Event, err
}

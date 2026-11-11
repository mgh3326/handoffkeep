package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// WriteRemote is the ytwrite.Remote over YouTrack's REST API (slice b1a,
// docs/youtrack-write-protocol.md). It shares the Client's origin, bearer
// token, transport and response cap, but not its request loop: every call is
// one request, redirects are never followed (a POST is never replayed), and
// Retry-After is passed up as the server sent it. The protocol decides what
// happens next:
//
//	single POST observation                     what CreateComment returns     protocol outcome
//	2xx with a body that decodes to an id       the id                         applied (if the id is valid)
//	2xx with no usable id, or unreadable body   "", nil                        remote_unknown (no_id_in_reply)
//	timeout, connection loss, cancel            transport error                remote_unknown
//	3xx (not followed)                          transport error                remote_unknown
//	408, 5xx                                    *ytwrite.RemoteError           remote_unknown
//	401                                         *ytwrite.RemoteError           prepared, auth wait
//	429                                         *ytwrite.RemoteError + wait    prepared until Retry-After (kept as sent, up to ytwrite.RetryAfterCap)
//	other 4xx (400, 403, 404, 409, 422, ...)    *ytwrite.RemoteError           rejected
//
// The bot key is removed from every error text before it leaves this file, so
// it can never reach the op log.
type WriteRemote struct {
	c    *Client
	http *http.Client
	// Now is the clock a Retry-After HTTP date is read against (nil: the
	// wall clock).
	Now func() time.Time
}

var (
	_ ytwrite.Remote      = (*WriteRemote)(nil)
	_ ytwrite.IssueRemote = (*WriteRemote)(nil)
)

// NewWriteRemote wraps a configured client.
func NewWriteRemote(c *Client) *WriteRemote {
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &WriteRemote{c: c, http: &hc}
}

func (r *WriteRemote) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// Tenant is the YouTrack host the client writes to.
func (r *WriteRemote) Tenant() string { return tenantFromClient(r.c) }

// errNotFollowed is a 3xx answer: the request may have been processed.
var errNotFollowed = errors.New("youtrack answered a redirect, not followed")

// send makes exactly one request. A non-2xx answer is a *ytwrite.RemoteError;
// readErr reports a 2xx whose body could not be read in full.
func (r *WriteRemote) send(ctx context.Context, method, path string, query url.Values, body any) (data []byte, readErr error, err error) {
	var payload io.Reader
	if body != nil {
		b, merr := json.Marshal(body)
		if merr != nil {
			return nil, nil, merr
		}
		payload = bytes.NewReader(b)
	}
	endpoint := r.c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	if _, ok := ctx.Deadline(); !ok && r.c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.c.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return nil, nil, fmt.Errorf("youtrack %s %s: %w", method, path, redact(err, r.c.token))
	}
	req.Header.Set("Authorization", "Bearer "+r.c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", r.c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("youtrack %s %s: %w", method, path, redact(err, r.c.token))
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if rerr == nil && len(data) > maxBodyBytes {
		rerr = fmt.Errorf("%w: response larger than %d bytes", ErrResponseTooLarge, maxBodyBytes)
	}
	if rerr != nil {
		rerr = fmt.Errorf("youtrack %s %s: read: %w", method, path, redact(rerr, r.c.token))
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return data, rerr, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, nil, fmt.Errorf("youtrack %s %s: %w (%d)", method, path, errNotFollowed, resp.StatusCode)
	}
	apiErr := decodeAPIError(resp.StatusCode, data)
	apiErr.Method, apiErr.Path = method, path
	msg := apiErr.Error()
	if rerr != nil {
		msg += "; " + rerr.Error()
	}
	return nil, nil, &ytwrite.RemoteError{
		Status:     resp.StatusCode,
		RetryAfter: parseRetryAfterAt(resp.Header.Get("Retry-After"), ytwrite.RetryAfterCap, r.now()),
		Message:    redactText(msg, r.c.token),
	}
}

// CreateComment makes one POST /api/issues/{id}/comments.
func (r *WriteRemote) CreateComment(ctx context.Context, issueID, text string) (string, error) {
	q := url.Values{}
	q.Set("fields", "id")
	data, readErr, err := r.send(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(issueID)+"/comments", q, struct {
		Text string `json:"text"`
	}{text})
	if err != nil {
		return "", err
	}
	if readErr != nil {
		// The comment may exist; the id is unknown.
		return "", nil
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &out) != nil {
		return "", nil
	}
	return out.ID, nil
}

// listedComment is one comment as the listing returns it.
type listedComment struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Created int64  `json:"created"`
	Updated *int64 `json:"updated"`
	Deleted bool   `json:"deleted"`
	Author  *struct {
		ID string `json:"id"`
	} `json:"author"`
}

// ListComments reads every comment of the issue twice per attempt
// (ytwrite.EnumerateStableComments), with its revision time, deletion flag
// and author id.
// One request per page, no retry: a 429 comes back as a RemoteError with the
// server's wait.
func (r *WriteRemote) ListComments(ctx context.Context, issueID string, pageSize int) ([]ytwrite.RemoteComment, error) {
	if pageSize < 1 || pageSize > 1000 {
		pageSize = 100
	}
	path := "/api/issues/" + url.PathEscape(issueID) + "/comments"
	return ytwrite.EnumerateStableComments(ctx, pageSize, func(ctx context.Context, skip, top int) ([]ytwrite.RemoteComment, error) {
		q := url.Values{}
		q.Set("fields", "id,text,created,updated,deleted,author(id)")
		q.Set("$skip", strconv.Itoa(skip))
		q.Set("$top", strconv.Itoa(top))
		data, readErr, err := r.send(ctx, http.MethodGet, path, q, nil)
		if err != nil {
			return nil, err
		}
		if readErr != nil {
			return nil, readErr
		}
		var page []listedComment
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("youtrack GET %s: decode: %w", path, err)
		}
		out := make([]ytwrite.RemoteComment, 0, len(page))
		for _, c := range page {
			at := c.Created
			if c.Updated != nil {
				at = *c.Updated
			}
			rc := ytwrite.RemoteComment{ID: c.ID, Text: c.Text, UpdatedAt: time.UnixMilli(at), Deleted: c.Deleted}
			if c.Author != nil {
				rc.AuthorID = c.Author.ID
			}
			out = append(out, rc)
		}
		return out, nil
	})
}

// GetIssue reads one issue in one request (canary enrollment).
func (r *WriteRemote) GetIssue(ctx context.Context, issueID string) (Issue, error) {
	q := url.Values{}
	q.Set("fields", issueFields)
	data, readErr, err := r.send(ctx, http.MethodGet, "/api/issues/"+url.PathEscape(issueID), q, nil)
	var re *ytwrite.RemoteError
	if errors.As(err, &re) && re.Status == http.StatusNotFound {
		return Issue{}, fmt.Errorf("%w: issue %s", ErrNotFound, issueID)
	}
	if err != nil {
		return Issue{}, err
	}
	if readErr != nil {
		return Issue{}, readErr
	}
	var out Issue
	if err := json.Unmarshal(data, &out); err != nil {
		return Issue{}, fmt.Errorf("youtrack GET issue %s: decode: %w", issueID, err)
	}
	return out, nil
}

// taskIssueSearchTop is the page an issue search asks for. A task has one
// issue; a full page cannot be proven complete and is an error.
const taskIssueSearchTop = 50

// FindTaskIssues searches the project for the issues whose hk ID is taskID in
// one request (issue_create, hk 3898), checking each hit against the project
// id and the field value as FindIssueByField does.
func (r *WriteRemote) FindTaskIssues(ctx context.Context, projectShort, projectID string, taskID int64) ([]ytwrite.RemoteIssue, error) {
	field := DefaultSchema().HKIDField
	data, readErr, err := r.send(ctx, http.MethodGet, "/api/issues", issueFieldQuery(projectShort, field, taskID, taskIssueSearchTop), nil)
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	var page []Issue
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, fmt.Errorf("youtrack GET /api/issues: decode: %w", err)
	}
	if len(page) >= taskIssueSearchTop {
		return nil, fmt.Errorf("youtrack GET /api/issues: %d or more issues match hk ID %d; the search is not complete", taskIssueSearchTop, taskID)
	}
	out := []ytwrite.RemoteIssue{}
	for _, issue := range page {
		if !issueHasField(issue, projectID, field, taskID) {
			continue
		}
		hit := ytwrite.RemoteIssue{ID: issue.ID, Readable: issue.IDReadable, Summary: issue.Summary, Description: issue.Description}
		if issue.Project != nil {
			hit.ProjectID = issue.Project.ID
		}
		out = append(out, hit)
	}
	return out, nil
}

// CreateIssue makes one POST /api/issues with the project, summary,
// description and only the hk ID and hk 프로젝트 fields (issue_create): the
// other fields are a later op's, and leaving them out keeps their usual 4xx
// causes out of the create.
func (r *WriteRemote) CreateIssue(ctx context.Context, in ytwrite.IssueCreate) (string, error) {
	schema := DefaultSchema()
	fields := []CustomField{IntField(schema.HKIDField, int(in.TaskID))}
	if in.HKProject != "" {
		fields = append(fields, EnumField(schema.ProjectField, in.HKProject))
	}
	body := issueBody{Summary: in.Summary, Description: in.Description, CustomFields: fields}
	body.Project.ID = in.ProjectID
	q := url.Values{}
	q.Set("fields", "id")
	data, readErr, err := r.send(ctx, http.MethodPost, "/api/issues", q, body)
	if err != nil {
		return "", err
	}
	if readErr != nil {
		// The issue may exist; the id is unknown.
		return "", nil
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &out) != nil {
		return "", nil
	}
	return out.ID, nil
}

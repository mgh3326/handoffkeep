// Package plane contains the deliberately closed Plane REST surface used by
// the hk-to-Plane mirror. It exposes only the operations the writer and the
// reconciler need; it is not a general Plane API client.
package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAPIURL is the official Plane cloud endpoint for operators to
	// configure explicitly — NewClient refuses an empty URL so a missing
	// config can never silently target a real service.
	DefaultAPIURL = "https://api.plane.so"
	HTTPTimeout   = 10 * time.Second
)

type Operation string

const (
	OperationProjectList Operation = "project_list"
	OperationStateList   Operation = "state_list"
	OperationLabelList   Operation = "label_list"
	OperationIssueList   Operation = "issue_list"
	OperationIssueCreate Operation = "issue_create"
	OperationIssueGet    Operation = "issue_get"
	OperationIssueUpdate Operation = "issue_update"
	OperationIssueDelete Operation = "issue_delete"
)

var AllowedOperations = []Operation{
	OperationProjectList,
	OperationStateList,
	OperationLabelList,
	OperationIssueList,
	OperationIssueCreate,
	OperationIssueGet,
	OperationIssueUpdate,
	OperationIssueDelete,
}

type Config struct {
	APIURL    string
	APIKey    string
	Workspace string
	HTTP      *http.Client
}

type Client struct {
	apiURL    string
	apiKey    string
	workspace string
	http      *http.Client

	mu          sync.Mutex
	projectIDs  map[string]string
	projectByID map[string]string
}

func NewClient(config Config) (*Client, error) {
	if strings.TrimSpace(config.APIURL) == "" {
		return nil, errors.New("Plane API URL is required when sync is enabled (set HK_PLANE_API_URL, e.g. " + DefaultAPIURL + ")")
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("Plane API key is required when sync is enabled")
	}
	if strings.TrimSpace(config.Workspace) == "" {
		return nil, errors.New("Plane workspace slug is required when sync is enabled")
	}
	apiURL := strings.TrimRight(strings.TrimSpace(config.APIURL), "/")
	parsed, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Plane API URL %q: %w", config.APIURL, err)
	}
	// The client sends X-API-Key on every request, so plaintext http is
	// refused except for loopback endpoints (tests, local dev servers).
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("Plane API URL %q must use https; http is allowed only for loopback hosts", config.APIURL)
	}
	httpClient := config.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: HTTPTimeout}
	} else {
		clone := *httpClient
		if clone.Timeout == 0 {
			clone.Timeout = HTTPTimeout
		}
		httpClient = &clone
	}
	// Redirects are refused outright so do can never forward the API key to
	// a foreign host.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		apiURL:      apiURL,
		apiKey:      strings.TrimSpace(config.APIKey),
		workspace:   strings.TrimSpace(config.Workspace),
		http:        httpClient,
		projectIDs:  map[string]string{},
		projectByID: map[string]string{},
	}, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type APIError struct {
	Operation Operation
	Message   string
	Permanent bool
	Ambiguous bool
	// NotFound marks HTTP 404 responses so callers can distinguish a missing
	// remote work item from a genuine permanent failure.
	NotFound bool
}

func (err *APIError) Error() string {
	return fmt.Sprintf("plane %s: %s", err.Operation, err.Message)
}

func IsPermanent(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Permanent
}

func IsAmbiguous(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Ambiguous
}

func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.NotFound
}

func permanentError(operation Operation, message string) error {
	return &APIError{Operation: operation, Message: message, Permanent: true}
}

func operationMayWrite(operation Operation) bool {
	switch operation {
	case OperationIssueCreate, OperationIssueUpdate, OperationIssueDelete:
		return true
	default:
		return false
	}
}

func (client *Client) path(parts ...string) string {
	escaped := make([]string, 0, len(parts)+2)
	escaped = append(escaped, "api", "v1")
	escaped = append(escaped, "workspaces", url.PathEscape(client.workspace))
	for _, part := range parts {
		escaped = append(escaped, url.PathEscape(part))
	}
	return client.apiURL + "/" + strings.Join(escaped, "/") + "/"
}

// listPage mirrors Plane's cursor-paginated envelope. next_page_results is
// the documented end-of-pages signal; next_cursor feeds the ?cursor= param.
type listPage struct {
	NextCursor      string          `json:"next_cursor"`
	NextPageResults bool            `json:"next_page_results"`
	Results         json.RawMessage `json:"results"`
}

func (client *Client) do(ctx context.Context, operation Operation, method, rawURL string, input, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	request.Header.Set("X-API-Key", client.apiKey)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		return &APIError{Operation: operation, Message: err.Error(), Ambiguous: operationMayWrite(operation)}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return &APIError{Operation: operation, Message: err.Error(), Ambiguous: operationMayWrite(operation)}
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return &APIError{Operation: operation, Message: fmt.Sprintf("HTTP %d", response.StatusCode), Permanent: true}
	case response.StatusCode == http.StatusNotFound:
		return &APIError{Operation: operation, Message: "HTTP 404", Permanent: true, NotFound: true}
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return &APIError{Operation: operation, Message: fmt.Sprintf("HTTP %d", response.StatusCode), Ambiguous: operationMayWrite(operation)}
	case response.StatusCode < 200 || response.StatusCode >= 300:
		// The response body is remote-controlled text and can echo back
		// request content — it never reaches logs or outbox.last_error.
		return &APIError{Operation: operation, Message: fmt.Sprintf("HTTP %d", response.StatusCode), Permanent: true}
	}
	if output == nil {
		return nil
	}
	if len(raw) == 0 {
		return &APIError{Operation: operation, Message: "empty response", Ambiguous: operationMayWrite(operation)}
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return &APIError{Operation: operation, Message: "invalid JSON response: " + err.Error(), Ambiguous: operationMayWrite(operation)}
	}
	return nil
}

func (client *Client) listAll(ctx context.Context, operation Operation, base string, each func(json.RawMessage) error) error {
	cursor := ""
	for page := 0; ; page++ {
		if page > 100 {
			return permanentError(operation, "pagination exceeded 100 pages")
		}
		target := base
		if cursor != "" {
			target += "?cursor=" + url.QueryEscape(cursor)
		}
		var out listPage
		if err := client.do(ctx, operation, http.MethodGet, target, nil, &out); err != nil {
			return err
		}
		if len(out.Results) > 0 {
			if err := each(out.Results); err != nil {
				return err
			}
		}
		if !out.NextPageResults || out.NextCursor == "" {
			return nil
		}
		cursor = out.NextCursor
	}
}

type Project struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Name       string `json:"name"`
}

type State struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"`
}

type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Issue struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	DescriptionHTML string   `json:"description_html"`
	State           string   `json:"state"`
	Priority        string   `json:"priority"`
	Labels          []string `json:"labels"`
	Project         string   `json:"project"`
	SequenceID      int64    `json:"sequence_id"`
	ArchivedAt      *string  `json:"archived_at"`
}

// IssueInput is the create body. State and Labels are optional: an empty
// State leaves the work item in the project's default state, which is how
// Mutate:false hk states (needs_decision) arrive on the board.
type IssueInput struct {
	Name            string   `json:"name"`
	DescriptionHTML string   `json:"description_html,omitempty"`
	State           string   `json:"state,omitempty"`
	Priority        string   `json:"priority,omitempty"`
	Labels          []string `json:"labels,omitempty"`
}

// IssuePatch is the update body. Fields left empty are not sent, so a Mutate
// :false hk state (empty payload.State) freezes the remote state while other
// fields still sync.
type IssuePatch struct {
	Name     string   `json:"name,omitempty"`
	State    string   `json:"state,omitempty"`
	Priority string   `json:"priority,omitempty"`
	Labels   []string `json:"labels,omitempty"`
}

func (client *Client) ListProjects(ctx context.Context) ([]Project, error) {
	projects := []Project{}
	err := client.listAll(ctx, OperationProjectList, client.path("projects"), func(raw json.RawMessage) error {
		var page []Project
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		projects = append(projects, page...)
		return nil
	})
	return projects, err
}

func (client *Client) ensureProjects(ctx context.Context) error {
	client.mu.Lock()
	loaded := len(client.projectIDs) > 0
	client.mu.Unlock()
	if loaded {
		return nil
	}
	projects, err := client.ListProjects(ctx)
	if err != nil {
		return err
	}
	client.mu.Lock()
	for _, project := range projects {
		client.projectIDs[project.Identifier] = project.ID
		client.projectByID[project.ID] = project.Identifier
	}
	client.mu.Unlock()
	return nil
}

// ProjectIDFor resolves a Plane project identifier (the short key in issue
// numbers) to its UUID, caching the workspace project list after the first
// fetch. Unknown identifiers fail closed — a mistyped map entry must never
// silently pick another project.
func (client *Client) ProjectIDFor(ctx context.Context, identifier string) (string, error) {
	client.mu.Lock()
	cached, ok := client.projectIDs[identifier]
	client.mu.Unlock()
	if ok {
		return cached, nil
	}
	if err := client.ensureProjects(ctx); err != nil {
		return "", err
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if id, ok := client.projectIDs[identifier]; ok {
		return id, nil
	}
	return "", permanentError(OperationProjectList, "no Plane project with identifier "+identifier)
}

// ProjectIdentifierFor is the reverse lookup used by the reconciler to
// report drift in human terms.
func (client *Client) ProjectIdentifierFor(ctx context.Context, id string) (string, error) {
	if err := client.ensureProjects(ctx); err != nil {
		return "", err
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if name, ok := client.projectByID[id]; ok {
		return name, nil
	}
	return "", permanentError(OperationProjectList, "no Plane project with id "+id)
}

func (client *Client) ListStates(ctx context.Context, projectID string) ([]State, error) {
	states := []State{}
	err := client.listAll(ctx, OperationStateList, client.path("projects", projectID, "states"), func(raw json.RawMessage) error {
		var page []State
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		states = append(states, page...)
		return nil
	})
	return states, err
}

func (client *Client) ListLabels(ctx context.Context, projectID string) ([]Label, error) {
	labels := []Label{}
	err := client.listAll(ctx, OperationLabelList, client.path("projects", projectID, "labels"), func(raw json.RawMessage) error {
		var page []Label
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		labels = append(labels, page...)
		return nil
	})
	return labels, err
}

func (client *Client) ListIssues(ctx context.Context, projectID string) ([]Issue, error) {
	issues := []Issue{}
	err := client.listAll(ctx, OperationIssueList, client.path("projects", projectID, "issues"), func(raw json.RawMessage) error {
		var page []Issue
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		issues = append(issues, page...)
		return nil
	})
	return issues, err
}

// hasMarker reports whether html embeds marker bounded by a non-digit or
// the end of the string. hk:task/1 must not match inside the description of
// hk:task/10, so a bare substring search is not enough — the digit suffix
// of a longer id is the only collision shape, because task ids are decimal.
func hasMarker(html, marker string) bool {
	for offset := 0; offset+len(marker) <= len(html); {
		idx := strings.Index(html[offset:], marker)
		if idx < 0 {
			return false
		}
		end := offset + idx + len(marker)
		if end == len(html) || html[end] < '0' || html[end] > '9' {
			return true
		}
		offset += idx + 1
	}
	return false
}

// FindIssueByMarker scans one project's work items for the hk:task/<id>
// marker embedded in the mirrored description. It is the adoption path that
// makes a retried create idempotent after an ambiguous response loss.
func (client *Client) FindIssueByMarker(ctx context.Context, projectID, marker string) (Issue, bool, error) {
	if marker == "" {
		return Issue{}, false, permanentError(OperationIssueList, "empty marker")
	}
	var found []Issue
	err := client.listAll(ctx, OperationIssueList, client.path("projects", projectID, "issues"), func(raw json.RawMessage) error {
		var page []Issue
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, issue := range page {
			if hasMarker(issue.DescriptionHTML, marker) {
				found = append(found, issue)
			}
		}
		return nil
	})
	if err != nil {
		return Issue{}, false, err
	}
	if len(found) > 1 {
		return Issue{}, false, permanentError(OperationIssueList, "multiple issues contain marker "+marker)
	}
	if len(found) == 0 {
		return Issue{}, false, nil
	}
	return found[0], true, nil
}

func (client *Client) CreateIssue(ctx context.Context, projectID string, input IssueInput) (Issue, error) {
	var issue Issue
	if err := client.do(ctx, OperationIssueCreate, http.MethodPost, client.path("projects", projectID, "issues"), input, &issue); err != nil {
		return Issue{}, err
	}
	if issue.ID == "" {
		return Issue{}, permanentError(OperationIssueCreate, "create returned no issue id")
	}
	return issue, nil
}

// GetIssue distinguishes a missing remote item (found=false) from transport
// or permanent failures so the drain can recreate deleted work items.
func (client *Client) GetIssue(ctx context.Context, projectID, issueID string) (Issue, bool, error) {
	var issue Issue
	err := client.do(ctx, OperationIssueGet, http.MethodGet, client.path("projects", projectID, "issues", issueID), nil, &issue)
	if err != nil {
		if IsNotFound(err) {
			return Issue{}, false, nil
		}
		return Issue{}, false, err
	}
	if issue.ID == "" {
		return Issue{}, false, permanentError(OperationIssueGet, "issue was not found")
	}
	return issue, true, nil
}

func (client *Client) UpdateIssue(ctx context.Context, projectID, issueID string, patch IssuePatch) (Issue, error) {
	var issue Issue
	if err := client.do(ctx, OperationIssueUpdate, http.MethodPatch, client.path("projects", projectID, "issues", issueID), patch, &issue); err != nil {
		return Issue{}, err
	}
	return issue, nil
}

// DeleteIssue removes a remote work item. The writer uses it only for the
// project-move path — recreating the item under a different project — never
// for ordinary drift, where hk wins by overwriting fields.
func (client *Client) DeleteIssue(ctx context.Context, projectID, issueID string) error {
	err := client.do(ctx, OperationIssueDelete, http.MethodDelete, client.path("projects", projectID, "issues", issueID), nil, nil)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

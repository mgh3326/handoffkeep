// Package linear contains the deliberately closed Linear GraphQL surface used
// by the hk-to-Linear connector. It does not expose arbitrary GraphQL.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultAPIURL = "https://api.linear.app/graphql"
	HTTPTimeout   = 10 * time.Second
)

type Operation string

const (
	OperationIssueSearch   Operation = "issue_search"
	OperationIssueCreate   Operation = "issue_create"
	OperationIssueUpdate   Operation = "issue_update"
	OperationCommentCreate Operation = "comment_create"
	OperationCommentList   Operation = "comment_list"
	OperationIssueArchive  Operation = "issue_archive"
	OperationIssueGet      Operation = "issue_get"
	OperationTeamLookup    Operation = "team_lookup"
)

var AllowedOperations = []Operation{
	OperationIssueSearch,
	OperationIssueCreate,
	OperationIssueUpdate,
	OperationCommentCreate,
	OperationCommentList,
	OperationIssueArchive,
	OperationIssueGet,
	OperationTeamLookup,
}

type Config struct {
	APIURL string
	APIKey string
	TeamID string
	HTTP   *http.Client
}

type Client struct {
	apiURL string
	apiKey string
	teamID string
	http   *http.Client
}

func NewClient(config Config) (*Client, error) {
	if strings.TrimSpace(config.APIURL) == "" {
		config.APIURL = DefaultAPIURL
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("Linear API key is required when sync is enabled")
	}
	if strings.TrimSpace(config.TeamID) == "" {
		return nil, errors.New("Linear team ID is required when sync is enabled")
	}
	httpClient := config.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: HTTPTimeout}
	} else if httpClient.Timeout == 0 {
		clone := *httpClient
		clone.Timeout = HTTPTimeout
		httpClient = &clone
	}
	return &Client{
		apiURL: strings.TrimSpace(config.APIURL),
		apiKey: strings.TrimSpace(config.APIKey),
		teamID: strings.TrimSpace(config.TeamID),
		http:   httpClient,
	}, nil
}

type APIError struct {
	Operation Operation
	Message   string
	Permanent bool
	Ambiguous bool
}

func (err *APIError) Error() string {
	return fmt.Sprintf("linear %s: %s", err.Operation, err.Message)
}

func IsPermanent(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Permanent
}

func IsAmbiguous(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Ambiguous
}

func permanentError(operation Operation, message string) error {
	return &APIError{Operation: operation, Message: message, Permanent: true}
}

type graphqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

type graphqlEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphqlError  `json:"errors"`
}

type graphqlRequest struct {
	OperationName string `json:"operationName"`
	Query         string `json:"query"`
	Variables     any    `json:"variables"`
}

func operationMayWrite(operation Operation) bool {
	switch operation {
	case OperationIssueCreate, OperationIssueUpdate, OperationCommentCreate, OperationIssueArchive:
		return true
	default:
		return false
	}
}

func (client *Client) do(ctx context.Context, operation Operation, name, query string, variables, output any) error {
	raw, err := json.Marshal(graphqlRequest{OperationName: name, Query: query, Variables: variables})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.apiURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", client.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return &APIError{Operation: operation, Message: err.Error(), Ambiguous: operationMayWrite(operation)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return &APIError{Operation: operation, Message: err.Error(), Ambiguous: operationMayWrite(operation)}
	}
	var envelope graphqlEnvelope
	decodeErr := json.Unmarshal(body, &envelope)
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return &APIError{Operation: operation, Message: fmt.Sprintf("HTTP %d", response.StatusCode), Permanent: true}
	}
	if decodeErr == nil && len(envelope.Errors) > 0 {
		parts := make([]string, 0, len(envelope.Errors))
		permanent := false
		transient := false
		for _, item := range envelope.Errors {
			parts = append(parts, item.Message)
			switch item.Extensions.Code {
			case "AUTHENTICATION_ERROR", "FORBIDDEN":
				permanent = true
			case "RATELIMITED":
				transient = true
			}
		}
		if !transient && !permanent {
			permanent = response.StatusCode < 500
		}
		return &APIError{
			Operation: operation,
			Message:   strings.Join(parts, "; "),
			Permanent: permanent,
			Ambiguous: operationMayWrite(operation) && !permanent,
		}
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return &APIError{Operation: operation, Message: fmt.Sprintf("HTTP %d", response.StatusCode), Ambiguous: operationMayWrite(operation)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &APIError{Operation: operation, Message: fmt.Sprintf("HTTP %d", response.StatusCode), Permanent: true}
	}
	if decodeErr != nil {
		return &APIError{Operation: operation, Message: "invalid GraphQL response: " + decodeErr.Error(), Ambiguous: operationMayWrite(operation)}
	}
	if output == nil {
		return nil
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return &APIError{Operation: operation, Message: "GraphQL response has no data", Ambiguous: operationMayWrite(operation)}
	}
	if err := json.Unmarshal(envelope.Data, output); err != nil {
		return &APIError{Operation: operation, Message: "invalid GraphQL data: " + err.Error(), Ambiguous: operationMayWrite(operation)}
	}
	return nil
}

type WorkflowState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Issue struct {
	ID          string        `json:"id"`
	Identifier  string        `json:"identifier"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	ArchivedAt  *time.Time    `json:"archivedAt"`
	State       WorkflowState `json:"state"`
	Labels      struct {
		Nodes []Label `json:"nodes"`
	} `json:"labels"`
}

type Comment struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

type TeamMetadata struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	States struct {
		Nodes []WorkflowState `json:"nodes"`
	} `json:"states"`
	Labels struct {
		Nodes []Label `json:"nodes"`
	} `json:"labels"`
}

func (client *Client) SearchIssue(ctx context.Context, marker string) (Issue, bool, error) {
	const query = `query HKIssueSearch($marker: String!) {
  issues(first: 10, includeArchived: true, filter: {description: {contains: $marker}}) {
    nodes { id identifier title description archivedAt state { id name } labels { nodes { id name } } }
  }
}`
	var data struct {
		Issues struct {
			Nodes []Issue `json:"nodes"`
		} `json:"issues"`
	}
	if err := client.do(ctx, OperationIssueSearch, "HKIssueSearch", query, map[string]any{"marker": marker}, &data); err != nil {
		return Issue{}, false, err
	}
	if len(data.Issues.Nodes) > 1 {
		return Issue{}, false, permanentError(OperationIssueSearch, "multiple issues contain marker "+marker)
	}
	if len(data.Issues.Nodes) == 0 {
		return Issue{}, false, nil
	}
	return data.Issues.Nodes[0], true, nil
}

func (client *Client) LookupTeam(ctx context.Context) (TeamMetadata, error) {
	const query = `query HKTeamLookup($teamId: String!) {
  team(id: $teamId) {
    id key name
    states(first: 100) { nodes { id name } }
    labels(first: 250) { nodes { id name } }
  }
}`
	var data struct {
		Team TeamMetadata `json:"team"`
	}
	if err := client.do(ctx, OperationTeamLookup, "HKTeamLookup", query, map[string]any{"teamId": client.teamID}, &data); err != nil {
		return TeamMetadata{}, err
	}
	if data.Team.ID == "" {
		return TeamMetadata{}, permanentError(OperationTeamLookup, "team was not found")
	}
	return data.Team, nil
}

func (client *Client) CreateIssue(ctx context.Context, title, description, stateID string, labelIDs []string) (Issue, error) {
	const query = `mutation HKIssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) { success issue { id identifier title description archivedAt state { id name } labels { nodes { id name } } } }
}`
	input := map[string]any{"teamId": client.teamID, "title": title, "description": description, "stateId": stateID, "labelIds": labelIDs}
	var data struct {
		Create struct {
			Success bool  `json:"success"`
			Issue   Issue `json:"issue"`
		} `json:"issueCreate"`
	}
	if err := client.do(ctx, OperationIssueCreate, "HKIssueCreate", query, map[string]any{"input": input}, &data); err != nil {
		return Issue{}, err
	}
	if !data.Create.Success || data.Create.Issue.ID == "" {
		return Issue{}, permanentError(OperationIssueCreate, "issueCreate returned success=false")
	}
	return data.Create.Issue, nil
}

func (client *Client) UpdateIssueState(ctx context.Context, issueID, stateID string) (Issue, error) {
	const query = `mutation HKIssueUpdate($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) { success issue { id identifier title description archivedAt state { id name } labels { nodes { id name } } } }
}`
	var data struct {
		Update struct {
			Success bool  `json:"success"`
			Issue   Issue `json:"issue"`
		} `json:"issueUpdate"`
	}
	variables := map[string]any{"id": issueID, "input": map[string]any{"stateId": stateID}}
	if err := client.do(ctx, OperationIssueUpdate, "HKIssueUpdate", query, variables, &data); err != nil {
		return Issue{}, err
	}
	if !data.Update.Success {
		return Issue{}, permanentError(OperationIssueUpdate, "issueUpdate returned success=false")
	}
	return data.Update.Issue, nil
}

func (client *Client) CreateComment(ctx context.Context, issueID, body string) (Comment, error) {
	const query = `mutation HKCommentCreate($input: CommentCreateInput!) {
  commentCreate(input: $input) { success comment { id body } }
}`
	var data struct {
		Create struct {
			Success bool    `json:"success"`
			Comment Comment `json:"comment"`
		} `json:"commentCreate"`
	}
	variables := map[string]any{"input": map[string]any{"issueId": issueID, "body": body}}
	if err := client.do(ctx, OperationCommentCreate, "HKCommentCreate", query, variables, &data); err != nil {
		return Comment{}, err
	}
	if !data.Create.Success {
		return Comment{}, permanentError(OperationCommentCreate, "commentCreate returned success=false")
	}
	return data.Create.Comment, nil
}

func (client *Client) ListComments(ctx context.Context, issueID string) ([]Comment, error) {
	const query = `query HKCommentList($id: String!) {
  issue(id: $id) { comments(first: 250, includeArchived: true) { nodes { id body } } }
}`
	var data struct {
		Issue struct {
			Comments struct {
				Nodes []Comment `json:"nodes"`
			} `json:"comments"`
		} `json:"issue"`
	}
	if err := client.do(ctx, OperationCommentList, "HKCommentList", query, map[string]any{"id": issueID}, &data); err != nil {
		return nil, err
	}
	return data.Issue.Comments.Nodes, nil
}

func (client *Client) GetIssue(ctx context.Context, issueID string) (Issue, error) {
	const query = `query HKIssueGet($id: String!) {
  issue(id: $id) { id identifier title description archivedAt state { id name } labels { nodes { id name } } }
}`
	var data struct {
		Issue Issue `json:"issue"`
	}
	if err := client.do(ctx, OperationIssueGet, "HKIssueGet", query, map[string]any{"id": issueID}, &data); err != nil {
		return Issue{}, err
	}
	if data.Issue.ID == "" {
		return Issue{}, permanentError(OperationIssueGet, "issue was not found")
	}
	return data.Issue, nil
}

func (client *Client) ArchiveIssue(ctx context.Context, issueID string) error {
	const query = `mutation HKIssueArchive($id: String!) {
  issueArchive(id: $id, trash: false) { success }
}`
	var data struct {
		Archive struct {
			Success bool `json:"success"`
		} `json:"issueArchive"`
	}
	if err := client.do(ctx, OperationIssueArchive, "HKIssueArchive", query, map[string]any{"id": issueID}, &data); err != nil {
		return err
	}
	if !data.Archive.Success {
		return permanentError(OperationIssueArchive, "issueArchive returned success=false")
	}
	return nil
}

package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// POST /hooks/linear receives Linear's Comment webhooks. It carries no bearer
// token: the signature over the raw body is the authentication.
// https://linear.app/developers/webhooks

const (
	linearWebhookMaxAge   = 60 * time.Second
	linearWebhookMaxBytes = store.DocumentMaxBytes + store.MaxBytes
	linearAPIURL          = "https://api.linear.app/graphql"
	// Linear fails a delivery that takes longer than 5 seconds.
	linearAPITimeout = 3 * time.Second
)

// LinearIssue is what the webhook needs to know about a comment's issue.
type LinearIssue struct {
	Identifier  string
	Title       string
	URL         string
	Labels      []string
	StateType   string
	CompletedAt string
	CanceledAt  string
}

// LinearIssueFetcher reads an issue the webhook payload did not describe.
type LinearIssueFetcher interface {
	LinearIssue(ctx context.Context, issueID string) (LinearIssue, error)
}

// DotsRecorder is the store surface the webhook writes through.
type DotsRecorder interface {
	RecordDotsResult(ctx context.Context, kind string, issue store.DotsIssue, c store.DotsComment) (string, bool, error)
	RecordDotsFollowup(ctx context.Context, issue store.DotsIssue, commentID string) (bool, error)
}

// LinearWebhook is the handler for POST /hooks/linear.
type LinearWebhook struct {
	Secret string
	Issues LinearIssueFetcher
	Store  DotsRecorder
}

type linearWebhookPayload struct {
	Action           string `json:"action"`
	Type             string `json:"type"`
	WebhookTimestamp int64  `json:"webhookTimestamp"`
	Data             struct {
		ID        string          `json:"id"`
		Body      string          `json:"body"`
		CreatedAt string          `json:"createdAt"`
		IssueID   string          `json:"issueId"`
		Issue     json.RawMessage `json:"issue"`
	} `json:"data"`
}

// linearPayloadIssue is data.issue. Linear's documented Comment example has no
// labels, so Labels stays nil on the usual delivery and the issue is fetched.
type linearPayloadIssue struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Labels     []struct {
		Name string `json:"name"`
	} `json:"labels"`
	State struct {
		Type string `json:"type"`
	} `json:"state"`
	CompletedAt string `json:"completedAt"`
	CanceledAt  string `json:"canceledAt"`
}

// issueFromPayload returns the issue the payload describes, or false when it
// lacks labels. A shape this code does not know counts as lacking them.
func issueFromPayload(raw json.RawMessage) (LinearIssue, bool) {
	var in linearPayloadIssue
	if len(raw) == 0 || json.Unmarshal(raw, &in) != nil || in.Labels == nil {
		return LinearIssue{}, false
	}
	out := LinearIssue{Identifier: in.Identifier, Title: in.Title, URL: in.URL, Labels: []string{}, StateType: in.State.Type, CompletedAt: in.CompletedAt, CanceledAt: in.CanceledAt}
	for _, label := range in.Labels {
		out.Labels = append(out.Labels, label.Name)
	}
	return out, true
}

// verifyLinearSignature checks the header, a hex HMAC-SHA256 of the raw body.
func verifyLinearSignature(secret string, body []byte, header string) bool {
	got, err := hex.DecodeString(header)
	if err != nil || len(got) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func (h LinearWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, linearWebhookMaxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if !verifyLinearSignature(h.Secret, raw, r.Header.Get("Linear-Signature")) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// Not decode(): this is a third party's payload, and a field Linear adds
	// tomorrow must not turn every delivery into a 400.
	var payload linearWebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if age := time.Since(time.UnixMilli(payload.WebhookTimestamp)); age > linearWebhookMaxAge || age < -linearWebhookMaxAge {
		writeError(w, http.StatusUnauthorized, "stale_timestamp")
		return
	}
	status, err := h.handle(r.Context(), payload)
	if err != nil {
		var fetch linearFetchError
		if errors.As(err, &fetch) {
			// 503 makes Linear retry; the reconciling poller catches the rest.
			log.Printf("linear_webhook.fetch_failed comment=%q err=%q", payload.Data.ID, err.Error())
			writeError(w, http.StatusServiceUnavailable, "linear_unavailable")
			return
		}
		log.Printf("linear_webhook.rejected comment=%q err=%q", payload.Data.ID, err.Error())
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]string{"status": status})
}

func (h LinearWebhook) handle(ctx context.Context, payload linearWebhookPayload) (string, error) {
	const ignored = "ignored"
	data := payload.Data
	if payload.Type != "Comment" || payload.Action != "create" || data.IssueID == "" {
		return ignored, nil
	}
	kind, dots := store.DotsKind(data.Body)
	recorded := dots && store.DotsKindRecorded(kind)
	if dots && !recorded {
		return ignored, nil
	}
	issue, ok := issueFromPayload(data.Issue)
	if !ok {
		var err error
		if issue, err = h.Issues.LinearIssue(ctx, data.IssueID); err != nil {
			return "", linearFetchError{err}
		}
	}
	if !slices.Contains(issue.Labels, store.DotsLabel) {
		return ignored, nil
	}
	dotsIssue := store.DotsIssue{Identifier: issue.Identifier, Title: issue.Title, URL: issue.URL}
	if recorded {
		_, created, err := h.Store.RecordDotsResult(ctx, kind, dotsIssue, store.DotsComment{ID: data.ID, CreatedAt: data.CreatedAt, Body: data.Body})
		if err != nil {
			return "", err
		}
		if created {
			return "recorded", nil
		}
		return "duplicate", nil
	}
	if !commentAfterClose(issue, data.CreatedAt) {
		return ignored, nil
	}
	created, err := h.Store.RecordDotsFollowup(ctx, dotsIssue, data.ID)
	if err != nil {
		return "", err
	}
	if created {
		return "flagged", nil
	}
	return "duplicate", nil
}

// commentAfterClose compares Linear's ISO-8601 UTC timestamps as text, which
// orders them because Linear renders every one in the same format.
func commentAfterClose(issue LinearIssue, createdAt string) bool {
	if issue.StateType != "completed" && issue.StateType != "canceled" {
		return false
	}
	closedAt := issue.CompletedAt
	if closedAt == "" {
		closedAt = issue.CanceledAt
	}
	return closedAt != "" && createdAt > closedAt
}

// linearFetchError marks a failed read of Linear's API, as opposed to a record
// the store refused.
type linearFetchError struct{ err error }

func (e linearFetchError) Error() string { return e.err.Error() }
func (e linearFetchError) Unwrap() error { return e.err }

// LinearAPI reads issues from Linear's GraphQL API.
type LinearAPI struct {
	Key  string
	URL  string // empty means Linear's production endpoint
	HTTP *http.Client
}

const linearIssueQuery = `query($id:String!){issue(id:$id){identifier title url completedAt canceledAt state{type} labels(first:100){nodes{name}}}}`

func (a LinearAPI) LinearIssue(ctx context.Context, issueID string) (LinearIssue, error) {
	endpoint := a.URL
	if endpoint == "" {
		endpoint = linearAPIURL
	}
	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	body, err := json.Marshal(map[string]any{"query": linearIssueQuery, "variables": map[string]string{"id": issueID}})
	if err != nil {
		return LinearIssue{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, linearAPITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return LinearIssue{}, err
	}
	req.Header.Set("Authorization", a.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return LinearIssue{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return LinearIssue{}, fmt.Errorf("linear api status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Issue *struct {
				Identifier string `json:"identifier"`
				Title      string `json:"title"`
				URL        string `json:"url"`
				State      struct {
					Type string `json:"type"`
				} `json:"state"`
				CompletedAt string `json:"completedAt"`
				CanceledAt  string `json:"canceledAt"`
				Labels      struct {
					Nodes []struct {
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"issue"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, store.MaxBytes)).Decode(&out); err != nil {
		return LinearIssue{}, fmt.Errorf("linear api response: %w", err)
	}
	if len(out.Errors) > 0 {
		return LinearIssue{}, fmt.Errorf("linear api error: %s", out.Errors[0].Message)
	}
	if out.Data.Issue == nil {
		return LinearIssue{}, errors.New("linear api returned no issue")
	}
	issue := out.Data.Issue
	result := LinearIssue{Identifier: issue.Identifier, Title: issue.Title, URL: issue.URL, Labels: []string{}, StateType: issue.State.Type, CompletedAt: issue.CompletedAt, CanceledAt: issue.CanceledAt}
	for _, label := range issue.Labels.Nodes {
		result.Labels = append(result.Labels, label.Name)
	}
	return result, nil
}

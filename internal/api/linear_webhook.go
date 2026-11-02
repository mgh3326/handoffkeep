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

// POST /hooks/linear receives Linear's Comment and Issue webhooks. It carries no bearer
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
	// Decisions turns on the decision path. Nil leaves it off, and Operator (the
	// Linear user id allowed to answer) and StateOptions are unused.
	Decisions    LinearDecisionRecorder
	Operator     string
	StateOptions map[string]int
}

type linearWebhookPayload struct {
	Action           string `json:"action"`
	Type             string `json:"type"`
	WebhookTimestamp int64  `json:"webhookTimestamp"`
	Actor            *struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"actor"`
	UpdatedFrom map[string]json.RawMessage `json:"updatedFrom"`
	Data        struct {
		ID        string          `json:"id"`
		Body      string          `json:"body"`
		CreatedAt string          `json:"createdAt"`
		UpdatedAt string          `json:"updatedAt"`
		IssueID   string          `json:"issueId"`
		Issue     json.RawMessage `json:"issue"`
		UserID    string          `json:"userId"`
		User      *struct {
			ID string `json:"id"`
		} `json:"user"`
		BotActor json.RawMessage `json:"botActor"`
		State    struct {
			Name string `json:"name"`
		} `json:"state"`
		Labels []linearPayloadLabel `json:"labels"`
	} `json:"data"`
}

type linearPayloadLabel struct {
	Name string `json:"name"`
}

// movedState reports whether this update changed the issue's state.
func (p linearWebhookPayload) movedState() bool {
	_, moved := p.UpdatedFrom["stateId"]
	return moved
}

// linearPayloadIssue is data.issue. Linear's documented Comment example has no
// labels, so Labels stays nil on the usual delivery and the issue is fetched.
type linearPayloadIssue struct {
	Identifier string               `json:"identifier"`
	Title      string               `json:"title"`
	URL        string               `json:"url"`
	Labels     []linearPayloadLabel `json:"labels"`
	State      struct {
		Type string `json:"type"`
	} `json:"state"`
	CompletedAt string `json:"completedAt"`
	CanceledAt  string `json:"canceledAt"`
}

// issueFromPayload returns the issue the payload describes, or false when it
// must be fetched instead. Labels without "dots" settle the delivery on their
// own; a dots issue must also carry what its records quote and what the close
// check reads. A shape this code does not know counts as lacking labels.
func issueFromPayload(raw json.RawMessage) (LinearIssue, bool) {
	var in linearPayloadIssue
	if len(raw) == 0 || json.Unmarshal(raw, &in) != nil || in.Labels == nil {
		return LinearIssue{}, false
	}
	out := LinearIssue{Identifier: in.Identifier, Title: in.Title, URL: in.URL, Labels: []string{}, StateType: in.State.Type, CompletedAt: in.CompletedAt, CanceledAt: in.CanceledAt}
	for _, label := range in.Labels {
		out.Labels = append(out.Labels, label.Name)
	}
	return out, !slices.Contains(out.Labels, store.DotsLabel) || out.complete()
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
	status, err := h.handle(r.Context(), payload, r.Header.Get("Linear-Delivery"))
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

func (h LinearWebhook) handle(ctx context.Context, payload linearWebhookPayload, delivery string) (string, error) {
	const ignored = "ignored"
	data := payload.Data
	if h.Decisions != nil && payload.Type == "Issue" && payload.Action == "update" {
		return h.handleDecisionIssue(ctx, payload, delivery)
	}
	if payload.Type != "Comment" || payload.Action != "create" || data.IssueID == "" {
		return ignored, nil
	}
	if h.Decisions != nil {
		_, linked, err := h.Decisions.LinearDecisionLinkByIssue(ctx, data.IssueID)
		if err != nil {
			return "", err
		}
		if linked {
			return h.handleDecisionComment(ctx, payload, delivery)
		}
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
	closedAt, _ := issue.closedAt()
	return closedAt != "" && createdAt > closedAt
}

// closedAt returns the close time of the issue's current state, and whether
// that state is a closed one.
func (i LinearIssue) closedAt() (string, bool) {
	switch i.StateType {
	case "completed":
		return i.CompletedAt, true
	case "canceled":
		return i.CanceledAt, true
	}
	return "", false
}

// complete reports whether the issue has everything a dots record needs.
func (i LinearIssue) complete() bool {
	closedAt, closed := i.closedAt()
	return i.Identifier != "" && i.Title != "" && i.URL != "" && i.StateType != "" && (!closed || closedAt != "")
}

// linearFetchError marks a failed read of Linear's API, as opposed to a record
// the store refused.
type linearFetchError struct{ err error }

func (e linearFetchError) Error() string { return e.err.Error() }
func (e linearFetchError) Unwrap() error { return e.err }

// LinearAPI reads issues from Linear's GraphQL API.
type LinearAPI struct {
	Key string
	// App, when set, replaces Key: requests carry a Bearer token minted for the
	// OAuth application, so hk's writes show as the app, not as a member.
	App  *LinearAppToken
	URL  string // empty means Linear's production endpoint
	HTTP *http.Client
}

const linearIssueQuery = `query($id:String!){issue(id:$id){identifier title url completedAt canceledAt state{type} labels(first:100){nodes{name}}}}`

func (a LinearAPI) LinearIssue(ctx context.Context, issueID string) (LinearIssue, error) {
	var out struct {
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
	}
	if err := a.graphql(ctx, linearAPITimeout, linearIssueQuery, map[string]any{"id": issueID}, &out); err != nil {
		return LinearIssue{}, err
	}
	if out.Issue == nil {
		return LinearIssue{}, errors.New("linear api returned no issue")
	}
	issue := out.Issue
	result := LinearIssue{Identifier: issue.Identifier, Title: issue.Title, URL: issue.URL, Labels: []string{}, StateType: issue.State.Type, CompletedAt: issue.CompletedAt, CanceledAt: issue.CanceledAt}
	for _, label := range issue.Labels.Nodes {
		result.Labels = append(result.Labels, label.Name)
	}
	return result, nil
}

// graphql runs one request against Linear and decodes its data into out.
func (a LinearAPI) graphql(ctx context.Context, timeout time.Duration, query string, variables map[string]any, out any) error {
	endpoint := a.URL
	if endpoint == "" {
		endpoint = linearAPIURL
	}
	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	auth := a.Key
	if a.App != nil {
		token, err := a.App.Token(ctx)
		if err != nil {
			return err
		}
		auth = "Bearer " + token
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear api status %d", resp.StatusCode)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, store.MaxBytes)).Decode(&envelope); err != nil {
		return fmt.Errorf("linear api response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("linear api error: %s", envelope.Errors[0].Message)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("linear api response: %w", err)
	}
	return nil
}

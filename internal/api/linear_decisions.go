package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Operator decisions through Linear: asking creates one issue per request, and
// the webhook turns the operator's comment or state change into a resolution.
// See docs/linear-decisions.md.

const (
	// linearWriteTimeout bounds one call made outside a webhook delivery.
	linearWriteTimeout = 10 * time.Second
	// linearTitleMaxRunes keeps a long question out of the title; the issue body
	// carries it in full.
	linearTitleMaxRunes = 200

	linearDecisionOpsInterval = 5 * time.Second
)

// errLinearDecisionTarget means the workspace lacks the team, label or canceled
// state an issue needs. Nothing has been written when it is returned.
var errLinearDecisionTarget = errors.New("linear_decision_target_missing")

// LinearDecisionTarget is what creating and closing a decision issue needs.
type LinearDecisionTarget struct {
	TeamID        string
	LabelID       string
	CancelStateID string
}

// LinearDecisionIssue is the input for creating an issue.
type LinearDecisionIssue struct {
	Board       bool
	TaskID      int64
	RequestID   string
	ID          string
	TeamID      string
	LabelID     string
	Title       string
	Description string
}

// LinearDecisionAPI is the part of Linear that asking and the ops worker write
// through. LinearAPI implements it.
type LinearDecisionAPI interface {
	LinearIssueFetcher
	LinearDecisionTarget(ctx context.Context, teamKey, label string) (LinearDecisionTarget, error)
	CreateLinearIssue(ctx context.Context, in LinearDecisionIssue) (LinearIssue, error)
	CreateLinearComment(ctx context.Context, id, issueID, body string) error
	UpdateLinearIssueState(ctx context.Context, issueID, stateID string) error
}

// decisionAskStore is the store surface asking writes through.
type decisionAskStore interface {
	RecordDecisionRequest(ctx context.Context, taskID int64, by string, in store.DecisionRequestInput) (store.DecisionRequestResult, error)
	LinearDecisionLinkByRequest(ctx context.Context, requestID string) (store.LinearDecisionLink, bool, error)
	RecordLinearDecisionIssue(ctx context.Context, link store.LinearDecisionLink, supersedes, closeStateID, announce string) (store.LinearDecisionLink, bool, error)
}

// LinearDecisions asks the operator through Linear.
type LinearDecisions struct {
	Team   string
	Linear LinearDecisionAPI
}

// Ask records the request and gives it its one issue. The order makes every
// retry safe: a failure after the request is recorded leaves it open with no
// issue, and re-sending the same input finds the same request and carries on.
// The issue id derives from the request id, so even a crash between creating the
// issue and storing the link cannot make a second one.
func (l LinearDecisions) Ask(ctx context.Context, st decisionAskStore, taskID int64, by string, in store.DecisionRequestInput) (store.DecisionAskResult, error) {
	target, err := l.Linear.LinearDecisionTarget(ctx, l.Team, store.LinearDecisionLabel)
	if err != nil {
		return store.DecisionAskResult{}, err
	}
	recorded, err := st.RecordDecisionRequest(ctx, taskID, by, in)
	if err != nil {
		return store.DecisionAskResult{}, err
	}
	request := recorded.Request
	var options store.DecisionOptions
	if recorded.Task.Refs.DecisionOptions != nil {
		options = *recorded.Task.Refs.DecisionOptions
	}
	ver := store.DecisionRequestVer(request.Question, options, request.Doc, request.Refs)
	out := store.DecisionAskResult{Task: recorded.Task, Request: request, DecisionRef: store.DecisionRef(request.ID), Ver: ver, Duplicate: recorded.Duplicate}
	link, found, err := st.LinearDecisionLinkByRequest(ctx, request.ID)
	if err != nil {
		return store.DecisionAskResult{}, err
	}
	if found {
		out.Issue = &link
		return out, nil
	}
	if request.Status != store.DecisionRequestOpen {
		return out, nil
	}
	issueID := store.LinearDecisionUUID("issue:" + request.ID)
	issue, err := l.Linear.CreateLinearIssue(ctx, LinearDecisionIssue{
		ID: issueID, TaskID: taskID, RequestID: request.ID, TeamID: target.TeamID, LabelID: target.LabelID,
		Title: linearDecisionTitle(request), Description: linearDecisionBody(request, options, ver),
	})
	if err != nil {
		// The create may have reached Linear and lost its response, or a retry
		// may have hit the issue an earlier attempt made. Only a read can tell.
		existing, readErr := l.Linear.LinearIssue(ctx, issueID)
		if readErr != nil {
			return store.DecisionAskResult{}, linearFetchError{fmt.Errorf("create issue: %w", err)}
		}
		issue = existing
	}
	stored, created, err := st.RecordLinearDecisionIssue(ctx, store.LinearDecisionLink{
		RequestID: request.ID, TaskID: taskID, Ver: ver, IssueID: issueID, Identifier: issue.Identifier, URL: issue.URL,
	}, request.Supersedes, target.CancelStateID, decisionAnnouncement(request, options, ver, issue.URL))
	if err != nil {
		return store.DecisionAskResult{}, err
	}
	out.Issue = &stored
	out.IssueCreated = created
	return out, nil
}

func linearDecisionTitle(request store.DecisionRequest) string {
	line, _, _ := strings.Cut(strings.TrimSpace(request.Question), "\n")
	runes := []rune(strings.TrimSpace(line))
	if len(runes) > linearTitleMaxRunes {
		line = string(runes[:linearTitleMaxRunes]) + "…"
	} else {
		line = string(runes)
	}
	return "[" + store.DecisionRef(request.ID) + "] " + line
}

// linearDecisionDeadlineZone renders deadlines in the operator's time zone. It
// is a fixed offset so a server image without tzdata renders the same text.
var linearDecisionDeadlineZone = time.FixedZone("KST", 9*60*60)

func linearDecisionBody(request store.DecisionRequest, options store.DecisionOptions, ver string) string {
	ref := store.DecisionRef(request.ID)
	var b strings.Builder
	b.WriteString(request.Question + "\n\n**Options**\n\n")
	for i, option := range options.Options {
		fmt.Fprintf(&b, "%d. %s: %s", i+1, option.Key, option.Label)
		if option.Recommended {
			b.WriteString(" (recommended)")
		}
		b.WriteString("\n")
	}
	if request.Reason != "" {
		b.WriteString("\n**Why recommended:** " + request.Reason + "\n")
	}
	b.WriteString("\n**Default:** " + request.DefaultAction)
	if request.DefaultOption != "" {
		b.WriteString(" (option " + request.DefaultOption + ")")
	}
	if request.DefaultTrigger != "" {
		b.WriteString(". Applied: " + request.DefaultTrigger)
	}
	b.WriteString("\n\n**Deadline:** ")
	if request.DueAt != nil {
		b.WriteString(request.DueAt.In(linearDecisionDeadlineZone).Format("2006-01-02 15:04 MST"))
	} else {
		b.WriteString("none")
	}
	b.WriteString("\n")
	refs := slices.Clone(request.Refs)
	if request.Doc != "" {
		refs = append([]string{"hk:doc " + request.Doc}, refs...)
	}
	if len(refs) > 0 {
		b.WriteString("\n**Refs:**\n\n")
		for _, r := range refs {
			b.WriteString("- " + r + "\n")
		}
	}
	first := "1"
	fmt.Fprintf(&b, "\n**To answer**, comment `%s` or `%s %s` as the first line. Only the operator's first valid answer counts.\n\n", first, ref, first)
	b.WriteString("Request `" + request.ID + "`, `ver=" + ver + "`\n")
	return b.String()
}

// discordContentMaxRunes is Discord's limit on one message.
const discordContentMaxRunes = 2000

// decisionAnnouncement is the channel message for a new issue. The question is
// what gets cut when the message is too long, so the options, ver, deadline and
// link always arrive.
func decisionAnnouncement(request store.DecisionRequest, options store.DecisionOptions, ver, url string) string {
	ref := store.DecisionRef(request.ID)
	deadline := "no deadline"
	if request.DueAt != nil {
		deadline = "deadline " + request.DueAt.In(linearDecisionDeadlineZone).Format("2006-01-02 15:04 MST")
	}
	head := "🗳️ **" + ref + "** ver=" + ver + ", " + deadline + "\n"
	var tail strings.Builder
	for i, option := range options.Options {
		fmt.Fprintf(&tail, "\n%d. %s: %s", i+1, option.Key, option.Label)
	}
	fmt.Fprintf(&tail, "\nAnswer on Linear, first line of a comment: `1` or `%s 1`\n%s", ref, url)
	const separator = "\n"
	room := discordContentMaxRunes - utf8.RuneCountInString(head) - utf8.RuneCountInString(separator) - utf8.RuneCountInString(tail.String())
	question := []rune(request.Question)
	if len(question) > room {
		question = append(question[:max(room-1, 0)], '…')
	}
	return head + string(question) + separator + tail.String()
}

// taskDecisionAsk is POST /v1/tasks/{id}/decision-request/ask. 201 means this
// call created the issue; 200 means there was nothing left to create.
func (s Server) taskDecisionAsk(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if s.LinearDecisions == nil {
		writeError(w, http.StatusServiceUnavailable, "linear_decisions_disabled")
		return
	}
	defer r.Body.Close()
	var input store.DecisionRequestInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	out, err := s.LinearDecisions.Ask(r.Context(), s.Service.Store, id, client, input)
	if err != nil {
		var fetch linearFetchError
		switch {
		case errors.Is(err, errLinearDecisionTarget):
			log.Printf("linear_decisions.target_missing task=%d err=%q", id, err.Error())
			writeError(w, http.StatusConflict, "linear_decision_target_missing")
		case errors.As(err, &fetch):
			log.Printf("linear_decisions.unavailable task=%d err=%q", id, err.Error())
			writeError(w, http.StatusServiceUnavailable, "linear_unavailable")
		default:
			appErr(w, err)
		}
		return
	}
	status := http.StatusOK
	if out.IssueCreated {
		status = http.StatusCreated
	}
	jsonOut(w, status, out)
}

// LinearDecisionRecorder is the store surface the webhook's decision path uses.
type LinearDecisionRecorder interface {
	LinearDecisionLinkByIssue(ctx context.Context, issueID string) (store.LinearDecisionLink, bool, error)
	AnswerLinearDecision(ctx context.Context, in store.LinearDecisionAnswer) (string, error)
}

// ParseLinearDecisionStates reads the state mapping, e.g. "Approved=1,Rejected=2":
// a Linear state name and the option number moving there chooses.
func ParseLinearDecisionStates(raw string) (map[string]int, error) {
	out := map[string]int{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, part := range strings.Split(raw, ",") {
		name, number, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		n, err := strconv.Atoi(strings.TrimSpace(number))
		if !ok || name == "" || err != nil || n < 1 || n > 6 {
			return nil, fmt.Errorf("invalid state mapping %q: want Name=<option number 1-6>", part)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("invalid state mapping: %q appears twice", name)
		}
		out[name] = n
	}
	return out, nil
}

// operatorActed reports whether the webhook's actor is the operator as a person.
// An OAuth client or integration acting under that user does not count.
func (p linearWebhookPayload) operatorActed(operator string) bool {
	return p.Actor != nil && p.Actor.ID == operator && strings.EqualFold(p.Actor.Type, "user")
}

// operatorCommented requires the comment's own author. Linear documents actor as
// nullable, so a missing actor falls back on the author alone; an actor that is
// present must agree, and a comment made by a bot never counts.
func (p linearWebhookPayload) operatorCommented(operator string) bool {
	data := p.Data
	author := data.UserID
	if author == "" && data.User != nil {
		author = data.User.ID
	}
	if author != operator || !jsonAbsent(data.BotActor) {
		return false
	}
	return p.Actor == nil || p.operatorActed(operator)
}

func jsonAbsent(raw []byte) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// handleDecisionComment applies a comment on an issue that carries a request.
func (h LinearWebhook) handleDecisionComment(ctx context.Context, payload linearWebhookPayload, delivery string) (string, error) {
	const ignored = "ignored"
	data := payload.Data
	answer, ok := store.ParseLinearDecisionComment(data.Body)
	if !ok {
		return ignored, nil
	}
	if !payload.operatorCommented(h.Operator) {
		log.Printf("linear_webhook.decision_not_operator comment=%q issue=%q", data.ID, data.IssueID)
		return ignored, nil
	}
	issue, labelled, err := h.decisionIssue(ctx, data.Issue, data.IssueID)
	if err != nil {
		return "", err
	}
	if !labelled {
		return ignored, nil
	}
	_, closed := issue.closedAt()
	return h.Decisions.AnswerLinearDecision(ctx, store.LinearDecisionAnswer{
		IssueID: data.IssueID, Source: store.LinearDecisionSourceComment, UserID: h.Operator, CommentID: data.ID, DeliveryID: delivery,
		EventAt: data.CreatedAt, RawText: data.Body, Ref: answer.Ref, Ver: answer.Ver, Choice: answer.Choice, IssueClosed: closed,
	})
}

// decisionIssue reads the labels and state from the payload when it has both,
// and from Linear otherwise.
func (h LinearWebhook) decisionIssue(ctx context.Context, raw []byte, issueID string) (LinearIssue, bool, error) {
	if issue, ok := issueFromPayload(raw); ok && issue.StateType != "" {
		return issue, slices.Contains(issue.Labels, store.LinearDecisionLabel), nil
	} else if ok && !slices.Contains(issue.Labels, store.LinearDecisionLabel) {
		return issue, false, nil
	}
	issue, err := h.Issues.LinearIssue(ctx, issueID)
	if err != nil {
		return LinearIssue{}, false, linearFetchError{err}
	}
	return issue, slices.Contains(issue.Labels, store.LinearDecisionLabel), nil
}

// handleDecisionIssue applies an Issue update: a move into a mapped state, by the
// operator, on an issue that carries a request. updatedFrom must name the state,
// or any later edit of an issue resting in that state would count as an answer.
func (h LinearWebhook) handleDecisionIssue(ctx context.Context, payload linearWebhookPayload, delivery string) (string, error) {
	const ignored = "ignored"
	data := payload.Data
	number, mapped := h.StateOptions[data.State.Name]
	if !mapped || !payload.movedState() {
		return ignored, nil
	}
	if _, found, err := h.Decisions.LinearDecisionLinkByIssue(ctx, data.ID); err != nil || !found {
		return ignored, err
	}
	if !payload.operatorActed(h.Operator) {
		log.Printf("linear_webhook.decision_not_operator issue=%q state=%q", data.ID, data.State.Name)
		return ignored, nil
	}
	labelled := slices.ContainsFunc(data.Labels, func(l linearPayloadLabel) bool { return l.Name == store.LinearDecisionLabel })
	if data.Labels == nil {
		var err error
		if _, labelled, err = h.decisionIssue(ctx, nil, data.ID); err != nil {
			return "", err
		}
	}
	if !labelled {
		return ignored, nil
	}
	covered := map[int]bool{}
	for _, n := range h.StateOptions {
		covered[n] = true
	}
	return h.Decisions.AnswerLinearDecision(ctx, store.LinearDecisionAnswer{
		IssueID: data.ID, Source: store.LinearDecisionSourceState, UserID: h.Operator, DeliveryID: delivery, EventAt: data.UpdatedAt,
		RawText: "state: " + data.State.Name, Choice: strconv.Itoa(number), StateOptions: covered,
	})
}

// LinearDecisionOpsStore is the store surface the ops worker drains.
type LinearDecisionOpsStore interface {
	ProcessNextLinearDecisionOp(ctx context.Context, process func(context.Context, store.LinearDecisionOp) error) (bool, error, error)
}

// LinearDecisionOpsWorker sends the comments and closes the decision flow owes
// Linear. They are queued, not sent in line, because Linear fails a delivery that
// takes over 5 seconds and a reply must survive a Linear outage.
type LinearDecisionOpsWorker struct {
	Store  LinearDecisionOpsStore
	Linear LinearDecisionAPI
	// Discord posts to the decisions channel. Nil settles its ops unsent.
	Discord  *DiscordWebhook
	Interval time.Duration
}

// Run drains due ops every interval until ctx ends.
func (w LinearDecisionOpsWorker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = linearDecisionOpsInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		w.Drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Drain sends every op now due.
func (w LinearDecisionOpsWorker) Drain(ctx context.Context) {
	for ctx.Err() == nil {
		found, opErr, err := w.Store.ProcessNextLinearDecisionOp(ctx, w.send)
		switch {
		case errors.Is(opErr, store.ErrLinearDecisionOpGaveUp):
			log.Printf("linear_decisions.op_gave_up err=%q", opErr.Error())
		case opErr != nil:
			log.Printf("linear_decisions.op_failed err=%q", opErr.Error())
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("linear_decisions.drain_error err=%q", err.Error())
			}
			return
		}
		if !found {
			return
		}
	}
}

func (w LinearDecisionOpsWorker) send(ctx context.Context, op store.LinearDecisionOp) error {
	switch op.Op {
	case store.LinearDecisionOpComment:
		return w.Linear.CreateLinearComment(ctx, store.LinearDecisionUUID(op.DedupeKey), op.IssueID, op.Body)
	case store.LinearDecisionOpClose:
		return w.Linear.UpdateLinearIssueState(ctx, op.IssueID, op.StateID)
	case store.LinearDecisionOpDiscord:
		if w.Discord == nil {
			return store.ErrLinearDecisionOpSkipped
		}
		return w.Discord.Post(ctx, op.Body)
	}
	return fmt.Errorf("unknown linear decision op %q", op.Op)
}

// DiscordWebhook posts to one Discord incoming webhook. Its URL is a credential,
// so no error it returns may contain the URL.
type DiscordWebhook struct {
	URL  string
	HTTP *http.Client
}

// ParseDiscordWebhookURL accepts only an https URL, so the credential never
// crosses the network in the clear. The message says why without quoting it.
func ParseDiscordWebhookURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("discord webhook url must be an https url")
	}
	return raw, nil
}

// Post sends one message. allowed_mentions is empty so text from a question can
// never ping @everyone.
func (d DiscordWebhook) Post(ctx context.Context, content string) error {
	body, err := json.Marshal(map[string]any{"content": content, "allowed_mentions": map[string]any{"parse": []string{}}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, linearWriteTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return errors.New("discord webhook request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	client := d.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// *url.Error quotes the full URL; keep only its cause.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("discord webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("discord webhook status %d", resp.StatusCode)
	}
	return nil
}

const (
	linearDecisionTargetQuery = `query($key:String!,$label:String!){teams(filter:{key:{eq:$key}}){nodes{id states{nodes{id type position}}}} issueLabels(filter:{name:{eq:$label}}){nodes{id team{id}}}}`
	linearIssueCreateMutation = `mutation($input:IssueCreateInput!){issueCreate(input:$input){success issue{identifier url}}}`
	linearCommentMutation     = `mutation($input:CommentCreateInput!){commentCreate(input:$input){success}}`
	linearIssueUpdateMutation = `mutation($id:String!,$input:IssueUpdateInput!){issueUpdate(id:$id,input:$input){success}}`
)

// LinearDecisionTarget resolves the team, its first canceled state and the label,
// which may belong to the team or the whole workspace.
func (a LinearAPI) LinearDecisionTarget(ctx context.Context, teamKey, label string) (LinearDecisionTarget, error) {
	var out struct {
		Teams struct {
			Nodes []struct {
				ID     string `json:"id"`
				States struct {
					Nodes []struct {
						ID       string  `json:"id"`
						Type     string  `json:"type"`
						Position float64 `json:"position"`
					} `json:"nodes"`
				} `json:"states"`
			} `json:"nodes"`
		} `json:"teams"`
		IssueLabels struct {
			Nodes []struct {
				ID   string `json:"id"`
				Team *struct {
					ID string `json:"id"`
				} `json:"team"`
			} `json:"nodes"`
		} `json:"issueLabels"`
	}
	if err := a.graphql(ctx, linearWriteTimeout, linearDecisionTargetQuery, map[string]any{"key": teamKey, "label": label}, &out); err != nil {
		return LinearDecisionTarget{}, linearFetchError{err}
	}
	if len(out.Teams.Nodes) != 1 {
		return LinearDecisionTarget{}, fmt.Errorf("%w: team %s not found", errLinearDecisionTarget, teamKey)
	}
	team := out.Teams.Nodes[0]
	target := LinearDecisionTarget{TeamID: team.ID}
	position := 0.0
	for _, state := range team.States.Nodes {
		if state.Type == "canceled" && (target.CancelStateID == "" || state.Position < position) {
			target.CancelStateID, position = state.ID, state.Position
		}
	}
	if target.CancelStateID == "" {
		return LinearDecisionTarget{}, fmt.Errorf("%w: team %s has no canceled state", errLinearDecisionTarget, teamKey)
	}
	for _, node := range out.IssueLabels.Nodes {
		if node.Team != nil && node.Team.ID == team.ID {
			target.LabelID = node.ID
			break
		}
		if node.Team == nil && target.LabelID == "" {
			target.LabelID = node.ID
		}
	}
	if target.LabelID == "" {
		return LinearDecisionTarget{}, fmt.Errorf("%w: label %s not found for team %s", errLinearDecisionTarget, label, teamKey)
	}
	return target, nil
}

// CreateLinearIssue creates an issue under a client-supplied id.
func (a LinearAPI) CreateLinearIssue(ctx context.Context, in LinearDecisionIssue) (LinearIssue, error) {
	return a.admittedCreate(ctx, in)
}

func (a LinearAPI) createLinearIssue(ctx context.Context, in LinearDecisionIssue) (LinearIssue, error) {
	var out struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   *struct {
				Identifier string `json:"identifier"`
				URL        string `json:"url"`
			} `json:"issue"`
		} `json:"issueCreate"`
	}
	input := map[string]any{"id": in.ID, "teamId": in.TeamID, "labelIds": []string{in.LabelID}, "title": in.Title, "description": in.Description}
	if err := a.graphql(ctx, linearWriteTimeout, linearIssueCreateMutation, map[string]any{"input": input}, &out); err != nil {
		return LinearIssue{}, err
	}
	if !out.IssueCreate.Success || out.IssueCreate.Issue == nil {
		return LinearIssue{}, linearRejectionError{err: errors.New("linear api did not create the issue")}
	}
	return LinearIssue{Identifier: out.IssueCreate.Issue.Identifier, URL: out.IssueCreate.Issue.URL}, nil
}

// CreateLinearComment posts a comment under a client-supplied id.
func (a LinearAPI) CreateLinearComment(ctx context.Context, id, issueID, body string) error {
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	input := map[string]any{"id": id, "issueId": issueID, "body": body}
	if err := a.graphql(ctx, linearWriteTimeout, linearCommentMutation, map[string]any{"input": input}, &out); err != nil {
		return err
	}
	if !out.CommentCreate.Success {
		return errors.New("linear api did not create the comment")
	}
	return nil
}

// UpdateLinearIssueState moves an issue to a state.
func (a LinearAPI) UpdateLinearIssueState(ctx context.Context, issueID, stateID string) error {
	var out struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	vars := map[string]any{"id": issueID, "input": map[string]any{"stateId": stateID}}
	if err := a.graphql(ctx, linearWriteTimeout, linearIssueUpdateMutation, vars, &out); err != nil {
		return err
	}
	if !out.IssueUpdate.Success {
		return errors.New("linear api did not update the issue")
	}
	return nil
}

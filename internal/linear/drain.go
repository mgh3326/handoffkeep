package linear

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const (
	defaultDrainPollInterval = time.Second
	initialRetryBackoff      = time.Second
	maximumRetryBackoff      = 5 * time.Minute
	lockReleaseBudget        = 2 * time.Second
)

type Drain struct {
	Store        *store.Store
	Client       *Client
	PollInterval time.Duration
	Logger       *log.Logger
	Jitter       func(time.Duration) time.Duration
}

func (drain *Drain) logf(format string, values ...any) {
	if drain.Logger != nil {
		drain.Logger.Printf(format, values...)
	}
}

func waitFor(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (drain *Drain) Run(ctx context.Context) {
	if drain.Store == nil || drain.Client == nil {
		drain.logf("linear drain disabled: store or client is nil")
		return
	}
	poll := drain.PollInterval
	if poll <= 0 {
		poll = defaultDrainPollInterval
	}
	var lease *store.LinearDrainLease
	loggedUnavailable := false
	for lease == nil {
		candidate, acquired, err := drain.Store.TryLinearDrainLease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			drain.logf("linear drain advisory lock error: %v", err)
		} else if acquired {
			lease = candidate
			break
		} else if !loggedUnavailable {
			drain.logf("linear drain advisory lock is held by another instance")
			loggedUnavailable = true
		}
		if !waitFor(ctx, poll) {
			return
		}
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), lockReleaseBudget)
		defer cancel()
		if err := lease.Release(releaseCtx); err != nil {
			drain.logf("linear drain advisory unlock error: %v", err)
		}
	}()

	for {
		processed, err := drain.Store.ProcessNextLinearOutbox(ctx, drain.process)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			drain.logf("linear drain database error: %v", err)
			if !waitFor(ctx, poll) {
				return
			}
			continue
		}
		if processed {
			continue
		}
		if !waitFor(ctx, poll) {
			return
		}
	}
}

func (drain *Drain) retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := initialRetryBackoff
	for i := 1; i < attempt && delay < maximumRetryBackoff; i++ {
		delay *= 2
		if delay > maximumRetryBackoff {
			delay = maximumRetryBackoff
		}
	}
	if drain.Jitter != nil {
		return drain.Jitter(delay)
	}
	// Jitter is additive and bounded to 25%, keeping the documented cap.
	maxJitter := delay / 4
	if maxJitter > 0 {
		delay += time.Duration(rand.Int64N(int64(maxJitter) + 1))
	}
	if delay > maximumRetryBackoff {
		return maximumRetryBackoff
	}
	return delay
}

func (drain *Drain) failed(item store.LinearOutbox, err error) store.LinearOutboxResult {
	message := err.Error()
	if IsAmbiguous(err) {
		// The remote service may have accepted a mutation before its response was
		// lost. Keep the row pending; the next attempt re-reads the marker or
		// remote state before it considers sending the mutation again.
		message = "ambiguous outcome; verify remote state before retry: " + message
	}
	drain.logf("linear outbox task=%d seq=%d op=%s error=%s", item.TaskID, item.Seq, item.Op, message)
	if IsPermanent(err) {
		return store.LinearOutboxResult{State: "failed", LastError: message}
	}
	return store.LinearOutboxResult{
		State:     "pending",
		RetryAt:   time.Now().UTC().Add(drain.retryDelay(item.Attempts + 1)),
		LastError: message,
	}
}

func stateID(metadata TeamMetadata, name string) (string, error) {
	for _, state := range metadata.States.Nodes {
		if state.Name == name {
			return state.ID, nil
		}
	}
	return "", permanentError(OperationTeamLookup, "missing workflow state "+name)
}

func labelIDs(metadata TeamMetadata, requested []string) (ids, missing []string) {
	byName := map[string]string{}
	for _, label := range metadata.Labels.Nodes {
		byName[label.Name] = label.ID
	}
	seen := map[string]bool{}
	for _, name := range requested {
		if seen[name] {
			continue
		}
		seen[name] = true
		if id := byName[name]; id != "" {
			ids = append(ids, id)
		} else {
			missing = append(missing, name)
		}
	}
	sort.Strings(ids)
	sort.Strings(missing)
	return ids, missing
}

func markerForTask(taskID int64) string { return fmt.Sprintf("[hk-task:%d]", taskID) }

func markerForOutbox(item store.LinearOutbox) string {
	return fmt.Sprintf("[hk-task:%d seq:%d]", item.TaskID, item.Seq)
}

func (drain *Drain) issueForTask(ctx context.Context, taskID int64) (store.LinearIssue, error) {
	issue, found, err := drain.Store.GetLinearIssue(ctx, taskID)
	if err != nil {
		return store.LinearIssue{}, err
	}
	if !found {
		return store.LinearIssue{}, permanentError(OperationIssueGet, fmt.Sprintf("task %d has no Linear issue", taskID))
	}
	return issue, nil
}

func (drain *Drain) process(ctx context.Context, item store.LinearOutbox) (store.LinearOutboxResult, error) {
	switch item.Op {
	case store.LinearOpIssueCreate:
		return drain.processCreate(ctx, item), nil
	case store.LinearOpIssueState:
		return drain.processState(ctx, item), nil
	case store.LinearOpTerminalComment:
		return drain.processComment(ctx, item), nil
	case store.LinearOpTerminalArchive:
		return drain.processArchive(ctx, item), nil
	default:
		return store.LinearOutboxResult{}, fmt.Errorf("unsupported outbox operation %q", item.Op)
	}
}

func (drain *Drain) processCreate(ctx context.Context, item store.LinearOutbox) store.LinearOutboxResult {
	issue, found, err := drain.Client.SearchIssue(ctx, markerForTask(item.TaskID))
	if err != nil {
		return drain.failed(item, err)
	}
	if found {
		return store.LinearOutboxResult{State: "skipped", RemoteID: issue.ID, Identifier: issue.Identifier}
	}
	metadata, err := drain.Client.LookupTeam(ctx)
	if err != nil {
		return drain.failed(item, err)
	}
	state, err := stateID(metadata, item.Payload.State)
	if err != nil {
		return drain.failed(item, err)
	}
	labels, missing := labelIDs(metadata, item.Payload.Labels)
	issue, err = drain.Client.CreateIssue(ctx, item.Payload.Title, item.Payload.Description, state, labels)
	if err != nil {
		return drain.failed(item, err)
	}
	warning := ""
	if len(missing) > 0 {
		warning = "missing Linear labels (not created): " + strings.Join(missing, ", ")
		drain.logf("linear outbox task=%d seq=%d %s", item.TaskID, item.Seq, warning)
	}
	return store.LinearOutboxResult{State: "sent", LastError: warning, RemoteID: issue.ID, Identifier: issue.Identifier}
}

func (drain *Drain) processState(ctx context.Context, item store.LinearOutbox) store.LinearOutboxResult {
	linked, err := drain.issueForTask(ctx, item.TaskID)
	if err != nil {
		return drain.failed(item, err)
	}
	issue, err := drain.Client.GetIssue(ctx, linked.IssueID)
	if err != nil {
		return drain.failed(item, err)
	}
	if issue.State.Name == item.Payload.State {
		return store.LinearOutboxResult{State: "skipped"}
	}
	metadata, err := drain.Client.LookupTeam(ctx)
	if err != nil {
		return drain.failed(item, err)
	}
	state, err := stateID(metadata, item.Payload.State)
	if err != nil {
		return drain.failed(item, err)
	}
	if _, err = drain.Client.UpdateIssueState(ctx, linked.IssueID, state); err != nil {
		return drain.failed(item, err)
	}
	return store.LinearOutboxResult{State: "sent"}
}

func (drain *Drain) processComment(ctx context.Context, item store.LinearOutbox) store.LinearOutboxResult {
	linked, err := drain.issueForTask(ctx, item.TaskID)
	if err != nil {
		return drain.failed(item, err)
	}
	marker := markerForOutbox(item)
	comments, err := drain.Client.ListComments(ctx, linked.IssueID)
	if err != nil {
		return drain.failed(item, err)
	}
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			return store.LinearOutboxResult{State: "skipped"}
		}
	}
	body := item.Payload.Comment
	if !strings.Contains(body, marker) {
		body = strings.TrimRight(body, "\n") + "\n\n" + marker
	}
	if _, err = drain.Client.CreateComment(ctx, linked.IssueID, body); err != nil {
		return drain.failed(item, err)
	}
	return store.LinearOutboxResult{State: "sent"}
}

func (drain *Drain) processArchive(ctx context.Context, item store.LinearOutbox) store.LinearOutboxResult {
	linked, err := drain.issueForTask(ctx, item.TaskID)
	if err != nil {
		return drain.failed(item, err)
	}
	issue, err := drain.Client.GetIssue(ctx, linked.IssueID)
	if err != nil {
		return drain.failed(item, err)
	}
	if issue.ArchivedAt != nil {
		return store.LinearOutboxResult{State: "skipped"}
	}
	if err = drain.Client.ArchiveIssue(ctx, linked.IssueID); err != nil {
		return drain.failed(item, err)
	}
	return store.LinearOutboxResult{State: "sent"}
}

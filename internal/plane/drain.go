package plane

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

// ProjectMapping binds hk project names to Plane project identifiers. The
// writer resolves the hk name to an identifier here and the identifier to a
// UUID through the client; a task whose project is unmapped fails closed
// unless Default names a catch-all Plane project.
type ProjectMapping struct {
	// Map holds hk project name -> Plane project identifier (the short key).
	Map map[string]string
	// Default is the Plane project identifier for tasks with no hk project
	// or one outside Map. Empty means unmapped work fails closed instead of
	// guessing a home for it.
	Default string
}

// ParseProjectMapping reads the HK_PLANE_PROJECT_MAP form
// "hkName=IDENT,other=IDENT2". Malformed entries are rejected — a partially
// applied map would strand work items under an operator-meaningless project.
func ParseProjectMapping(raw, defaultIdentifier string) (ProjectMapping, error) {
	mapping := ProjectMapping{Map: map[string]string{}, Default: strings.TrimSpace(defaultIdentifier)}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, identifier, found := strings.Cut(entry, "=")
		name, identifier = strings.TrimSpace(name), strings.TrimSpace(identifier)
		if !found || name == "" || identifier == "" || strings.ContainsAny(name+identifier, " \t\n") {
			return ProjectMapping{}, fmt.Errorf("invalid plane project map entry %q (want hkName=IDENT)", entry)
		}
		if _, dup := mapping.Map[name]; dup {
			return ProjectMapping{}, fmt.Errorf("duplicate plane project map entry for %q", name)
		}
		mapping.Map[name] = identifier
	}
	return mapping, nil
}

// IdentifierFor resolves an hk project name ("" for unset) to the configured
// Plane project identifier, failing closed when nothing covers it.
func (m ProjectMapping) IdentifierFor(hkProject string) (string, error) {
	if identifier, ok := m.Map[hkProject]; ok && hkProject != "" {
		return identifier, nil
	}
	if m.Default != "" {
		return m.Default, nil
	}
	if hkProject == "" {
		return "", permanentError(OperationProjectList, "task has no hk project and no default Plane project is configured")
	}
	return "", permanentError(OperationProjectList, "hk project "+hkProject+" is not in the Plane project map")
}

type Drain struct {
	Store        *store.Store
	Client       *Client
	Projects     ProjectMapping
	PollInterval time.Duration
	Logger       *log.Logger
	Jitter       func(time.Duration) time.Duration
	// DryRun is the default pilot posture: each pending op is printed with
	// the exact request it would send, then marked 'dryrun' — a terminal
	// outbox state — so the queue drains without the client being touched.
	// Live writes require DryRun=false plus an API key.
	DryRun bool
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
	if drain.Store == nil || (!drain.DryRun && drain.Client == nil) {
		drain.logf("plane drain disabled: store is nil, or live mode lacks a client")
		return
	}
	poll := drain.PollInterval
	if poll <= 0 {
		poll = defaultDrainPollInterval
	}
	var lease *store.PlaneDrainLease
	loggedUnavailable := false
	for lease == nil {
		candidate, acquired, err := drain.Store.TryPlaneDrainLease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			drain.logf("plane drain advisory lock error: %v", err)
		} else if acquired {
			lease = candidate
			break
		} else if !loggedUnavailable {
			drain.logf("plane drain advisory lock is held by another instance")
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
			drain.logf("plane drain advisory unlock error: %v", err)
		}
	}()

	for {
		processed, err := drain.Store.ProcessNextPlaneOutbox(ctx, drain.process)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			drain.logf("plane drain database error: %v", err)
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
	maxJitter := delay / 4
	if maxJitter > 0 {
		delay += time.Duration(rand.Int64N(int64(maxJitter) + 1))
	}
	if delay > maximumRetryBackoff {
		return maximumRetryBackoff
	}
	return delay
}

func (drain *Drain) failed(item store.PlaneOutbox, err error) store.PlaneOutboxResult {
	message := err.Error()
	if IsAmbiguous(err) {
		// The remote service may have accepted a mutation before its response
		// was lost. Keep the row pending; the next attempt re-reads the
		// marker or remote state before it considers sending the mutation
		// again.
		message = "ambiguous outcome; verify remote state before retry: " + message
	}
	drain.logf("plane outbox task=%d seq=%d op=%s error=%s", item.TaskID, item.Seq, item.Op, message)
	if IsPermanent(err) {
		return store.PlaneOutboxResult{State: "failed", LastError: message}
	}
	return store.PlaneOutboxResult{
		State:     "pending",
		RetryAt:   time.Now().UTC().Add(drain.retryDelay(item.Attempts + 1)),
		LastError: message,
	}
}

func markerForTask(taskID int64) string { return store.PlaneExternalID(taskID) }

// descriptionHTML is the only remote body the mirror writes. It carries the
// adoption marker and the one-way notice — never task content.
func descriptionHTML(taskID int64) string {
	return fmt.Sprintf("<p>Mirrored read-only from hk; hk is the source of truth. Reference: %s</p>", markerForTask(taskID))
}

func planeLabelNames(payload store.PlaneOutboxPayload) []string {
	return []string{"lane:" + payload.Lane, "kind:" + payload.Kind}
}

func stateIDFor(states []State, name string) (string, error) {
	for _, state := range states {
		if state.Name == name {
			return state.ID, nil
		}
	}
	return "", permanentError(OperationStateList, "missing Plane workflow state "+name)
}

func labelIDsFor(labels []Label, requested []string) (ids, missing []string) {
	byName := map[string]string{}
	for _, label := range labels {
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

// describeOp renders the exact remote call an op would make. Dry-run output
// shows this string; it only ever contains allowlist fields.
func describeOp(item store.PlaneOutbox, projectIdentifier string) string {
	payload := item.Payload
	target := "create work item"
	if item.Op == store.PlaneOpWorkItemUpdate {
		target = "update work item"
	}
	state := payload.State
	if state == "" {
		state = "(keep remote state)"
	}
	project := payload.Project
	if project == "" {
		project = "(unset)"
	}
	return fmt.Sprintf("%s %s in plane-project=%s name=%q state=%s lane=%s kind=%s priority=%s project=%s",
		target, payload.ExternalID, projectIdentifier, payload.Name, state, payload.Lane, payload.Kind, payload.Priority, project)
}

func (drain *Drain) process(ctx context.Context, item store.PlaneOutbox) (store.PlaneOutboxResult, error) {
	switch item.Op {
	case store.PlaneOpWorkItemCreate, store.PlaneOpWorkItemUpdate:
		return drain.processMirror(ctx, item), nil
	case store.PlaneOpWorkItemRemove:
		return drain.processRemove(ctx, item), nil
	default:
		return store.PlaneOutboxResult{}, fmt.Errorf("unsupported plane outbox operation %q", item.Op)
	}
}

// processRemove takes a terminal task out of the projection. The link row is
// dropped in the same outbox commit, so a work item deleted here is never
// reprojected — terminal tasks have no out-edges in hk either.
func (drain *Drain) processRemove(ctx context.Context, item store.PlaneOutbox) store.PlaneOutboxResult {
	if drain.DryRun {
		drain.logf("plane dry-run task=%d seq=%d op=%s would delete work item for %s", item.TaskID, item.Seq, item.Op, item.Payload.ExternalID)
		return store.PlaneOutboxResult{State: "dryrun"}
	}
	linked, found, err := drain.Store.GetPlaneIssue(ctx, item.TaskID)
	if err != nil {
		return drain.failed(item, err)
	}
	if !found {
		// No link row: either the item never left dry-run or its project was
		// unmapped at the time. Confirm absence by marker scan only when the
		// task's project resolves — otherwise skip with the reason recorded.
		identifier, mapErr := drain.Projects.IdentifierFor(item.Payload.Project)
		if mapErr != nil {
			return store.PlaneOutboxResult{State: "skipped", LastError: "no link and project unmapped; assumed absent", Unlink: true}
		}
		projectID, err := drain.Client.ProjectIDFor(ctx, identifier)
		if err != nil {
			return drain.failed(item, err)
		}
		remote, remoteFound, err := drain.Client.FindIssueByMarker(ctx, projectID, markerForTask(item.TaskID))
		if err != nil {
			return drain.failed(item, err)
		}
		if !remoteFound {
			return store.PlaneOutboxResult{State: "skipped", Unlink: true}
		}
		linked = store.PlaneIssue{ProjectID: remote.Project, WorkItemID: remote.ID}
	}
	if err := drain.Client.DeleteIssue(ctx, linked.ProjectID, linked.WorkItemID); err != nil {
		return drain.failed(item, err)
	}
	return store.PlaneOutboxResult{State: "sent", Unlink: true}
}

// dryRunResult prints the planned write and marks the row 'dryrun'. The
// client is never consulted, so a dry-run drain cannot issue a remote write
// even accidentally.
func (drain *Drain) dryRunResult(item store.PlaneOutbox) store.PlaneOutboxResult {
	identifier, err := drain.Projects.IdentifierFor(item.Payload.Project)
	note := ""
	if err != nil {
		note = " (project resolution: " + err.Error() + ")"
	}
	drain.logf("plane dry-run task=%d seq=%d op=%s would %s%s", item.TaskID, item.Seq, item.Op, describeOp(item, identifier), note)
	return store.PlaneOutboxResult{State: "dryrun"}
}

func (drain *Drain) processMirror(ctx context.Context, item store.PlaneOutbox) store.PlaneOutboxResult {
	if drain.DryRun {
		return drain.dryRunResult(item)
	}
	payload := item.Payload
	identifier, err := drain.Projects.IdentifierFor(payload.Project)
	if err != nil {
		return drain.failed(item, err)
	}
	projectID, err := drain.Client.ProjectIDFor(ctx, identifier)
	if err != nil {
		return drain.failed(item, err)
	}
	linked, found, err := drain.Store.GetPlaneIssue(ctx, item.TaskID)
	if err != nil {
		return drain.failed(item, err)
	}
	var remote Issue
	remoteFound := false
	if found {
		remote, remoteFound, err = drain.Client.GetIssue(ctx, linked.ProjectID, linked.WorkItemID)
		if err != nil {
			return drain.failed(item, err)
		}
	}
	if !remoteFound {
		remote, remoteFound, err = drain.Client.FindIssueByMarker(ctx, projectID, markerForTask(item.TaskID))
		if err != nil {
			return drain.failed(item, err)
		}
	}
	if !remoteFound {
		return drain.createRemote(ctx, item, projectID)
	}
	if remote.Project != "" && remote.Project != projectID {
		// The task moved hk projects. Plane cannot re-parent a work item
		// through the public API, so hk wins by replacement: delete the
		// stale item, then recreate under the mapped project — retries of
		// a half-done move cannot accumulate duplicates.
		return drain.moveRemote(ctx, item, remote, projectID)
	}
	return drain.updateRemote(ctx, item, remote, projectID)
}

func (drain *Drain) remoteLabels(ctx context.Context, item store.PlaneOutbox, projectID string) ([]string, string, error) {
	labels, err := drain.Client.ListLabels(ctx, projectID)
	if err != nil {
		return nil, "", err
	}
	ids, missing := labelIDsFor(labels, planeLabelNames(item.Payload))
	warning := ""
	if len(missing) > 0 {
		warning = "missing Plane labels (not created): " + strings.Join(missing, ", ")
		drain.logf("plane outbox task=%d seq=%d %s", item.TaskID, item.Seq, warning)
	}
	return ids, warning, nil
}

func (drain *Drain) desiredStateID(ctx context.Context, item store.PlaneOutbox, projectID string) (string, error) {
	if item.Payload.State == "" {
		return "", nil
	}
	states, err := drain.Client.ListStates(ctx, projectID)
	if err != nil {
		return "", err
	}
	return stateIDFor(states, item.Payload.State)
}

func (drain *Drain) createRemote(ctx context.Context, item store.PlaneOutbox, projectID string) store.PlaneOutboxResult {
	stateID, err := drain.desiredStateID(ctx, item, projectID)
	if err != nil {
		return drain.failed(item, err)
	}
	labelIDs, warning, err := drain.remoteLabels(ctx, item, projectID)
	if err != nil {
		return drain.failed(item, err)
	}
	issue, err := drain.Client.CreateIssue(ctx, projectID, IssueInput{
		Name:            item.Payload.Name,
		DescriptionHTML: descriptionHTML(item.TaskID),
		State:           stateID,
		Priority:        item.Payload.Priority,
		Labels:          labelIDs,
	})
	if err != nil {
		return drain.failed(item, err)
	}
	return store.PlaneOutboxResult{State: "sent", LastError: warning, RemoteID: issue.ID, ProjectID: projectID}
}

// moveRemote replaces a work item that lives in the wrong Plane project.
// Order matters: the stale item is deleted before the replacement is
// created so a retried move never stacks duplicates. If the create fails
// after the delete landed, the next pass sees the linked id as missing and
// converges through the marker scan into a fresh create — a temporary gap
// in the mirror self-heals, while a stray duplicate in the old project
// would drift forever.
func (drain *Drain) moveRemote(ctx context.Context, item store.PlaneOutbox, remote Issue, projectID string) store.PlaneOutboxResult {
	if err := drain.Client.DeleteIssue(ctx, remote.Project, remote.ID); err != nil {
		return drain.failed(item, err)
	}
	return drain.createRemote(ctx, item, projectID)
}

// updateRemote writes the hk snapshot over the remote work item — hk wins on
// every projected field. Already-matching fields are not sent, so steady
// state costs one read and zero writes.
func (drain *Drain) updateRemote(ctx context.Context, item store.PlaneOutbox, remote Issue, projectID string) store.PlaneOutboxResult {
	payload := item.Payload
	patch := IssuePatch{}
	if remote.Name != payload.Name {
		patch.Name = payload.Name
	}
	if payload.Priority != "" && remote.Priority != payload.Priority {
		patch.Priority = payload.Priority
	}
	stateID, err := drain.desiredStateID(ctx, item, projectID)
	if err != nil {
		return drain.failed(item, err)
	}
	if stateID != "" && remote.State != stateID {
		patch.State = stateID
	}
	labelIDs, warning, err := drain.remoteLabels(ctx, item, projectID)
	if err != nil {
		return drain.failed(item, err)
	}
	sort.Strings(labelIDs)
	actual := append([]string{}, remote.Labels...)
	sort.Strings(actual)
	if strings.Join(labelIDs, ",") != strings.Join(actual, ",") {
		patch.Labels = labelIDs
	}
	if patch.Name == "" && patch.State == "" && patch.Priority == "" && patch.Labels == nil {
		return store.PlaneOutboxResult{State: "skipped", RemoteID: remote.ID, ProjectID: projectID, LastError: warning}
	}
	if _, err = drain.Client.UpdateIssue(ctx, projectID, remote.ID, patch); err != nil {
		return drain.failed(item, err)
	}
	return store.PlaneOutboxResult{State: "sent", RemoteID: remote.ID, ProjectID: projectID, LastError: warning}
}

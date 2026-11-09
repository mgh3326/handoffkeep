// Package api is the one application core used by HTTP, CLI proxies, and MCP.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

type Service struct {
	Store       *store.Store
	Attachments *attachments.Manager
	// Dots backs the /v1/dots/results routes; nil uses Store.
	Dots DotsResultStore
	// YouTrack overlays the YouTrack-owned priority and lane on linked tasks
	// and writes those changes through YouTrack first. Nil disables the
	// feature; every other route behaves exactly as before.
	YouTrack *youtrack.Service
	// SearchBackend is "youtrack" to answer /v1/search from YouTrack, with
	// PostgreSQL as the fallback. Anything else, including "", is PostgreSQL.
	SearchBackend string
}

// DotsResultStore lists dots results and records their adoption.
type DotsResultStore interface {
	ListDotsResults(ctx context.Context, taskID int64, issue string, limit int) ([]store.DotsResult, error)
	SetDotsAdoption(ctx context.Context, id int64, by string, in store.DotsAdoptionInput) (store.DotsResult, bool, error)
}

// overlayTasks replaces priority and lane on linked tasks with the cached
// YouTrack values. Unlinked tasks keep their hk columns unchanged.
func (s Service) overlayTasks(ctx context.Context, tasks []store.Task) {
	if s.YouTrack != nil {
		s.YouTrack.Overlay(ctx, tasks)
	}
}

func (s Service) dotsResults() DotsResultStore {
	if s.Dots != nil {
		return s.Dots
	}
	return s.Store
}

var decisionResolverRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// DecisionResolveInput records an answer made outside the web console and
// closes the matching open decision. It is shared by the HTTP route and the
// authenticated remote client payload.
type DecisionResolveInput struct {
	Type     string `json:"type"`
	ID       int64  `json:"id"`
	By       string `json:"by"`
	Answer   string `json:"answer"`
	Note     string `json:"note,omitempty"`
	NoInject bool   `json:"no_inject,omitempty"`
	// NoJob is the caller-stated reason recorded when the resolve lands a
	// task without claimed_by + refs.job_id back in claimed.
	NoJob string `json:"no_job,omitempty"`
}

func validDecisionResolveText(value string, required bool) bool {
	if required && strings.TrimSpace(value) == "" {
		return false
	}
	if value == "" {
		return true
	}
	if len(value) > store.RelayLaneEventMaxBytes {
		return false
	}
	for _, r := range value {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}

func decisionEventID(prefix string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// ResolveDecision writes a lane event before transitioning a task, preserving
// the same durable ordering as the web decision-answer route.
func (s Service) ResolveDecision(ctx context.Context, input DecisionResolveInput) (store.RelayEvent, error) {
	if (input.Type != "task" && input.Type != "escalation" && input.Type != "lane") || input.ID < 1 || !decisionResolverRE.MatchString(input.By) || !validDecisionResolveText(input.Answer, true) || !validDecisionResolveText(input.Note, false) || !validDecisionResolveText(input.NoJob, false) {
		return store.RelayEvent{}, errors.New("invalid decision resolve")
	}

	lane := ""
	if input.Type == "task" {
		if err := s.wrkTaskPrecheck(ctx, input.ID, "claimed", false); err != nil {
			return store.RelayEvent{}, err
		}
		task, found, err := s.Store.GetTask(ctx, input.ID)
		if err != nil {
			return store.RelayEvent{}, err
		}
		if !found {
			return store.RelayEvent{}, store.ErrTaskNotFound
		}
		// Disposition items are answered only by the operator's Access-authenticated
		// web route; a bearer token identifies a machine, not the operator, and
		// By is a free string. Refuse before any lane event is written.
		if task.Refs.Disposition != nil {
			return store.RelayEvent{}, store.ErrDispositionOperatorOnly
		}
		if !task.AwaitingDecision {
			return store.RelayEvent{}, store.ErrTaskConflict
		}
		// An open structured request is closed with tasks decision-resolve,
		// which records the answer on that request_id. Resolving it here
		// would unblock the task and leave the request open.
		if task.Refs.DecisionRequest != nil && task.Refs.DecisionRequest.Status == store.DecisionRequestOpen {
			return store.RelayEvent{}, store.ErrDecisionRequestOpen
		}
		// Validate the no-job reason before the lane event below commits:
		// a reason the transition would reject (secret-looking content)
		// must not leave a resolved answer queued behind a refused task.
		if _, err = store.CheckTaskNoJob(input.NoJob); err != nil {
			return store.RelayEvent{}, err
		}
		lane = task.Lane
	} else {
		var events []store.RelayEvent
		var err error
		if input.Type == "escalation" {
			events, err = s.Store.ListOpenEscalations(ctx, 1000)
		} else {
			events, err = s.Store.ListOpenLaneDecisions(ctx, 1000)
		}
		if err != nil {
			return store.RelayEvent{}, err
		}
		for _, event := range events {
			if event.ID == input.ID {
				lane = event.OwnerLane
				break
			}
		}
		if lane == "" {
			return store.RelayEvent{}, store.ErrTaskConflict
		}
	}

	prefix := "[decision]"
	if input.Type == "lane" {
		prefix = "[decision-answered]"
	}
	text := fmt.Sprintf("%s #%d: %s (from %s)", prefix, input.ID, input.Answer, input.By)
	if input.Note != "" {
		text += " — " + input.Note
	}
	if len([]byte(text)) > store.RelayLaneEventMaxBytes || !validDecisionResolveText(text, true) {
		return store.RelayEvent{}, errors.New("decision response is too long")
	}
	eventID, err := decisionEventID(fmt.Sprintf("cli-decision-%s-%d-", input.Type, input.ID))
	if err != nil {
		return store.RelayEvent{}, err
	}
	event, _, err := s.Store.AppendRelayEvent(ctx, store.RelayEvent{Kind: "lane.event", OwnerLane: lane, EventID: eventID, Text: text})
	if err != nil {
		return store.RelayEvent{}, err
	}
	if input.NoInject {
		event, err = s.Store.MarkRelayEventDelivered(ctx, event.ID, "resolve", input.By)
		if err != nil {
			return store.RelayEvent{}, err
		}
	}
	if input.Type == "task" {
		note := input.Answer
		if input.Note != "" {
			note += " — " + input.Note
		}
		note += " — resolved by " + input.By
		// The answer clears the awaiting_decision flag and keeps the work
		// state, so resuming enters no started state and needs no job link.
		// noJob was already validated before the lane event was written.
		if _, err := s.Store.ClearAwaitingDecision(ctx, input.ID, input.By, note); err != nil {
			return store.RelayEvent{}, err
		}
	}
	return event, nil
}

func (s Service) Checkpoint(ctx context.Context, client string, x store.Checkpoint) (store.Checkpoint, error) {
	x.CreatedBy = client
	return s.Store.CreateCheckpoint(ctx, x)
}
func (s Service) Recent(ctx context.Context, session, kind string, limit int) ([]store.Checkpoint, error) {
	return s.Store.RecentCheckpoints(ctx, session, kind, limit)
}
func (s Service) PutMemory(ctx context.Context, client string, x store.Memory) (store.Memory, error) {
	x.UpdatedBy = client
	return s.Store.PutMemory(ctx, x)
}
func (s Service) GetMemory(ctx context.Context, agent, name string) (store.Memory, bool, error) {
	return s.Store.GetMemory(ctx, agent, name)
}
func (s Service) ListMemory(ctx context.Context, agent string, content bool) ([]store.Memory, error) {
	return s.Store.ListMemory(ctx, agent, content)
}
func (s Service) PutDocument(ctx context.Context, client string, x store.Document) (store.Document, bool, error) {
	x.CreatedBy = client
	return s.Store.PutDocument(ctx, x)
}
func (s Service) GetDocument(ctx context.Context, key string) (store.Document, bool, error) {
	return s.Store.GetDocument(ctx, key)
}
func (s Service) GetDocumentByID(ctx context.Context, id int64) (store.Document, bool, error) {
	return s.Store.GetDocumentByID(ctx, id)
}
func (s Service) ListDocuments(ctx context.Context, prefix, kind, session string, task int64, limit int) ([]store.Document, error) {
	return s.Store.ListDocuments(ctx, prefix, kind, session, task, limit)
}
func (s Service) Search(ctx context.Context, q, scope, session string, limit int) ([]store.SearchResult, error) {
	if s.SearchBackend == youtrackSearchBackend {
		if s.YouTrack == nil {
			log.Printf("search.youtrack_fallback reason=config_missing scope=%q", scope)
			return s.Store.Search(ctx, q, scope, session, limit)
		}
		return s.YouTrack.Search(ctx, s.Store.Search, q, scope, session, limit)
	}
	return s.Store.Search(ctx, q, scope, session, limit)
}

// youtrackSearchBackend is the HANDOFFKEEP_SEARCH_BACKEND value that selects
// the YouTrack search backend.
const youtrackSearchBackend = "youtrack"

func (s Service) PutAttachment(ctx context.Context, client, name, mime, ref string, body []byte) (store.Attachment, bool, error) {
	if s.Attachments == nil {
		return store.Attachment{}, false, attachments.ErrDisabled
	}
	return s.Attachments.Put(ctx, client, name, mime, ref, body)
}
func (s Service) GetAttachment(ctx context.Context, sha string) (store.Attachment, io.ReadCloser, error) {
	if s.Attachments == nil {
		return store.Attachment{}, nil, attachments.ErrDisabled
	}
	return s.Attachments.Get(ctx, sha)
}
func (s Service) AttachmentURL(ctx context.Context, sha string) (string, error) {
	if s.Attachments == nil {
		return "", attachments.ErrDisabled
	}
	return s.Attachments.Presign(ctx, sha)
}
func (s Service) ListAttachments(ctx context.Context, ref string, limit int) ([]store.Attachment, error) {
	if s.Attachments == nil {
		return nil, attachments.ErrDisabled
	}
	return s.Attachments.List(ctx, ref, limit)
}
func (s Service) AttachmentUsage(ctx context.Context) (store.AttachmentUsage, error) {
	if s.Attachments == nil {
		return store.AttachmentUsage{}, attachments.ErrDisabled
	}
	return s.Attachments.Usage(ctx)
}
func (s Service) CreateTask(ctx context.Context, client string, x store.Task, check store.DuplicateCheck) (store.Task, error) {
	x.CreatedBy = client
	return s.Store.CreateTaskChecked(ctx, x, check)
}
func (s Service) ClaimTask(ctx context.Context, id int64, by, jobID, noJob string) (store.Task, error) {
	if err := s.wrkTaskPrecheck(ctx, id, "", true); err != nil {
		return store.Task{}, err
	}
	return s.Store.ClaimTask(ctx, id, by, jobID, noJob)
}

func (s Service) RebindTaskJob(ctx context.Context, id int64, fromJob, toJob, by, reason string) (store.Task, error) {
	if err := s.wrkTaskPrecheck(ctx, id, "", true); err != nil {
		return store.Task{}, err
	}
	return s.Store.RebindTaskJob(ctx, id, fromJob, toJob, by, reason)
}

func (s Service) NextTask(ctx context.Context, lane, by, jobID, noJob string) (store.Task, error) {
	if err := s.Store.WrkFence(ctx, wrkrecords.LegacyIntent{JobID: jobID, Binding: true}); err != nil {
		return store.Task{}, err
	}
	return s.Store.NextTask(ctx, lane, by, jobID, noJob)
}
func (s Service) CreateDisposition(ctx context.Context, client string, x store.DispositionInput) (store.Task, bool, error) {
	x.CreatedBy = client
	return s.Store.CreateDisposition(ctx, x)
}
func (s Service) DispositionSummary(ctx context.Context, asOf time.Time) (store.DispositionSummary, error) {
	return s.Store.DispositionSummary(ctx, asOf)
}
func (s Service) ApplyDisposition(ctx context.Context, id int64, client, note string) (store.Task, error) {
	task, _, err := s.Store.GetTask(ctx, id)
	if err != nil {
		return store.Task{}, err
	}
	if err := s.wrkTaskPrecheck(ctx, id, store.DispositionTarget(task), false); err != nil {
		return store.Task{}, err
	}
	return s.Store.ApplyDisposition(ctx, id, client, note)
}
func (s Service) ReopenTask(ctx context.Context, id int64, client, reason string) (store.Task, error) {
	if err := s.wrkTaskPrecheck(ctx, id, "reopened", true); err != nil {
		return store.Task{}, err
	}
	return s.Store.ReopenTask(ctx, id, client, reason)
}
func (s Service) TransitionTask(ctx context.Context, id int64, to, client, note string, refs *store.TaskRefs, noJob string) (store.Task, error) {
	return s.TransitionTaskWith(ctx, id, client, store.TransitionInput{To: to, Note: note, Refs: refs, NoJob: noJob})
}

// TransitionTaskWith is TransitionTask with the options a plain call cannot
// carry, such as the duplicate_of target.
func (s Service) TransitionTaskWith(ctx context.Context, id int64, client string, in store.TransitionInput) (store.Task, error) {
	if err := s.wrkTaskPrecheck(ctx, id, store.FenceTarget(in.To), in.Refs != nil); err != nil {
		return store.Task{}, err
	}
	in.By = client
	return s.Store.TransitionTaskWith(ctx, id, in)
}

// SetTaskParent sets or clears a task's parent.
func (s Service) SetTaskParent(ctx context.Context, id int64, parent *int64, client string) (store.Task, bool, error) {
	if err := s.wrkTaskPrecheck(ctx, id, "", true); err != nil {
		return store.Task{}, false, err
	}
	return s.Store.SetTaskParent(ctx, id, parent, client)
}

// AddTaskRelation adds an edge from the task to another.
func (s Service) AddTaskRelation(ctx context.Context, id int64, kind string, other int64, client string) (bool, error) {
	if err := s.wrkTaskPrecheck(ctx, id, "", true); err != nil {
		return false, err
	}
	return s.Store.AddTaskRelation(ctx, id, kind, other, client)
}

// RemoveTaskRelation archives an edge.
func (s Service) RemoveTaskRelation(ctx context.Context, id int64, kind string, other int64, client string) (bool, error) {
	if err := s.wrkTaskPrecheck(ctx, id, "", true); err != nil {
		return false, err
	}
	return s.Store.RemoveTaskRelation(ctx, id, kind, other, client)
}
func (s Service) RelaneTask(ctx context.Context, id int64, to, client, note string, allowNewLane bool) (store.Task, bool, error) {
	if err := s.wrkTaskPrecheck(ctx, id, "", true); err != nil {
		return store.Task{}, false, err
	}
	// PG is the execution value. The YouTrack export is queued after the
	// write, so a YouTrack outage never blocks the move.
	task, changed, err := s.Store.RelaneTask(ctx, id, to, client, note, allowNewLane)
	if err != nil {
		return store.Task{}, false, err
	}
	if s.YouTrack != nil && changed {
		s.YouTrack.RecordLane(ctx, id, to)
	}
	return task, changed, nil
}
func (s Service) SetTaskProject(ctx context.Context, id int64, project, client, note string) (store.Task, bool, error) {
	if err := s.wrkTaskPrecheck(ctx, id, "", true); err != nil {
		return store.Task{}, false, err
	}
	return s.Store.SetTaskProject(ctx, id, project, client, note)
}
func (s Service) SetTaskArea(ctx context.Context, id int64, area *string, client, note string) (store.Task, bool, error) {
	return s.Store.SetTaskArea(ctx, id, area, client, note)
}
func (s Service) ListTaskProjects(ctx context.Context) ([]string, error) {
	return s.Store.ListTaskProjects(ctx)
}
func (s Service) AddTaskProject(ctx context.Context, client, name string) (bool, error) {
	return s.Store.AddTaskProject(ctx, name, client)
}
func (s Service) ListTasks(ctx context.Context, lane, state, parentLane string, project, area *string, limit int) ([]store.Task, error) {
	return s.Store.ListTasks(ctx, lane, state, parentLane, project, area, limit)
}
func (s Service) ListTasksPage(ctx context.Context, lane, state, parentLane string, project, area *string, afterID int64, limit int) ([]store.Task, error) {
	return s.Store.ListTasksPage(ctx, lane, state, parentLane, project, area, afterID, limit)
}

// ExportTasks returns a consistent task snapshot and stamps the serving
// build's VCS revision into the source object. The revision is the binary's
// embedded stamp; "unknown" is emitted rather than invented when it is absent.
func (s Service) ExportTasks(ctx context.Context, lane, state, parentLane string, project *string, limit int) (store.TaskExport, error) {
	out, err := s.Store.ExportTasks(ctx, lane, state, parentLane, project, limit)
	if err != nil {
		return out, err
	}
	out.Source = store.TaskExportSource{VCSRevision: "unknown"}
	if info, ok := readBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				out.Source.VCSRevision = setting.Value
			}
		}
	}
	return out, nil
}
func (s Service) GetTask(ctx context.Context, id int64) (store.Task, bool, error) {
	return s.Store.GetTask(ctx, id)
}
func (s Service) AppendRelayEvent(ctx context.Context, x store.RelayEvent) (store.RelayEvent, bool, error) {
	if err := s.Store.WrkFence(ctx, wrkrecords.LegacyIntent{JobID: x.JobID, Kind: x.Kind, Head: x.Head, Digest: x.ReportSHA, Receipt: x.WrkReceipt}); err != nil {
		return store.RelayEvent{}, false, err
	}

	return s.Store.AppendRelayEvent(ctx, x)
}
func (s Service) MarkRelayEventDelivered(ctx context.Context, id int64, machine, pane string) (store.RelayEvent, error) {
	return s.Store.MarkRelayEventDelivered(ctx, id, machine, pane)
}
func (s Service) ListRelayEvents(ctx context.Context, lane, kind string, undelivered bool, afterID int64, limit int) ([]store.RelayEvent, error) {
	return s.Store.ListRelayEventsPage(ctx, lane, kind, undelivered, afterID, limit)
}
func (s Service) ListBenchScores(ctx context.Context, modelID, source string, limit int) ([]store.BenchScore, error) {
	return s.Store.ListBenchScores(ctx, modelID, source, limit)
}
func (s Service) UpsertBenchScores(ctx context.Context, client string, xs []store.BenchScore) (int, error) {
	for i := range xs {
		xs[i].UpdatedBy = client
	}
	return s.Store.UpsertBenchScores(ctx, xs)
}
func (s Service) ListBenchReps(ctx context.Context, profile, grade, effort string, limit int) ([]store.BenchRep, error) {
	return s.Store.ListBenchReps(ctx, profile, grade, effort, limit)
}
func (s Service) UpsertBenchReps(ctx context.Context, client string, xs []store.BenchRep) ([]int64, error) {
	for i := range xs {
		xs[i].CreatedBy = client
	}
	return s.Store.UpsertBenchReps(ctx, xs)
}
func (s Service) PatchBenchRepOutcome(ctx context.Context, id int64, p store.BenchRepOutcomePatch) (store.BenchRep, error) {
	return s.Store.PatchBenchRepOutcome(ctx, id, p)
}
func (s Service) ListBenchGrades(ctx context.Context) ([]store.BenchGrade, error) {
	return s.Store.ListBenchGrades(ctx)
}
func (s Service) UpsertBenchGrades(ctx context.Context, client string, xs []store.BenchGrade) (int, error) {
	for i := range xs {
		xs[i].DecidedBy = client
	}
	return s.Store.UpsertBenchGrades(ctx, xs)
}
func (s Service) ListBenchCatalog(ctx context.Context, pool string, includeRetired bool) ([]store.BenchCatalogEntry, error) {
	return s.Store.ListBenchCatalog(ctx, pool, includeRetired)
}

// UpsertBenchCatalog deliberately does not overwrite DecidedBy with the
// authenticated client: the field records which decision produced the row,
// and the caller is always the operator anyway.
func (s Service) UpsertBenchCatalog(ctx context.Context, xs []store.BenchCatalogEntry) (int, error) {
	return s.Store.UpsertBenchCatalog(ctx, xs)
}
func (s Service) ListBenchAreaExceptions(ctx context.Context, area string, includeRetired bool) ([]store.BenchAreaException, error) {
	return s.Store.ListBenchAreaExceptions(ctx, area, includeRetired)
}

// UpsertBenchAreaExceptions stamps decided_by with the authenticated client
// and decided_at with the server clock — unlike the catalog's caller-carried
// provenance, an exception row's writer is always the operator client.
func (s Service) UpsertBenchAreaExceptions(ctx context.Context, client string, xs []store.BenchAreaException) (int, error) {
	now := time.Now().UTC()
	for i := range xs {
		xs[i].DecidedBy = client
		xs[i].DecidedAt = now
	}
	return s.Store.UpsertBenchAreaExceptions(ctx, xs)
}

type Tokens map[string]string

// QuotaAccounts maps an authenticated client id to the quota account keys it
// may write: publish observations, lease and release. A client absent from
// the map, or a key absent from its scope, is refused with 403. The mapping
// lives in the server auth file: optional HANDOFFKEEP_QUOTA_<CLIENT> lines
// hold a comma-separated grant list next to the client's
// HANDOFFKEEP_TOKEN_<CLIENT> credential. Each entry is an exact account key
// ("pool:fingerprint"), a bare pool name granting every account key of that
// pool, or "*" granting every account key. Reads are not scoped by it.
type QuotaAccounts map[string]map[string]bool

func LoadTokens(path string) (Tokens, QuotaAccounts, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	out := Tokens{}
	grants := QuotaAccounts{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if id, isQuota := strings.CutPrefix(k, "HANDOFFKEEP_QUOTA_"); isQuota {
			// A bad quota line is loud but never fatal: an auth-file typo
			// must not stop the server. Token lines stay strict. This runs
			// before the generic guard so an empty value or a missing '='
			// is also a skipped grant, not a startup failure.
			var keys []string
			valid := ok && v != "" && id != ""
			for _, key := range strings.Split(v, ",") {
				if !valid {
					break
				}
				key = strings.TrimSpace(key)
				if key != "*" && !store.ValidQuotaKey(key) && !store.ValidQuotaPool(key) {
					valid = false
					break
				}
				keys = append(keys, key)
			}
			if !valid {
				fmt.Fprintf(os.Stderr, "handoffkeep: ignoring invalid quota grant line %q\n", line)
				continue
			}
			set := grants[id]
			if set == nil {
				set = map[string]bool{}
				grants[id] = set
			}
			for _, key := range keys {
				set[key] = true
			}
			continue
		}
		// Host grant lines are parsed by LoadHostGrants, which logs and
		// skips a bad one; they are never a token and never fatal here.
		if strings.HasPrefix(k, hostGrantPrefix) {
			continue
		}
		// Import grants are read by LoadImportReadGrants from the same
		// auth file; they are not bearer-token lines.
		if strings.HasPrefix(k, "HANDOFFKEEP_IMPORT_READONLY_") {
			continue
		}
		if !ok || v == "" {
			return nil, nil, errors.New("invalid HANDOFFKEEP_AUTH_FILE")
		}
		id, isToken := strings.CutPrefix(k, "HANDOFFKEEP_TOKEN_")
		if !isToken {
			return nil, nil, errors.New("invalid HANDOFFKEEP_AUTH_FILE")
		}
		if id == "" {
			return nil, nil, errors.New("invalid token client id")
		}
		if _, duplicate := out[id]; duplicate {
			return nil, nil, errors.New("ambiguous auth identity")
		}
		digest := sha256.Sum256([]byte(v))
		for _, existing := range out {
			other := sha256.Sum256([]byte(existing))
			if subtle.ConstantTimeCompare(digest[:], other[:]) == 1 {
				return nil, nil, errors.New("ambiguous auth identity")
			}
		}
		out[id] = v
	}
	if len(out) == 0 {
		return nil, nil, errors.New("HANDOFFKEEP_AUTH_FILE contains no tokens")
	}
	for id := range grants {
		if _, ok := out[id]; !ok {
			fmt.Fprintf(os.Stderr, "handoffkeep: ignoring HANDOFFKEEP_QUOTA_ grants for unknown client %q\n", id)
			delete(grants, id)
		}
	}
	return out, grants, nil
}

// LoadImportReadGrants reads exact opt-in read-only YouTrack import grants.
// A grant is HANDOFFKEEP_IMPORT_READONLY_<CLIENT>=true next to that client's
// bearer token; it grants access only to /v1/import/*.
func LoadImportReadGrants(path string, tokens Tokens) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	grants := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		id, found := strings.CutPrefix(k, "HANDOFFKEEP_IMPORT_READONLY_")
		if !found {
			continue
		}
		if !ok || v != "true" || id == "" {
			fmt.Fprintf(os.Stderr, "handoffkeep: ignoring invalid import read grant for %q\n", id)
			continue
		}
		if _, known := tokens[id]; !known {
			fmt.Fprintf(os.Stderr, "handoffkeep: ignoring import read grant for unknown client %q\n", id)
			continue
		}
		grants[id] = true
	}
	return grants, nil
}
func (t Tokens) Client(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) == len("Bearer ") {
		return "", false
	}
	digest := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
	client := ""
	for id, want := range t {
		other := sha256.Sum256([]byte(want))
		if subtle.ConstantTimeCompare(digest[:], other[:]) == 1 {
			client = id
		}
	}
	return client, client != ""
}

type Server struct {
	Service Service
	Tokens  Tokens
	// QuotaAccounts scopes quota writes; a nil map refuses every account.
	QuotaAccounts QuotaAccounts
	// ImportReadGrants restricts a client to GET /v1/import/* and denies every
	// other route, including all writes.
	ImportReadGrants map[string]bool
	// HostGrants binds a client to the host ids it may act for. A client with
	// no entry keeps today's behavior and is logged (see HostGrants.Allows).
	HostGrants HostGrants
	// UI is independently authenticated by Cloudflare Access. A nil UI leaves
	// the route unregistered so deployments without complete Access settings
	// fail closed with the standard ServeMux 404.
	UI http.Handler
	// LinearWebhook authenticates by signature, not bearer token. A nil handler
	// leaves POST /hooks/linear unregistered, so an unconfigured deployment
	// answers it with the standard ServeMux 404.
	LinearWebhook http.Handler
	// LinearDecisions backs POST /v1/tasks/{id}/decision-request/ask. Nil answers
	// that route 503 linear_decisions_disabled.
	LinearDecisions *LinearDecisions
	// ClientRevisions records each authenticated client's CLI revision from
	// its User-Agent and reports it on GET /metrics. Nil records nothing.
	ClientRevisions *ClientRevisions
}

func (s Server) Handler() http.Handler {
	m := http.NewServeMux()
	wrkRegistered := s.Service.Store != nil && (s.Service.Store.WrkEngine().Mode != "off" || s.Service.Store.HasWrkEnrollment(context.Background()))
	if wrkRegistered {
		m.HandleFunc("/v1/wrk/", s.wrk)
	}
	m.HandleFunc("GET /healthz", healthz)
	m.HandleFunc("/v1/checkpoints", s.checkpoints)
	m.HandleFunc("GET /v1/search", s.search)
	m.HandleFunc("PUT /v1/translations", s.translationsPut)
	m.HandleFunc("GET /v1/translations/missing", s.translationsMissing)
	m.HandleFunc("GET /v1/sync/delta", s.syncDelta)
	m.HandleFunc("GET /v1/sync/bootstrap", s.syncBootstrap)
	m.HandleFunc("GET /v1/sync/rows/{model}/{model_id...}", s.syncRow)
	m.HandleFunc("GET /v1/ledger", s.ledger)
	m.HandleFunc("POST /v1/ledger/ack", s.ledgerAck)
	m.HandleFunc("GET /v1/projects/{name}/status-updates", s.statusUpdates)
	m.HandleFunc("POST /v1/projects/{name}/status-updates", s.statusUpdates)
	m.HandleFunc("GET /v1/documents", s.documents)
	m.HandleFunc("GET /v1/import/{resource}", s.importExport)
	m.HandleFunc("/v1/documents/{key...}", s.document)
	m.HandleFunc("GET /v1/memory/{agent}", s.memoryList)
	m.HandleFunc("/v1/memory/{agent}/{name}", s.memory)
	m.HandleFunc("PUT /v1/attachments", s.attachmentPut)
	m.HandleFunc("GET /v1/attachments", s.attachments)
	m.HandleFunc("GET /v1/attachments/{sha}", s.attachment)
	m.HandleFunc("GET /v1/usage", s.usage)
	m.HandleFunc("GET /metrics", s.metrics)
	m.HandleFunc("POST /v1/tasks", s.tasksCreate)
	m.HandleFunc("POST /v1/intake", s.intake)
	m.HandleFunc("POST /v1/ticket-sets", s.ticketSetsCreate)
	m.HandleFunc("GET /v1/triage", s.triageList)
	m.HandleFunc("GET /v1/tasks", s.tasksList)
	m.HandleFunc("GET /v1/tasks/export", s.tasksExport)
	m.HandleFunc("GET /v1/tasks/sweep", s.tasksSweepPreview)
	m.HandleFunc("POST /v1/task-events", s.taskEvents)
	m.HandleFunc("GET /v1/standing-orders", s.standingList)
	m.HandleFunc("POST /v1/standing-orders", s.standingPut)
	m.HandleFunc("GET /v1/projects/{name}/frontier", s.projectFrontier)
	m.HandleFunc("GET /v1/changes", s.changesList)
	m.HandleFunc("POST /v1/changes/{id}/merge-flow", s.changeMergeFlow)
	m.HandleFunc("POST /v1/changes/verifications", s.changeVerification)
	m.HandleFunc("PUT /v1/changes/state", s.changeStatePut)
	m.HandleFunc("PUT /v1/changes/checks", s.changeChecksPut)
	m.HandleFunc("PUT /v1/changes/threads", s.changeThreadsPut)
	m.HandleFunc("PUT /v1/changes/findings", s.changeFindingsPut)
	m.HandleFunc("PUT /v1/changes/gate", s.changeGatePut)
	m.HandleFunc("POST /v1/changes/batch", s.changeBatch)
	m.HandleFunc("GET /v1/changes/one", s.changeOne)
	m.HandleFunc("GET /v1/deploy-services", s.deployServicesList)
	m.HandleFunc("PUT /v1/deploy-services/{name}", s.deployServiceWrite)
	m.HandleFunc("POST /v1/deploy-services/{name}/deactivate", s.deployServiceWrite)
	m.HandleFunc("GET /v1/services", s.servicesList)
	m.HandleFunc("PUT /v1/services/{name}", s.servicePut)
	m.HandleFunc("GET /v1/services/{name}/pending", s.servicePending)
	m.HandleFunc("POST /v1/deploys", s.deployStart)
	m.HandleFunc("POST /v1/deploys/{attempt_id}/finish", s.deployFinish)
	m.HandleFunc("POST /v1/tasks/next", s.tasksNext)
	m.HandleFunc("GET /v1/tasks/projects", s.taskProjectsList)
	m.HandleFunc("POST /v1/tasks/projects", s.taskProjectsCreate)
	m.HandleFunc("GET /v1/tasks/projects/owners", s.projectOwnersList)
	m.HandleFunc("GET /v1/tasks/projects/{project}/owner", s.projectOwner)
	m.HandleFunc("PUT /v1/tasks/projects/{project}/owner", s.projectOwner)
	m.HandleFunc("GET /v1/tasks/projects/{project}/metadata", s.projectMetadata)
	m.HandleFunc("PUT /v1/tasks/projects/{project}/metadata", s.projectMetadata)
	m.HandleFunc("PATCH /v1/tasks/projects/{project}/metadata", s.projectMetadata)
	m.HandleFunc("POST /v1/milestones", s.milestoneSave)
	m.HandleFunc("PUT /v1/milestones/{milestone}", s.milestoneSave)
	m.HandleFunc("PATCH /v1/milestones/{milestone}", s.milestonePatch)
	m.HandleFunc("GET /v1/milestones/{milestone}", s.milestoneGet)
	m.HandleFunc("GET /v1/labels", s.labelsList)
	m.HandleFunc("POST /v1/labels", s.labelSave)
	m.HandleFunc("PUT /v1/labels/{label}", s.labelSave)
	m.HandleFunc("POST /v1/labels/{label}/retire", s.labelRetire)
	m.HandleFunc("GET /v1/tasks/{id}", s.task)
	m.HandleFunc("POST /v1/tasks/{id}/claim", s.taskClaim)
	m.HandleFunc("POST /v1/tasks/{id}/rebind", s.taskRebind)
	m.HandleFunc("POST /v1/tasks/{id}/transition", s.taskTransition)
	m.HandleFunc("POST /v1/tasks/{id}/parent", s.taskParent)
	m.HandleFunc("POST /v1/tasks/{id}/relations", s.taskRelationAdd)
	m.HandleFunc("POST /v1/tasks/{id}/relations/remove", s.taskRelationRemove)
	m.HandleFunc("POST /v1/tasks/{id}/origins", s.taskOriginAdd)
	m.HandleFunc("GET /v1/tasks/{id}/origins", s.taskOriginList)
	m.HandleFunc("GET /v1/task-origins", s.taskOriginFind)
	m.HandleFunc("GET /v1/dots/results", s.dotsResultsList)
	m.HandleFunc("POST /v1/dots/requests", s.dotsRequestCreate)
	m.HandleFunc("POST /v1/dots/results/{id}/adoption", s.dotsAdoption)
	m.HandleFunc("POST /v1/tasks/{id}/reopen", s.taskReopen)
	m.HandleFunc("POST /v1/tasks/{id}/project", s.taskProject)
	m.HandleFunc("POST /v1/tasks/{id}/area", s.taskArea)
	m.HandleFunc("POST /v1/tasks/{id}/milestone", s.taskMilestone)
	m.HandleFunc("POST /v1/tasks/{id}/labels", s.taskLabel)
	m.HandleFunc("POST /v1/tasks/{id}/deadlines", s.taskDeadlines)
	m.HandleFunc("POST /v1/tasks/{id}/due", s.taskDue)
	m.HandleFunc("POST /v1/tasks/{id}/subscribers", s.taskSubscriber)
	m.HandleFunc("GET /v1/task-areas", s.taskAreasList)
	m.HandleFunc("POST /v1/tasks/relane", s.tasksRelane)
	m.HandleFunc("POST /v1/tasks/{id}/decision-request", s.taskDecisionRequest)
	m.HandleFunc("POST /v1/tasks/{id}/decision-request/resolve", s.taskDecisionResolve)
	m.HandleFunc("POST /v1/tasks/{id}/decision-request/ask", s.taskDecisionAsk)
	m.HandleFunc("POST /v1/tasks/dispositions", s.dispositionCreate)
	m.HandleFunc("GET /v1/tasks/dispositions/summary", s.dispositionSummary)
	m.HandleFunc("POST /v1/tasks/dispositions/{id}/apply", s.dispositionApply)
	m.HandleFunc("GET /v1/tasks/{id}/comments", s.taskCommentsList)
	m.HandleFunc("POST /v1/tasks/{id}/comments", s.taskCommentCreate)
	m.HandleFunc("POST /v1/decisions/resolve", s.decisionResolve)
	m.HandleFunc("GET /v1/decisions/deliveries/hygiene", s.decisionDeliveryHygiene)
	m.HandleFunc("GET /v1/decision-requests", s.decisionRecordsList)
	m.HandleFunc("GET /v1/decision-requests/sources", s.decisionSources)
	m.HandleFunc("POST /v1/decision-requests/{id}/snooze", s.decisionRecordSnooze)
	m.HandleFunc("POST /v1/relay/events", s.relayEventsCreate)
	m.HandleFunc("POST /v1/relay/events/{id}/delivered", s.relayEventDelivered)
	m.HandleFunc("GET /v1/relay/events", s.relayEventsList)
	m.HandleFunc("POST /v1/placements", s.placementCreate)
	m.HandleFunc("GET /v1/placements", s.placementsList)
	m.HandleFunc("POST /v1/jobs", s.jobClaim)
	m.HandleFunc("GET /v1/jobs", s.jobsList)
	m.HandleFunc("GET /v1/job-skills", s.jobSkillsReport)
	m.HandleFunc("GET /v1/jobs/{job}", s.jobGet)
	m.HandleFunc("PUT /v1/jobs/{job}", s.jobUpdate)
	m.HandleFunc("GET /v1/quota/accounts/{key}/latest", s.quotaLatest)
	m.HandleFunc("PUT /v1/quota/accounts/{key}/observation", s.quotaPut)
	m.HandleFunc("POST /v1/quota/accounts/{key}/lease", s.quotaLease)
	m.HandleFunc("DELETE /v1/quota/accounts/{key}/lease", s.quotaRelease)
	m.HandleFunc("GET /v1/quota/presence", s.quotaPresenceList)
	m.HandleFunc("PUT /v1/quota/presence", s.quotaPresencePut)
	m.HandleFunc("GET /v1/bench/scores", s.benchScoresList)
	m.HandleFunc("PUT /v1/bench/scores", s.benchScoresPut)
	m.HandleFunc("GET /v1/bench/reps", s.benchRepsList)
	m.HandleFunc("PUT /v1/bench/reps", s.benchRepsPut)
	m.HandleFunc("PATCH /v1/bench/reps/{id}", s.benchRepPatch)
	m.HandleFunc("GET /v1/bench/grades", s.benchGradesList)
	m.HandleFunc("PUT /v1/bench/grades", s.benchGradesPut)
	m.HandleFunc("GET /v1/bench/catalog", s.benchCatalogList)
	m.HandleFunc("PUT /v1/bench/catalog", s.benchCatalogPut)
	m.HandleFunc("GET /v1/bench/area-exceptions", s.benchAreaExceptionsList)
	m.HandleFunc("PUT /v1/bench/area-exceptions", s.benchAreaExceptionsPut)
	m.HandleFunc("PUT /v1/chat/questions/{id}", s.chatQuestionPut)
	m.HandleFunc("GET /v1/chat/questions/{id}", s.chatQuestionGet)
	m.HandleFunc("POST /v1/chat/questions", s.chatQuestionPost)
	m.HandleFunc("POST /v1/chat/questions/{id}/transition", s.chatQuestionTransition)
	m.HandleFunc("GET /v1/chat/questions", s.chatQuestionsList)
	m.HandleFunc("POST /v1/chat/messages", s.chatMessageCreate)
	m.HandleFunc("GET /v1/chat/messages", s.chatMessagesList)
	m.HandleFunc("POST /v1/chat/messages/{id}/delivered", s.chatMessageDelivered)
	m.HandleFunc("POST /v1/chat/messages/{id}/failed", s.chatMessageFailed)
	m.HandleFunc("GET /v1/assistant/pending", s.assistantPending)
	m.HandleFunc("GET /v1/assistant/outbox", s.assistantOutboxList)
	m.HandleFunc("POST /v1/assistant/outbox/sent", s.assistantOutboxSent)
	m.HandleFunc("POST /v1/assistant/decisions/resolve", s.assistantDecisionResolve)
	if s.UI != nil {
		m.Handle("/ui", s.UI)
		m.Handle("/ui/", s.UI)
	}
	if s.LinearWebhook != nil {
		m.Handle("POST /hooks/linear", s.LinearWebhook)
	}
	v1 := v1Handler{mux: m, legacy: s.legacyWrkFence(m)}
	if wrkRegistered {
		v1.wrk = http.HandlerFunc(s.wrk)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if client, ok := s.Tokens.Client(r); ok && s.ImportReadGrants[client] {
			if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/import/") {
				writeError(w, http.StatusForbidden, "import_read_only")
				return
			}
		}
		v1.ServeHTTP(w, r)
	})
}

// wrkPattern is the route pattern reported for the /v1/wrk surface.
const wrkPattern = "/v1/wrk/{path...}"

// v1Handler serves the /v1 surface behind the legacy wrk fence. Its Handler
// method reports the route a request dispatches to without serving it, the way
// *http.ServeMux does, so telemetry can key the head sampling decision on the
// route before the request starts (telemetry.routePattern follows any value
// with this method).
type v1Handler struct {
	mux    *http.ServeMux
	legacy http.Handler
	wrk    http.Handler // nil when the wrk routes are not registered
}

func (h v1Handler) isWrk(r *http.Request) bool {
	return h.wrk != nil && (r.URL.Path == "/v1/wrk" || strings.HasPrefix(r.URL.Path, "/v1/wrk/"))
}

func (h v1Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The legacy fence may return before ServeMux dispatches the request.
	_, r.Pattern = h.mux.Handler(r)
	// ServeMux cleans dot/slash paths with redirects before dispatch. O3
	// must authenticate and reject those paths without ever sending Location.
	if h.isWrk(r) {
		r.Pattern = wrkPattern
		h.wrk.ServeHTTP(w, r)
		return
	}
	h.legacy.ServeHTTP(w, r)
}

// Handler reports the route ServeHTTP would dispatch to, without serving it.
func (h v1Handler) Handler(r *http.Request) (http.Handler, string) {
	if h.isWrk(r) {
		return h.wrk, wrkPattern
	}
	return h.mux.Handler(r)
}

// readBuildInfo is a variable so tests can stamp a VCS-tagged build info.
var readBuildInfo = debug.ReadBuildInfo

// healthz is the unauthenticated liveness probe. Beyond "status":"ok" it
// reports the binary's embedded VCS stamp so a deploy can verify which commit
// is serving without shelling out to `strings` on the binary. The vcs_* keys
// are absent when the binary was built without VCS stamping (e.g. from a
// source export or with -buildvcs=false).
func healthz(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{"status": "ok"}
	if info, ok := readBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				out["vcs_revision"] = setting.Value
			case "vcs.time":
				out["vcs_time"] = setting.Value
			case "vcs.modified":
				out["vcs_modified"] = setting.Value
			}
		}
	}
	jsonOut(w, http.StatusOK, out)
}

func (s Server) auth(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := s.Tokens.Client(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
	}
	if ok {
		_, receipt := store.WrkPrincipal(r.Context())
		*r = *r.WithContext(store.WithWrkPrincipal(r.Context(), id, receipt))
		s.ClientRevisions.observe(id, r.UserAgent())
	}
	return id, ok
}

// pathID parses the int64 path value named key. It is the one id parser for
// routes; callers decide what range is valid.
func pathID(r *http.Request, key string) (int64, error) {
	return strconv.ParseInt(r.PathValue(key), 10, 64)
}

func (s Server) tasksCreate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	// link and new answer the duplicate check; see store.CreateTaskChecked.
	var input struct {
		store.Task
		Link int64 `json:"link,omitempty"`
		New  bool  `json:"new,omitempty"`
	}
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	if err := store.CheckTaskArea(input.Area); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.CreateTask(r.Context(), client, input.Task, store.DuplicateCheck{Link: input.Link, New: input.New})
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusCreated, x)
}

func (s Server) tasksList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 20, 1000)
	if err != nil {
		appErr(w, r, err)
		return
	}
	if r.URL.Query().Has("change") {
		// change is exclusive: any other filter next to it is a mistake, not
		// an intersection.
		changeID, parseErr := strconv.ParseInt(r.URL.Query().Get("change"), 10, 64)
		if parseErr != nil || changeID < 1 {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		for _, other := range []string{"lane", "state", "parent_lane", "project", "area", "stalled", "after_id"} {
			if r.URL.Query().Has(other) {
				writeError(w, http.StatusBadRequest, "invalid_context")
				return
			}
		}
		xs, err := s.Service.ListTasksFiltered(r.Context(), store.TaskQuery{ChangeID: &changeID, Limit: limit})
		if err != nil {
			appErr(w, r, err)
			return
		}
		// Echoed like project: an old server that drops the parameter would
		// answer with every task.
		s.Service.overlayTasks(r.Context(), xs)
		jsonOut(w, http.StatusOK, map[string]any{"tasks": xs, "change": changeID})
		return
	}
	lane, state, parentLane := r.URL.Query().Get("lane"), r.URL.Query().Get("state"), r.URL.Query().Get("parent_lane")
	var project *string
	if r.URL.Query().Has("project") {
		v := r.URL.Query().Get("project")
		project = &v
	}
	var area *string
	if r.URL.Query().Has("area") {
		v := r.URL.Query().Get("area")
		area = &v
	}
	// Refuse an unknown area before the store is consulted, so the 400 does
	// not depend on a database round trip.
	if area != nil && *area != "" {
		if err := store.CheckTaskArea(area); err != nil {
			appErr(w, r, err)
			return
		}
	}
	stalled := false
	if r.URL.Query().Has("stalled") {
		if stalled, err = strconv.ParseBool(r.URL.Query().Get("stalled")); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
	}
	var xs []store.Task
	switch {
	case stalled:
		q := store.TaskQuery{Lane: lane, State: state, ParentLane: parentLane, Project: project, Area: area, Stalled: true, Limit: limit}
		if r.URL.Query().Has("after_id") {
			afterID, parseErr := queryCursor(r, "after_id")
			if parseErr != nil {
				appErr(w, r, parseErr)
				return
			}
			q.AfterID = &afterID
		}
		xs, err = s.Service.ListTasksFiltered(r.Context(), q)
	case r.URL.Query().Has("after_id"):
		afterID, parseErr := queryCursor(r, "after_id")
		if parseErr != nil {
			appErr(w, r, parseErr)
			return
		}
		xs, err = s.Service.ListTasksPage(r.Context(), lane, state, parentLane, project, area, afterID, limit)
	default:
		xs, err = s.Service.ListTasks(r.Context(), lane, state, parentLane, project, area, limit)
	}
	if err != nil {
		appErr(w, r, err)
		return
	}
	s.Service.overlayTasks(r.Context(), xs)
	body := map[string]any{"tasks": xs}
	if project != nil {
		// Echo the requested filter so new clients can tell a filtered
		// answer from an old server that silently dropped the parameter.
		body["project"] = *project
	}
	if area != nil {
		// Same echo as project: a server that predates the filter cannot
		// echo it, which is how a client tells it from an empty result.
		body["area"] = *area
	}
	if stalled {
		// Same echo again: an old server ignores stalled=true and would
		// answer with every task.
		body["stalled"] = true
	}
	jsonOut(w, http.StatusOK, body)
}

// tasksExport serves one consistent task snapshot. It reuses the existing
// bearer authentication and filter validation; it adds no credential class.
// Error bodies carry only a stable code — never task content or scope data.
func (s Server) tasksExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 1000, store.ExportLimitMax)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_export_query")
		return
	}
	lane, state, parentLane := r.URL.Query().Get("lane"), r.URL.Query().Get("state"), r.URL.Query().Get("parent_lane")
	var project *string
	if r.URL.Query().Has("project") {
		v := r.URL.Query().Get("project")
		project = &v
	}
	out, err := s.Service.ExportTasks(r.Context(), lane, state, parentLane, project, limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidExportQuery) {
			writeError(w, http.StatusBadRequest, "invalid_export_query")
			return
		}
		writeError(w, http.StatusInternalServerError, "export_unavailable")
		return
	}
	jsonOut(w, http.StatusOK, out)
}

func (s Server) task(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	x, found, err := s.Service.GetTask(r.Context(), id)
	if err != nil {
		appErr(w, r, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	tasks := []store.Task{x}
	s.Service.overlayTasks(r.Context(), tasks)
	jsonOut(w, http.StatusOK, tasks[0])
}

type taskClaimInput struct {
	ClaimedBy string `json:"claimed_by"`
	JobID     string `json:"job_id"`
	// NoJob records why a claim without job_id is allowed; the claim event
	// keeps it. An empty reason is refused with task_job_required.
	NoJob string `json:"no_job,omitempty"`
}

// taskProjectsList answers the server-configured project vocabulary — the
// set tasks add --project accepts. The list is data, not code: operators
// extend it through taskProjectsCreate.
func (s Server) taskProjectsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	names, err := s.Service.ListTaskProjects(r.Context())
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"projects": names})
}

// taskProjectsCreate extends the project vocabulary by one name. Re-adding
// an existing name is an idempotent 200 rather than a conflict.
func (s Server) taskProjectsCreate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input struct {
		Name string `json:"name"`
	}
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	created, err := s.Service.AddTaskProject(r.Context(), client, input.Name)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, map[string]any{"name": input.Name, "created": created})
}

// taskProject reclassifies one task inside the configured vocabulary and
// records the move as a kind='project' event — the relane-like audit trail
// for the field.
func (s Server) taskProject(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input struct {
		Project  string                      `json:"project"`
		Note     string                      `json:"note"`
		Metadata *store.ProjectMetadataPatch `json:"metadata,omitempty"`
	}
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	if err := s.Service.wrkTaskPrecheck(r.Context(), id, "", true); err != nil {
		appErr(w, r, err)
		return
	}
	x, _, err := s.Service.Store.SetTaskProjectWithMetadata(r.Context(), id, input.Project, client, input.Note, input.Metadata)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

// taskAreasList answers the closed area vocabulary in its code order — the
// set tasks add --area accepts. It is read only: the list changes with the
// code, never through a route.
func (s Server) taskAreasList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"areas": store.TaskAreas()})
}

// taskArea tags or clears one task's area and records the change as a
// kind='area' event. The body must carry the area key: an explicit null
// clears, an absent key is a malformed request rather than a clear.
func (s Server) taskArea(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input struct {
		Area json.RawMessage `json:"area"`
		Note string          `json:"note"`
	}
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	var area *string
	switch {
	case len(input.Area) == 0:
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	case string(input.Area) != "null":
		var v string
		if err := json.Unmarshal(input.Area, &v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		area = &v
		if err := store.CheckTaskArea(area); err != nil {
			appErr(w, r, err)
			return
		}
	}
	x, _, err := s.Service.SetTaskArea(r.Context(), id, area, client, input.Note)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func (s Server) taskClaim(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input taskClaimInput
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.ClaimTask(r.Context(), id, input.ClaimedBy, input.JobID, input.NoJob)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

type taskRebindInput struct {
	FromJob string `json:"from_job"`
	ToJob   string `json:"to_job"`
	By      string `json:"by"`
	Reason  string `json:"reason"`
}

func (s Server) taskRebind(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input taskRebindInput
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.RebindTaskJob(r.Context(), id, input.FromJob, input.ToJob, input.By, input.Reason)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func (s Server) tasksNext(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input struct {
		Lane      string `json:"lane"`
		ClaimedBy string `json:"claimed_by"`
		JobID     string `json:"job_id"`
		NoJob     string `json:"no_job,omitempty"`
	}
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.NextTask(r.Context(), input.Lane, input.ClaimedBy, input.JobID, input.NoJob)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

type taskTransitionInput struct {
	Closure  string          `json:"closure,omitempty"`
	Evidence string          `json:"evidence,omitempty"`
	To       string          `json:"to"`
	Note     string          `json:"note"`
	Refs     *store.TaskRefs `json:"refs,omitempty"`
	// NoJob records the caller-stated reason a transition may land in
	// claimed/in_progress without claimed_by + refs.job_id. Without linkage
	// an empty reason is refused with task_job_required.
	NoJob string `json:"no_job,omitempty"`
	// DuplicateOf names the task a duplicate transition points at. The
	// duplicate_of relation and the duplicate state are written together.
	DuplicateOf int64 `json:"duplicate_of,omitempty"`
	// Priority, Kind and Project set the values a task leaves triage with;
	// Set applies the triage exit to its children still in triage.
	Priority *int    `json:"priority,omitempty"`
	Kind     string  `json:"kind,omitempty"`
	Project  *string `json:"project,omitempty"`
	Set      bool    `json:"set,omitempty"`
}

func (s Server) taskTransition(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input taskTransitionInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.TransitionTaskWith(r.Context(), id, client, store.TransitionInput{To: input.To, Closure: input.Closure, Evidence: input.Evidence, Note: input.Note, Refs: input.Refs, NoJob: input.NoJob, DuplicateOf: input.DuplicateOf, Priority: input.Priority, Kind: input.Kind, Project: input.Project, Set: input.Set})
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

// taskParent sets (parent_id) or clears (null) a task's single parent. The
// link is acyclic; setting the current value changes nothing.
func (s Server) taskParent(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input struct {
		ParentID *int64 `json:"parent_id"`
	}
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	x, _, err := s.Service.SetTaskParent(r.Context(), id, input.ParentID, client)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

type taskRelationInput struct {
	Kind string `json:"kind"`
	// Other is the task at the other end: the one this task blocks, is a
	// duplicate of, or relates to.
	Other int64 `json:"other"`
}

// taskRelationAdd adds a relation edge. Adding a live edge again changes
// nothing, and adding a duplicate_of edge also moves the task to duplicate.
func (s Server) taskRelationAdd(w http.ResponseWriter, r *http.Request) {
	s.taskRelationWrite(w, r, true)
}

// taskRelationRemove archives a relation edge; removing an absent one changes
// nothing.
func (s Server) taskRelationRemove(w http.ResponseWriter, r *http.Request) {
	s.taskRelationWrite(w, r, false)
}

func (s Server) taskRelationWrite(w http.ResponseWriter, r *http.Request, add bool) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input taskRelationInput
	if err := decode(r, &input, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	var changed bool
	if add {
		changed, err = s.Service.AddTaskRelation(r.Context(), id, input.Kind, input.Other, client)
	} else {
		changed, err = s.Service.RemoveTaskRelation(r.Context(), id, input.Kind, input.Other, client)
	}
	if err != nil {
		appErr(w, r, err)
		return
	}
	x, found, err := s.Service.GetTask(r.Context(), id)
	if err != nil {
		appErr(w, r, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"changed": changed, "task": x})
}

// taskDecisionRequest records a structured decision request (#618). The
// requester is the authenticated client. 201 is a new record, 200 a
// duplicate re-send returning the already recorded request_id.
func (s Server) taskDecisionRequest(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input store.DecisionRequestInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.Store.RecordDecisionRequest(r.Context(), id, client, input)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusCreated
	if x.Duplicate {
		status = http.StatusOK
	}
	jsonOut(w, status, x)
}

// DecisionResolveResponse maps one ResolveDecision outcome to the HTTP status
// and body every answer route shares (hk 3061; R2 result enum):
//
//	answered              200 (a re-send also carries duplicate:true)
//	pending_confirmation  202, the request stays open
//	denied                200, the operator denied a pending attempt; the
//	                      request stays open
//	stale_revision        409 decision_request_stale
//	superseded            409 decision_request_stale, or decision_request_resolved
//	                      for a withdrawn request
//	conflict, expired     409 decision_request_resolved
//
// A 200 or 202 body is the DecisionRequestResult with its result key; a 409
// body is {"error": code, "result": result}.
func DecisionResolveResponse(out store.ResolveDecisionOutcome) (int, any) {
	switch {
	case out.Refusal != nil:
		code := "decision_request_resolved"
		if errors.Is(out.Refusal, store.ErrDecisionRequestStale) {
			code = "decision_request_stale"
		}
		return http.StatusConflict, map[string]string{"error": code, "result": out.Result}
	case out.Result == store.DecisionResultPendingConfirmation:
		return http.StatusAccepted, decisionRequestResult(out)
	}
	return http.StatusOK, decisionRequestResult(out)
}

func decisionRequestResult(out store.ResolveDecisionOutcome) store.DecisionRequestResult {
	return store.DecisionRequestResult{Task: out.Task, Request: out.Request, Duplicate: out.Duplicate, Result: out.Result, ResolutionID: out.ResolutionID, AttemptID: out.AttemptID}
}

// taskDecisionResolveInput is the bearer resolve body: the shared resolve
// input plus the session channel's claim. Channel is "" or "cli" (today's
// path) or "session" (an operator session recording the operator's words).
type taskDecisionResolveInput struct {
	store.DecisionResolveInput
	Channel       string `json:"channel,omitempty"`
	Provenance    string `json:"provenance,omitempty"`
	OperatorQuote string `json:"operator_quote,omitempty"`
	DecisionRef   string `json:"decision_ref,omitempty"`
}

// taskDecisionResolve closes the current request (answered, default_applied
// with a receipt, or withdrawn). It never changes the task state. Only the
// bearer client "operator" on the cli channel is a verified operator path;
// every other client, and the session channel, is a relay (R1).
func (s Server) taskDecisionResolve(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input taskDecisionResolveInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	in := store.ResolveDecisionInput{
		TaskID: id, RequestID: input.RequestID, Kind: input.Kind, Option: input.Option, Text: input.Text,
		Receipt: input.Receipt, Responder: input.Responder, EventKey: input.EventKey,
		Provenance: input.Provenance, OperatorQuote: input.OperatorQuote, DecisionRef: input.DecisionRef,
		Actor: client,
	}
	switch input.Channel {
	case "", store.DecisionChannelCLI:
		in.Channel, in.Verified = store.DecisionChannelCLI, client == operatorClientID
	case store.DecisionChannelSession:
		in.Channel = store.DecisionChannelSession
	default:
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	out, err := s.Service.Store.ResolveDecision(r.Context(), in)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status, body := DecisionResolveResponse(out)
	jsonOut(w, status, body)
}

// TaskRelaneBatchMax bounds one relane request. It comfortably covers the
// triage migrations this route exists for while keeping a mistake bounded.
// Exported so the CLI can refuse an oversized batch before sending.
const TaskRelaneBatchMax = 500

type taskRelaneInput struct {
	IDs          []int64 `json:"ids"`
	To           string  `json:"to"`
	Note         string  `json:"note"`
	AllowNewLane bool    `json:"allow_new_lane,omitempty"`
}

// taskRelaneResult is one item's outcome. A failed item carries a stable
// error code; the request keeps processing the rest of the batch.
type taskRelaneResult struct {
	ID      int64       `json:"id"`
	OK      bool        `json:"ok"`
	Changed bool        `json:"changed"`
	Task    *store.Task `json:"task,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// relaneErrorCode maps a store failure to the closed error vocabulary items
// report. Validation errors collapse to invalid_task_relane; unrecognized
// errors (driver faults, cancelled contexts) report internal_error rather
// than leaking internals or posing as a retry-safe validation failure.
func relaneErrorCode(err error) string {
	var denied wrkrecords.Error
	if errors.As(err, &denied) {
		return wrkrecords.Code(denied)
	}
	switch {
	case errors.Is(err, store.ErrTaskNotFound):
		return "not_found"
	case errors.Is(err, store.ErrTaskTerminal):
		return "task_terminal"
	case errors.Is(err, store.ErrTaskLaneUnknown):
		return "unknown_lane"
	case errors.Is(err, store.ErrTaskConflict):
		return "task_conflict"
	case errors.Is(err, youtrack.ErrUnavailable):
		return "youtrack_unavailable"
	}
	if _, ok := guard.SecretPattern(err); ok {
		return "secret_like_content"
	}
	if strings.HasPrefix(err.Error(), "invalid task relane") {
		return "invalid_task_relane"
	}
	return "internal_error"
}

// tasksRelane moves one or more tasks between lanes. It reuses the existing
// bearer write authentication — no new credential class — and each id runs in
// its own store transaction so one failure never rolls back the batch's
// other items.
func (s Server) tasksRelane(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input taskRelaneInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	if len(input.IDs) < 1 || len(input.IDs) > TaskRelaneBatchMax || strings.TrimSpace(input.To) == "" || strings.TrimSpace(input.Note) == "" {
		writeError(w, http.StatusBadRequest, "invalid_task_relane")
		return
	}
	for _, id := range input.IDs {
		if id < 1 {
			writeError(w, http.StatusBadRequest, "invalid_task_relane")
			return
		}
	}
	results := make([]taskRelaneResult, 0, len(input.IDs))
	moved, unchanged, failed := 0, 0, 0
	for _, id := range input.IDs {
		task, changed, err := s.Service.RelaneTask(r.Context(), id, input.To, client, input.Note, input.AllowNewLane)
		item := taskRelaneResult{ID: id}
		if err != nil {
			item.Error = relaneErrorCode(err)
			failed++
		} else {
			item.OK = true
			item.Changed = changed
			item.Task = &task
			if changed {
				moved++
			} else {
				unchanged++
			}
		}
		results = append(results, item)
	}
	jsonOut(w, http.StatusOK, map[string]any{"results": results, "moved": moved, "unchanged": unchanged, "failed": failed})
}

func (s Server) dispositionCreate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input store.DispositionInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, created, err := s.Service.CreateDisposition(r.Context(), client, input)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, map[string]any{"task": x, "created": created})
}

func (s Server) dispositionSummary(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	asOf := time.Now().UTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("as_of")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid as_of")
			return
		}
		asOf = parsed
	}
	summary, err := s.Service.DispositionSummary(r.Context(), asOf)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, summary)
}

func (s Server) dispositionApply(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	defer r.Body.Close()
	var input struct {
		Note string `json:"note"`
	}
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.ApplyDisposition(r.Context(), id, client, input.Note)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func (s Server) decisionResolve(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input DecisionResolveInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	event, err := s.Service.ResolveDecision(r.Context(), input)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"event": event})
}

func (s Server) relayEventsCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var x store.RelayEvent
	if err := decode(r, &x, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, created, err := s.Service.AppendRelayEvent(r.Context(), x)
	if err != nil {
		appErr(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, x)
}

type relayEventDeliveryInput struct {
	Machine string `json:"machine"`
	Pane    string `json:"pane"`
}

func (s Server) relayEventDelivered(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input relayEventDeliveryInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.MarkRelayEventDelivered(r.Context(), id, input.Machine, input.Pane)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func relayEventsLimit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New("limit")
	}
	return limit, nil
}

// queryCursor parses the non-negative int64 cursor query parameter name
// ("after_id", "after"); absent is 0. It is the one cursor parser.
func queryCursor(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseInt(v, 10, 64)
	if err != nil || cursor < 0 {
		return 0, errors.New(name)
	}
	return cursor, nil
}

func (s Server) relayEventsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := relayEventsLimit(r)
	if err != nil {
		appErr(w, r, err)
		return
	}
	afterID, err := queryCursor(r, "after_id")
	if err != nil {
		appErr(w, r, err)
		return
	}
	undeliveredValue := r.URL.Query().Get("undelivered")
	undelivered := undeliveredValue == "1" || undeliveredValue == "true"
	xs, err := s.Service.ListRelayEvents(r.Context(), r.URL.Query().Get("lane"), r.URL.Query().Get("kind"), undelivered, afterID, limit)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"events": xs})
}

const benchRequestMaxBytes = 8 << 20

type benchScoresInput struct {
	Scores []store.BenchScore `json:"scores"`
}

type benchRepsInput struct {
	Reps []store.BenchRep `json:"reps"`
}

type benchGradesInput struct {
	Grades []store.BenchGrade `json:"grades"`
}

func benchBatchValid(n int) bool {
	return n >= 1 && n <= 1000
}

func (s Server) benchScoresList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 1000, 5000)
	if err != nil {
		appErr(w, r, err)
		return
	}
	xs, err := s.Service.ListBenchScores(r.Context(), r.URL.Query().Get("model_id"), r.URL.Query().Get("source"), limit)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"scores": xs})
}

func (s Server) benchScoresPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input benchScoresInput
	if err := decode(r, &input, benchRequestMaxBytes); err != nil || !benchBatchValid(len(input.Scores)) {
		if err == nil {
			err = errors.New("invalid bench scores")
		}
		appErr(w, r, err)
		return
	}
	n, err := s.Service.UpsertBenchScores(r.Context(), client, input.Scores)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"upserted": n})
}

func (s Server) benchRepsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 1000, 5000)
	if err != nil {
		appErr(w, r, err)
		return
	}
	xs, err := s.Service.ListBenchReps(r.Context(), r.URL.Query().Get("profile"), r.URL.Query().Get("grade"), r.URL.Query().Get("effort"), limit)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"reps": xs})
}

func (s Server) benchRepsPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input benchRepsInput
	if err := decode(r, &input, benchRequestMaxBytes); err != nil || !benchBatchValid(len(input.Reps)) {
		if err == nil {
			err = errors.New("invalid bench reps")
		}
		appErr(w, r, err)
		return
	}
	ids, err := s.Service.UpsertBenchReps(r.Context(), client, input.Reps)
	if err != nil {
		var repConflict *store.BenchRepConflictError
		if errors.As(err, &repConflict) {
			for _, c := range repConflict.Conflicts {
				log.Printf("bench_rep_conflict created_by=%s index=%d origin_id=%d conflict_server_id=%d", client, c.Index, c.OriginID, c.ServerID)
			}
		}
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"upserted": len(ids), "ids": ids})
}

// benchRepPatch fills a stored rep's outcome columns that are still unknown
// (store.PatchBenchRepOutcome). Any node token may patch.
func (s Server) benchRepPatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var p store.BenchRepOutcomePatch
	if err := decode(r, &p, benchRequestMaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.PatchBenchRepOutcome(r.Context(), id, p)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func (s Server) benchGradesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	xs, err := s.Service.ListBenchGrades(r.Context())
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"grades": xs})
}

func (s Server) benchGradesPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input benchGradesInput
	if err := decode(r, &input, benchRequestMaxBytes); err != nil || !benchBatchValid(len(input.Grades)) {
		if err == nil {
			err = errors.New("invalid bench grades")
		}
		appErr(w, r, err)
		return
	}
	n, err := s.Service.UpsertBenchGrades(r.Context(), client, input.Grades)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"upserted": n})
}

// operatorClientID is the reserved auth-file client id carrying the operator
// credential (HANDOFFKEEP_TOKEN_operator), the same convention panewire #68
// established with HUB_TOKEN_operator. Deployments without the entry fail
// closed: every PUT /v1/bench/catalog gets 403.
const operatorClientID = "operator"

// operatorDeskClientID is the operator-desk session's client id. It records
// the operator's catalog decisions on the operator's behalf, so it may write
// the bench catalog and area exceptions too (operator 2026-11-01). Every other
// client still gets operator_required.
const operatorDeskClientID = "operator-desk"

// panewireDesignClientID is the panewire design session's client id. It owns
// the bench catalog, so it writes the catalog and area exceptions under its
// own id instead of going through operator-desk (decision
// hk:doc decision/2026-11-08/panewire-design-catalog-writer). It gains no other
// right: quota grants, host grants and task writes are unchanged.
const panewireDesignClientID = "panewire-design"

// benchCatalogWriter reports whether client may write the bench catalog and
// the area exceptions.
func benchCatalogWriter(client string) bool {
	return client == operatorClientID || client == operatorDeskClientID || client == panewireDesignClientID
}

type benchCatalogInput struct {
	Catalog []store.BenchCatalogEntry `json:"catalog"`
}

func (s Server) benchCatalogList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	includeRetired := q.Get("include_retired") == "1" || q.Get("include_retired") == "true"
	xs, err := s.Service.ListBenchCatalog(r.Context(), q.Get("pool"), includeRetired)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"catalog": xs})
}

func (s Server) benchCatalogPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if !benchCatalogWriter(client) {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	defer r.Body.Close()
	// The body is buffered so a decode failure can be re-walked to name the
	// field that failed (benchCatalogDecodeDetail); the limit matches decode().
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(benchRequestMaxBytes+4096)))
	if err != nil {
		appErr(w, r, err)
		return
	}
	var input benchCatalogInput
	de := json.NewDecoder(bytes.NewReader(body))
	de.DisallowUnknownFields()
	if err := de.Decode(&input); err != nil {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_bench_catalog_json", "detail": benchCatalogDecodeDetail(body, err)})
		return
	}
	if !benchBatchValid(len(input.Catalog)) {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	n, err := s.Service.UpsertBenchCatalog(r.Context(), input.Catalog)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"upserted": n})
}

type benchAreaExceptionsInput struct {
	Exceptions []store.BenchAreaException `json:"exceptions"`
}

// benchAreaExceptionsList answers the routing exception list to any
// authenticated client — hosts read it for routing, only the operator
// client writes it.
func (s Server) benchAreaExceptionsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	includeRetired := q.Get("include_retired") == "1" || q.Get("include_retired") == "true"
	xs, err := s.Service.ListBenchAreaExceptions(r.Context(), q.Get("area"), includeRetired)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"exceptions": xs})
}

// benchAreaExceptionsPut upserts exception rows under the same
// operator-client rule and decode strictness as benchCatalogPut. Retiring a
// row means sending retired_at; there is no DELETE.
func (s Server) benchAreaExceptionsPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if !benchCatalogWriter(client) {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	defer r.Body.Close()
	// The body is buffered so a decode failure can be re-walked to name the
	// field that failed, exactly like the catalog path. One byte past the
	// limit is read on purpose: a body that fills the cap is refused as too
	// large instead of silently decoding a truncated prefix.
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(benchRequestMaxBytes+4096)+1))
	if err != nil {
		appErr(w, r, err)
		return
	}
	if len(body) > benchRequestMaxBytes+4096 {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_bench_area_exceptions_json", "detail": "body exceeds the request limit"})
		return
	}
	var input benchAreaExceptionsInput
	de := json.NewDecoder(bytes.NewReader(body))
	de.DisallowUnknownFields()
	if err := de.Decode(&input); err != nil {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_bench_area_exceptions_json", "detail": benchListDecodeDetail[store.BenchAreaException](body, "exceptions", benchAreaExceptionFieldTypes, err)})
		return
	}
	var extra json.RawMessage
	if err := de.Decode(&extra); err != io.EOF {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_bench_area_exceptions_json", "detail": "trailing data after the request object"})
		return
	}
	if !benchBatchValid(len(input.Exceptions)) {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	n, err := s.Service.UpsertBenchAreaExceptions(r.Context(), client, input.Exceptions)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"upserted": n})
}

// benchRowFieldTypes maps each JSON field name of T to its Go type so a
// decode failure can be re-walked field by field.
func benchRowFieldTypes[T any]() map[string]reflect.Type {
	t := reflect.TypeOf(*new(T))
	m := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			m[name] = t.Field(i).Type
		}
	}
	return m
}

var (
	benchCatalogEntryFieldTypes  = benchRowFieldTypes[store.BenchCatalogEntry]()
	benchAreaExceptionFieldTypes = benchRowFieldTypes[store.BenchAreaException]()
)

// benchListDecodeDetail renders a bench-list PUT decode failure for the
// operator. When the decoder's own message already names the field (unknown
// fields, type mismatches) it is returned as-is; when it does not — for
// example a time.ParseError from a non-RFC3339 decided_at — the buffered
// body is re-walked to find the first field that fails. Only the field name
// and the decoder's message are reported, never row contents.
func benchListDecodeDetail[T any](body []byte, listKey string, fieldTypes map[string]reflect.Type, cause error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(cause, &typeErr) || strings.HasPrefix(cause.Error(), "json: unknown field") {
		return cause.Error()
	}
	var raw map[string][]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return cause.Error()
	}
	for i, row := range raw[listKey] {
		var entry T
		de := json.NewDecoder(bytes.NewReader(row))
		de.DisallowUnknownFields()
		if err := de.Decode(&entry); err != nil {
			if field := benchRowErrorField(row, fieldTypes); field != "" {
				return fmt.Sprintf("%s[%d] field %s", listKey, i, field)
			}
			return fmt.Sprintf("%s[%d]: %v", listKey, i, err)
		}
	}
	return cause.Error()
}

// benchCatalogDecodeDetail renders a catalog PUT decode failure via
// benchListDecodeDetail with the catalog envelope key.
func benchCatalogDecodeDetail(body []byte, cause error) string {
	return benchListDecodeDetail[store.BenchCatalogEntry](body, "catalog", benchCatalogEntryFieldTypes, cause)
}

// benchRowErrorField re-decodes one bench list row field by field, in
// document order, and returns `"name": <decoder message>` for the first
// field that fails against its Go type in fieldTypes. It returns "" when no
// single field explains the failure — unknown fields and type errors are
// already named by the decoder's own message.
func benchRowErrorField(row []byte, fieldTypes map[string]reflect.Type) string {
	de := json.NewDecoder(bytes.NewReader(row))
	tok, err := de.Token()
	if err != nil {
		return ""
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return ""
	}
	for de.More() {
		key, err := de.Token()
		if err != nil {
			return ""
		}
		var value json.RawMessage
		if err := de.Decode(&value); err != nil {
			return ""
		}
		name, _ := key.(string)
		fieldType, ok := fieldTypes[name]
		if !ok {
			continue
		}
		if err := json.Unmarshal(value, reflect.New(fieldType).Interface()); err != nil {
			return fmt.Sprintf("%q: %v", name, err)
		}
	}
	return ""
}

// Payload caps for decode, by what the body carries. A route picks one of
// these or a store-owned limit (store.MaxBytes, benchRequestMaxBytes, ...).
const (
	bodyTiny     = 1 << 10  // an id or two and a short reason
	bodyStanding = 8 << 10  // standing order
	bodySmall    = 4 << 10  // one entity's short fields
	bodyEvent    = 16 << 10 // one external event with its references (16 review items fill ~11 KiB)
	bodyLarge    = 64 << 10 // free text or a list: descriptions, candidates
	bodyPRState  = 2 << 20  // one PR-state write: ~200 checks, 100 threads and a pass of findings (hk 3198)
)

// decode reads one JSON request body into v. max is the payload's own byte
// cap; 4 KiB of slack covers the envelope keys around it.
func decode(r *http.Request, v any, max int) error {
	return decodeLimited(r.Body, v, int64(max+4096))
}

// decodeLimited is the one JSON body decoder: at most limit bytes, unknown
// fields rejected, and exactly one value with nothing after it.
func decodeLimited(body io.ReadCloser, v any, limit int64) error {
	de := json.NewDecoder(http.MaxBytesReader(nil, body, limit))
	de.DisallowUnknownFields()
	if err := de.Decode(v); err != nil {
		return err
	}
	if de.Decode(new(json.RawMessage)) != io.EOF {
		return errors.New("trailing data after JSON body")
	}
	return nil
}
func queryLimit(r *http.Request, def, max int) (int, error) {
	x := def
	if v := r.URL.Query().Get("limit"); v != "" {
		var e error
		x, e = strconv.Atoi(v)
		if e != nil || x < 1 || x > max {
			return 0, errors.New("limit")
		}
	}
	return x, nil
}
func (s Server) checkpoints(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		defer r.Body.Close()
		var x store.Checkpoint
		if e := decode(r, &x, store.MaxBytes); e != nil {
			appErr(w, r, e)
			return
		}
		x, e := s.Service.Checkpoint(r.Context(), client, x)
		if e != nil {
			appErr(w, r, e)
			return
		}
		jsonOut(w, http.StatusCreated, x)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	n, e := queryLimit(r, 3, store.CheckpointKeep)
	if e != nil {
		appErr(w, r, e)
		return
	}
	xs, e := s.Service.Recent(r.Context(), r.URL.Query().Get("session"), r.URL.Query().Get("kind"), n)
	if e != nil {
		appErr(w, r, e)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"checkpoints": xs})
}
func (s Server) search(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	n, e := queryLimit(r, 20, 100)
	if e != nil {
		appErr(w, r, e)
		return
	}
	xs, e := s.Service.Search(r.Context(), r.URL.Query().Get("q"), defaultString(r.URL.Query().Get("scope"), "all"), r.URL.Query().Get("session"), n)
	if e != nil {
		appErr(w, r, e)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"results": xs})
}
func (s Server) documents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	// ?id=<n> fetches one document by numeric id (#551): key is the public
	// handle but callers often only have the id. A document id is never a
	// valid key, so the {key...} wildcard route cannot express this lookup.
	if r.URL.Query().Has("id") {
		raw := r.URL.Query().Get("id")
		id, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || id < 1 {
			writeError(w, http.StatusBadRequest, "invalid_document_id")
			return
		}
		x, found, e := s.Service.GetDocumentByID(r.Context(), id)
		if e != nil {
			appErr(w, r, e)
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		jsonOut(w, http.StatusOK, x)
		return
	}
	n, e := queryLimit(r, 100, 1000)
	if e != nil {
		appErr(w, r, e)
		return
	}
	var task int64
	if raw := r.URL.Query().Get("task"); raw != "" {
		if task, e = strconv.ParseInt(raw, 10, 64); e != nil || task < 1 {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
	}
	xs, e := s.Service.ListDocuments(r.Context(), r.URL.Query().Get("prefix"), r.URL.Query().Get("kind"), r.URL.Query().Get("session"), task, n)
	if e != nil {
		appErr(w, r, e)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"documents": xs})
}
func (s Server) document(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if r.Method == http.MethodGet {
		x, found, e := s.Service.GetDocument(r.Context(), key)
		if e != nil {
			appErr(w, r, e)
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		jsonOut(w, http.StatusOK, x)
		return
	}
	if r.Method == http.MethodPut {
		defer r.Body.Close()
		var x store.Document
		if e := decode(r, &x, store.DocumentMaxBytes); e != nil {
			appErr(w, r, e)
			return
		}
		x.Key = key
		x, changed, e := s.Service.PutDocument(r.Context(), client, x)
		if e != nil {
			appErr(w, r, e)
			return
		}
		jsonOut(w, http.StatusOK, map[string]any{"document": x, "changed": changed})
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
}
func (s Server) memoryList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	xs, e := s.Service.ListMemory(r.Context(), r.PathValue("agent"), false)
	if e != nil {
		appErr(w, r, e)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"memory": xs})
}
func (s Server) memory(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	agent, name := r.PathValue("agent"), r.PathValue("name")
	if r.Method == http.MethodGet {
		x, found, e := s.Service.GetMemory(r.Context(), agent, name)
		if e != nil {
			appErr(w, r, e)
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		jsonOut(w, http.StatusOK, x)
		return
	}
	if r.Method == http.MethodPut {
		defer r.Body.Close()
		var x store.Memory
		if e := decode(r, &x, store.MaxBytes); e != nil {
			appErr(w, r, e)
			return
		}
		x.Agent, x.Name = agent, name
		x, e := s.Service.PutMemory(r.Context(), client, x)
		if e != nil {
			appErr(w, r, e)
			return
		}
		jsonOut(w, http.StatusOK, x)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
}
func defaultString(x, d string) string {
	if x == "" {
		return d
	}
	return x
}
func (s Server) attachmentPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	if s.Service.Attachments == nil || !s.Service.Attachments.Enabled {
		appErr(w, r, attachments.ErrDisabled)
		return
	}
	max := s.Service.Attachments.Config.MaxBytes
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if e != nil {
		appErr(w, r, attachments.ErrTooLarge)
		return
	}
	x, new, e := s.Service.PutAttachment(r.Context(), client, r.Header.Get("X-HK-Name"), r.Header.Get("Content-Type"), r.Header.Get("X-HK-Ref"), b)
	if e != nil {
		appErr(w, r, e)
		return
	}
	jsonOut(w, http.StatusCreated, map[string]any{"attachment": x, "created": new, "url": "/v1/attachments/" + x.SHA256})
}
func (s Server) attachments(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	xs, e := s.Service.ListAttachments(r.Context(), r.URL.Query().Get("ref"), 100)
	if e != nil {
		appErr(w, r, e)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"attachments": xs})
}
func (s Server) attachment(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	sha := r.PathValue("sha")
	if r.URL.Query().Get("presign") == "1" {
		u, e := s.Service.AttachmentURL(r.Context(), sha)
		if e != nil {
			appErr(w, r, e)
			return
		}
		http.Redirect(w, r, u, http.StatusFound)
		return
	}
	x, body, e := s.Service.GetAttachment(r.Context(), sha)
	if e != nil {
		if e.Error() == "attachment_not_found" {
			writeError(w, http.StatusNotFound, "not_found")
		} else {
			appErr(w, r, e)
		}
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", x.MIME)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(x.OriginalName, "\"", "")+`"`)
	_, _ = io.Copy(w, body)
}
func (s Server) usage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	if s.Service.Attachments == nil || !s.Service.Attachments.Enabled {
		jsonOut(w, http.StatusOK, map[string]any{"attachments": "disabled"})
		return
	}
	u, e := s.Service.AttachmentUsage(r.Context())
	if e != nil {
		appErr(w, r, e)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"attachments": "enabled", "usage": u, "cap_bytes": s.Service.Attachments.Config.StorageCap, "cap_ratio": float64(u.TotalBytes) / float64(s.Service.Attachments.Config.StorageCap)})
}
func (s Server) metrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	if s.Service.Attachments == nil || !s.Service.Attachments.Enabled {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("handoffkeep_attach_enabled 0\n"))
		s.ClientRevisions.writeMetrics(w)
		return
	}
	u, e := s.Service.AttachmentUsage(r.Context())
	if e != nil {
		appErr(w, r, e)
		return
	}
	c := s.Service.Attachments.Config
	objects, e := s.Service.Store.AttachmentObjectCount(r.Context())
	if e != nil {
		appErr(w, r, e)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "handoffkeep_attach_bytes_total %d\nhandoffkeep_attach_objects %d\nhandoffkeep_attach_puts_month %d\nhandoffkeep_attach_gets_month %d\nhandoffkeep_attach_cap_bytes %d\nhandoffkeep_attach_cap_ratio %.8f\n", u.TotalBytes, objects, u.Puts, u.Gets, c.StorageCap, float64(u.TotalBytes)/float64(c.StorageCap))
	s.ClientRevisions.writeMetrics(w)
}

// taskReopen preserves the reason and authenticated identity in task history.
func (s Server) taskReopen(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var input struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.ReopenTask(r.Context(), id, client, input.Reason)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

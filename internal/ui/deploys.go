package ui

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The deploy-pending surface is read-only: it shows, per service, the last
// recorded deploy and the tasks that reached merged after it. Record shape is
// installer R12's deploy-record/v0; the read-side rule (current version = the
// newest result=success record by deployed_at) is admiral INSTALLER.md
// §배포 기록(hk). The heading must stay "마지막 배포 이후 머지됨" — never
// "미배포 확정" (design/2026-09-21/deploy-pending-surface §2 advisory).
const (
	deployRecordSchema = "deploy-record/v0"
	deployDocsPerSvc   = 200
	// deployDocsHardCap bounds the scan when the first window comes back
	// full — deployed_at can reorder against record keys, so every document
	// up to this bound is compared before current is asserted.
	deployDocsHardCap = 2000
	deployEventsLimit = 1000
)

// deployService maps the v0 services onto the GitHub repo a refs.pr value must
// match exactly. Partial matches never attribute a task.
var deployServices = []struct {
	Name string
	Repo string
}{
	{"handoffkeep", "mgh3326/handoffkeep"},
	{"auto_trader", "mgh3326/auto_trader"},
	{"panewire-hub", "mgh3326/panewire"},
}

var deployPRRepoRE = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/[0-9]+$`)

// deployRecord mirrors the installer R12 body. Unknown fields are ignored;
// fields the writer did not know arrive as JSON null.
type deployRecord struct {
	Schema      string   `json:"schema"`
	Service     string   `json:"service"`
	Target      string   `json:"target"`
	HeadSHA     *string  `json:"head_sha"`
	DeployedRef *string  `json:"deployed_ref"`
	PreviousRef *string  `json:"previous_ref"`
	DeployedAt  *string  `json:"deployed_at"`
	Result      string   `json:"result"`
	FailedStep  *int     `json:"failed_step"`
	JobID       *string  `json:"job_id"`
	ApprovalRef *string  `json:"approval_ref"`
	IncludedPRs []string `json:"included_prs"`
	Source      string   `json:"source"`
}

// deployRecordKeyTime extracts the UTC timestamp embedded in a
// deploy/<service>/<YYYYMMDDTHHMMSSZ> record key. It orders records when a
// record's own deployed_at is absent.
func deployRecordKeyTime(key string) (time.Time, bool) {
	stamp := key[strings.LastIndex(key, "/")+1:]
	t, err := time.Parse("20060102T150405Z", stamp)
	return t, err == nil
}

// parseDeployRecord validates one document body as a deploy record for the
// expected service. A record whose service field disagrees with its key, or
// whose schema/result is outside the contract, is not a deploy record for
// this view.
func parseDeployRecord(key, service, body string) (*deployRecord, *time.Time, bool) {
	var rec deployRecord
	if err := json.Unmarshal([]byte(body), &rec); err != nil {
		return nil, nil, false
	}
	if rec.Schema != deployRecordSchema || rec.Service != service {
		return nil, nil, false
	}
	switch rec.Result {
	case "success", "failed", "rolled_back":
	default:
		return nil, nil, false
	}
	var deployedAt *time.Time
	if rec.DeployedAt != nil {
		parsed, err := time.Parse(time.RFC3339, *rec.DeployedAt)
		if err != nil {
			return nil, nil, false
		}
		deployedAt = &parsed
	}
	return &rec, deployedAt, true
}

// deployRepoOfPR returns the "owner/repo" a refs.pr GitHub URL points at.
// Exact match only — any other shape returns "".
func deployRepoOfPR(pr string) string {
	m := deployPRRepoRE.FindStringSubmatch(pr)
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}

type deployRecordView struct {
	RecordKey   string     `json:"record_key"`
	Result      string     `json:"result"`
	DeployedRef *string    `json:"deployed_ref"`
	DeployedAt  *time.Time `json:"deployed_at"`
	FailedStep  *int       `json:"failed_step"`
	RecordedAt  time.Time  `json:"recorded_at"`
	Source      string     `json:"source"`
	// ServingMaybeChanged is the R12 caveat: a failed record with
	// failed_step >= 6 ran the deploy command before stopping, so the serving
	// version may already differ from Current.
	ServingMaybeChanged bool `json:"serving_maybe_changed,omitempty"`
}

type deployMergedTask struct {
	TaskID   int64     `json:"task_id"`
	Title    string    `json:"title"`
	PR       string    `json:"pr"`
	MergedAt time.Time `json:"merged_at"`
}

type deployServiceView struct {
	Service string `json:"service"`
	Repo    string `json:"repo"`
	Target  string `json:"target,omitempty"`
	DocURL  string `json:"doc_url"`
	// RecordCount covers every document scanned under deploy/<service>/ —
	// including ones this view could not parse (InvalidCount). When
	// DocsCapped is set the space may hold more than were scanned.
	RecordCount  int `json:"record_count"`
	InvalidCount int `json:"invalid_count,omitempty"`
	// DocsCapped is set when the scan stopped while older documents may
	// remain — either the first window was full, or the deeper scan hit its
	// hard cap before finding a success or reaching the end of the space.
	DocsCapped bool              `json:"docs_capped,omitempty"`
	Current    *deployRecordView `json:"current"`
	// Latest is the newest parseable record overall — present whenever it is
	// not the current one, so a failed/rolled_back attempt after the last
	// success is visible instead of hidden behind the good deploy.
	Latest *deployRecordView `json:"latest,omitempty"`
	// MergedBoundary is the deployed_at instant the merged list is measured
	// against: "deployed_at", "unrecorded" (current record's deployed_at is
	// null — the list cannot be computed), or "no_current" (no success
	// record exists at all).
	MergedBoundary string             `json:"merged_boundary"`
	MergedSince    []deployMergedTask `json:"merged_since"`
}

type deployPendingResponse struct {
	GeneratedAt time.Time           `json:"generated_at"`
	PRSource    string              `json:"pr_source"`
	Services    []deployServiceView `json:"services"`
	// EventsCapped is set when the merged-events scan hit its bound and the
	// oldest scanned event is still after some service's boundary — only then
	// is a merged_since list actually known to be a prefix, not the full set.
	EventsCapped bool `json:"events_capped,omitempty"`
}

func (h *Handler) deployPending(w http.ResponseWriter, r *http.Request) {
	// Fetch one row past the bound: "returned full" must mean an event was
	// actually left unscanned. At exactly the bound nothing was omitted and
	// no truncation warning is owed.
	events, err := h.store.ListMergedTaskEvents(r.Context(), deployEventsLimit+1)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	response := deployPendingResponse{
		GeneratedAt: time.Now().UTC(),
		PRSource:    "refs.pr",
		Services:    []deployServiceView{},
	}
	scanCapped := len(events) > deployEventsLimit
	if scanCapped {
		events = events[:deployEventsLimit]
	}
	for _, svc := range deployServices {
		view, err := h.deployServiceView(r, svc.Name, svc.Repo, events)
		if err != nil {
			http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
			return
		}
		// The DESC-ordered scan truncates this service's list only when its
		// oldest scanned event is still after the boundary — a full cap with
		// an older tail means the list is complete and must not warn.
		if scanCapped && view.Current != nil && view.Current.DeployedAt != nil &&
			events[len(events)-1].At.After(*view.Current.DeployedAt) {
			response.EventsCapped = true
		}
		response.Services = append(response.Services, view)
	}
	writeBoardJSON(w, response)
}

// deployParsedDoc is one document under deploy/<service>/ that parsed as a
// valid deploy-record/v0 body for that service.
type deployParsedDoc struct {
	key        string
	rec        *deployRecord
	deployedAt *time.Time
	keyTime    time.Time
	recordedAt time.Time
}

// deployServiceView assembles one service's block. Record selection follows
// the contract: the current serving version is the newest result=success
// record ordered by deployed_at (key time when a record did not measure one);
// the newest record overall is surfaced separately so a failed attempt never
// disappears behind it.
func (h *Handler) deployServiceView(r *http.Request, service, repo string, events []store.MergedTaskEvent) (deployServiceView, error) {
	view := deployServiceView{
		Service:        service,
		Repo:           repo,
		DocURL:         "/ui/doc/deploy/" + service + "/",
		MergedBoundary: "no_current",
		MergedSince:    []deployMergedTask{},
	}
	prefix := "deploy/" + service + "/"
	// One row past the bound distinguishes "exactly full" from "truncated" —
	// at exactly the bound nothing was omitted and no warning is owed.
	docs, err := h.store.ListDocumentsByPrefix(r.Context(), prefix, deployDocsPerSvc+1)
	if err != nil {
		return view, err
	}
	view.DocsCapped = len(docs) > deployDocsPerSvc
	if view.DocsCapped {
		docs = docs[:deployDocsPerSvc]
	}
	view.RecordCount = len(docs)
	parseDocs := func(page []store.Document) []deployParsedDoc {
		out := []deployParsedDoc{}
		for _, doc := range page {
			rec, deployedAt, ok := parseDeployRecord(doc.Key, service, doc.Body)
			if !ok {
				view.InvalidCount++
				continue
			}
			keyTime, _ := deployRecordKeyTime(doc.Key)
			out = append(out, deployParsedDoc{key: doc.Key, rec: rec, deployedAt: deployedAt, keyTime: keyTime, recordedAt: doc.CreatedAt})
		}
		return out
	}
	parsed := parseDocs(docs)
	// A full first window is never proof of completeness — deployed_at can
	// reorder against keys (a record's key is write time, its deployed_at is
	// measured), so the true current success may sit on any page. Drain to
	// deployDocsHardCap; the flag clears only when a short page proves the
	// scan reached the end of the key space.
	if view.DocsCapped {
		cursor := docs[len(docs)-1].Key
		for view.DocsCapped && view.RecordCount < deployDocsHardCap {
			var older []store.Document
			older, err = h.store.ListDocumentsByPrefixBefore(r.Context(), prefix, cursor, deployDocsPerSvc+1)
			if err != nil {
				return view, err
			}
			if len(older) > deployDocsPerSvc {
				older = older[:deployDocsPerSvc]
			} else {
				view.DocsCapped = false
			}
			view.RecordCount += len(older)
			if len(older) == 0 {
				break
			}
			cursor = older[len(older)-1].Key
			parsed = append(parsed, parseDocs(older)...)
		}
	}
	if len(parsed) == 0 {
		return view, nil
	}
	// Both selections are by contract time (deployed_at, key time fallback),
	// not document order — a failed attempt whose deployed_at postdates the
	// last success must surface even when its key sorts earlier.
	latest := parsed[0]
	var current *deployParsedDoc
	for i := range parsed {
		p := &parsed[i]
		if deployRecordLess(latest, *p) {
			latest = *p
		}
		if p.rec.Result != "success" {
			continue
		}
		if current == nil || deployRecordLess(*current, *p) {
			current = p
		}
	}
	view.Target = latest.rec.Target
	toView := func(p deployParsedDoc) deployRecordView {
		return deployRecordView{
			RecordKey:           p.key,
			Result:              p.rec.Result,
			DeployedRef:         p.rec.DeployedRef,
			DeployedAt:          p.deployedAt,
			FailedStep:          p.rec.FailedStep,
			RecordedAt:          p.recordedAt.UTC(),
			Source:              p.rec.Source,
			ServingMaybeChanged: p.rec.Result == "failed" && p.rec.FailedStep != nil && *p.rec.FailedStep >= 6,
		}
	}
	if current != nil {
		cur := toView(*current)
		view.Current = &cur
		if latest.key != current.key {
			lv := toView(latest)
			view.Latest = &lv
		}
		if current.deployedAt != nil {
			view.MergedBoundary = "deployed_at"
			view.MergedSince = mergedTasksSince(events, repo, *current.deployedAt)
		} else {
			// The current version is known but its deploy time is not —
			// "merged since" has no honest boundary to measure from.
			view.MergedBoundary = "unrecorded"
		}
	} else {
		lv := toView(latest)
		view.Latest = &lv
	}
	return view, nil
}

// deployRecordLess orders success records by deployed_at, falling back to the
// record-key timestamp when deployed_at is null (observed on backfill
// records). Keys sort chronologically, so the tiebreak keeps first-seen-newest
// from ListDocumentsByPrefix.
func deployRecordLess(a, b deployParsedDoc) bool {
	at, bt := a.keyTime, b.keyTime
	if a.deployedAt != nil {
		at = *a.deployedAt
	}
	if b.deployedAt != nil {
		bt = *b.deployedAt
	}
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return a.key < b.key
}

// mergedTasksSince filters merged events to one service's repo. The boundary
// is strict (at > deployed_at): a merge at the deploy instant is already in
// the deployed build and must not appear. A NULL refs snapshot or a non-URL
// refs.pr attributes the task to no service.
func mergedTasksSince(events []store.MergedTaskEvent, repo string, since time.Time) []deployMergedTask {
	out := []deployMergedTask{}
	for _, event := range events {
		if !event.At.After(since) {
			continue
		}
		if event.Refs == nil || deployRepoOfPR(event.Refs.PR) != repo {
			continue
		}
		out = append(out, deployMergedTask{TaskID: event.TaskID, Title: event.Title, PR: event.Refs.PR, MergedAt: event.At.UTC()})
	}
	return out
}

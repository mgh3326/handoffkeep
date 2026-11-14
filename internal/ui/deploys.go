package ui

import (
	"net/http"
	"regexp"
	"sort"
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
	deployDocsPerSvc = 200
	// deployDocsHardCap bounds the scan when the first window comes back
	// full — deployed_at can reorder against record keys, so every document
	// up to this bound is compared before current is asserted.
	deployDocsHardCap = 2000
	deployEventsLimit = 1000
)

var deployPRRepoRE = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/[0-9]+$`)

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
	RecordKey string `json:"record_key"`
	Result    string `json:"result"`
	// Target is the environment the record's attempt names (hk-ship/hk-follow
	// write "ncp", "mbp", …). Empty on a record that did not name one.
	Target      string     `json:"target,omitempty"`
	HeadSHA     *string    `json:"head_sha"`
	DeployedRef *string    `json:"deployed_ref"`
	DeployedAt  *time.Time `json:"deployed_at"`
	FailedStep  *int       `json:"failed_step"`
	// FailedStepName is the writer's name for the failed step — the one
	// failure reason a deploy record carries. Null when the writer did not
	// record one (older installer records).
	FailedStepName *string   `json:"failed_step_name"`
	RecordedAt     time.Time `json:"recorded_at"`
	Source         string    `json:"source"`
	// IncludedPRs is the record's frozen PR list: the PRs the running build
	// carries. It is the PR→sha link of the task→PR→sha→deploy chain.
	IncludedPRs []string `json:"included_prs"`
	// ServingMaybeChanged is the R12 caveat: a failed record with
	// failed_step >= 6 ran the deploy command before stopping, so the serving
	// version may already differ from Current.
	ServingMaybeChanged bool `json:"serving_maybe_changed,omitempty"`
}

// deployInvalidRecord is one scanned document that is not a deploy record —
// the key plus every failed check, so the operator sees what to rewrite.
type deployInvalidRecord struct {
	Key     string   `json:"key"`
	Prefix  string   `json:"prefix"`
	Reasons []string `json:"reasons"`
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
	// Invalid lists every scanned document that failed the record contract,
	// newest key first, with the reason classes that failed — the count-only
	// warning could never say which key to fix (#620 AC4).
	Invalid []deployInvalidRecord `json:"invalid,omitempty"`
	// DocsCapped is set when the scan stopped while older documents may
	// remain — either the first window was full, or the deeper scan hit its
	// hard cap before finding a success or reaching the end of the space.
	DocsCapped bool               `json:"docs_capped,omitempty"`
	Current    *deployRecordView  `json:"current"`
	Recent     []deployRecordView `json:"recent"`
	// Latest is the newest parseable record overall — present whenever it is
	// not the current one, so a failed/rolled_back attempt after the last
	// success is visible instead of hidden behind the good deploy.
	Latest *deployRecordView `json:"latest,omitempty"`
	// MergedBoundary names the instant the merged list is measured against,
	// the same one DeployLagStatus counts lag from: "deployed_at", or
	// "deploy_record_key" (the current record carries no deployed_at, so the
	// timestamp in its key stands in), or "no_current" (no success record
	// exists at all).
	MergedBoundary string             `json:"merged_boundary"`
	MergedSince    []deployMergedTask `json:"merged_since"`
	LagCount       int                `json:"lag_count,omitempty"`
	Lagging        bool               `json:"lagging,omitempty"`
	// State is what the records prove about the newest attempt, in the order
	// the operator reads it: the latest attempt's outcome first, then whether
	// merges are waiting for a deploy. States that a deploy record cannot
	// carry (not built, disabled, check failed, awaiting approval — holds and
	// refusals write no record, deploy/hk-follow/README.md §Report shape) are
	// never invented here.
	State string `json:"state"`
	// mergedFrom is the instant MergedBoundary names, nil for "no_current".
	// The caller reads it to tell a truncated merged scan from a complete one.
	mergedFrom *time.Time
}

// The states deployServiceView.State can hold.
const (
	DeployStateDeployed       = "deployed"        // newest attempt succeeded
	DeployStatePending        = "pending"         // newest attempt succeeded; merges wait for the next deploy
	DeployStateDeployFailed   = "deploy_failed"   // newest attempt failed before its deploy step reported success
	DeployStateHealthFailed   = "health_failed"   // newest attempt served and failed its health check; the row names the step
	DeployStateServingUnknown = "serving_unknown" // newest attempt failed and the record cannot say which version serves
	DeployStateNoRecord       = "no_record"       // no parseable deploy record
	DeployStatePartial        = "partial"         // the scan stopped early: the newest record may be unscanned
)

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
	response, err := h.pendingDeploys(r)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	writeBoardJSON(w, response)
}

// pendingDeploys assembles the deploy-pending document. Both
// /ui/api/deploy-pending and /ui/api/attention serve from it.
func (h *Handler) pendingDeploys(r *http.Request) (deployPendingResponse, error) {
	// Fetch one row past the bound: "returned full" must mean an event was
	// actually left unscanned. At exactly the bound nothing was omitted and
	// no truncation warning is owed.
	events, err := h.store.ListMergedTaskEvents(r.Context(), deployEventsLimit+1)
	if err != nil {
		return deployPendingResponse{}, err
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
	services, err := h.store.ListDeployServices(r.Context())
	if err != nil {
		return deployPendingResponse{}, err
	}
	for _, svc := range services {
		if !svc.Active {
			continue
		}
		view, err := h.deployServiceView(r, svc, events)
		if err != nil {
			return deployPendingResponse{}, err
		}
		// The DESC-ordered scan truncates this service's list only when its
		// oldest scanned event is still after the boundary — a full cap with
		// an older tail means the list is complete and must not warn.
		if scanCapped && view.mergedFrom != nil && events[len(events)-1].At.After(*view.mergedFrom) {
			response.EventsCapped = true
		}
		response.Services = append(response.Services, view)
	}
	return response, nil
}

// deployParsedDoc is one document under deploy/<service>/ that parsed as a
// valid deploy-record/v0 body for that service.
type deployParsedDoc struct {
	key        string
	rec        *store.DeployRecord
	deployedAt *time.Time
	keyTime    time.Time
	recordedAt time.Time
}

// deployServiceView assembles one service's block. Record selection follows
// the contract: the current serving version is the newest result=success
// record ordered by deployed_at (key time when a record did not measure one);
// the newest record overall is surfaced separately so a failed attempt never
// disappears behind it.
func (h *Handler) deployServiceView(r *http.Request, svc store.DeployService, events []store.MergedTaskEvent) (deployServiceView, error) {
	service, repo := svc.Name, svc.Repo
	view := deployServiceView{
		Service:        service,
		Repo:           repo,
		DocURL:         "/ui/doc/deploy/" + service + "/",
		MergedBoundary: "no_current",
		MergedSince:    []deployMergedTask{},
		Recent:         []deployRecordView{},
		State:          DeployStateNoRecord,
	}
	parsed := []deployParsedDoc{}
	seen := map[string]bool{}
	for _, prefix := range svc.RecordPrefixes {
		cursor := ""
		prefixCount := 0
		for {
			var docs []store.StoredDeployRecord
			var err error
			if cursor == "" {
				docs, err = h.store.ListDeployRecords(r.Context(), prefix, "", deployDocsPerSvc+1)
			} else {
				docs, err = h.store.ListDeployRecords(r.Context(), prefix, cursor, deployDocsPerSvc+1)
			}
			if err != nil {
				return view, err
			}
			more := len(docs) > deployDocsPerSvc
			if more {
				docs = docs[:deployDocsPerSvc]
			}
			for _, doc := range docs {
				if seen[doc.Key] {
					continue
				}
				seen[doc.Key] = true
				view.RecordCount++
				rec, deployedAt, reasons := doc.Classify(service)
				if len(reasons) > 0 {
					alias := strings.TrimSuffix(prefix, "/")
					alias = alias[strings.LastIndex(alias, "/")+1:]
					if alias != service {
						rec, deployedAt, reasons = doc.Classify(alias)
					}
				}
				if len(reasons) > 0 {
					view.InvalidCount++
					view.Invalid = append(view.Invalid, deployInvalidRecord{Key: doc.Key, Prefix: prefix, Reasons: reasons})
					continue
				}
				keyTime, _ := store.DeployRecordKeyTime(doc.Key)
				parsed = append(parsed, deployParsedDoc{key: doc.Key, rec: rec, deployedAt: deployedAt, keyTime: keyTime, recordedAt: doc.CreatedAt})
			}
			prefixCount += len(docs)
			if !more || len(docs) == 0 {
				break
			}
			if prefixCount >= deployDocsHardCap {
				view.DocsCapped = true
				break
			}
			cursor = docs[len(docs)-1].Key
		}
	}
	if len(parsed) == 0 {
		// No parseable record in the scanned window. Under a capped scan that
		// is not "no record": a valid record may sit beyond the cap, so the
		// view must stay partial rather than claim there is none.
		if view.DocsCapped {
			view.State = DeployStatePartial
		}
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
			Target:              p.rec.Target,
			HeadSHA:             p.rec.HeadSHA,
			DeployedRef:         p.rec.DeployedRef,
			DeployedAt:          p.deployedAt,
			FailedStep:          p.rec.FailedStep,
			FailedStepName:      p.rec.FailedStepName,
			RecordedAt:          p.recordedAt.UTC(),
			Source:              p.rec.Source,
			IncludedPRs:         append([]string{}, p.rec.IncludedPRs...),
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
		// The boundary is the one DeployLagStatus counts lag from:
		// deployed_at, else the record key's timestamp (a parsed record's
		// key always carries one). A list measured from anywhere else would
		// disagree with lagging and with the state it feeds.
		boundary := current.keyTime
		view.MergedBoundary = "deploy_record_key"
		if current.deployedAt != nil {
			boundary = *current.deployedAt
			view.MergedBoundary = "deployed_at"
		}
		view.mergedFrom = &boundary
		view.MergedSince = mergedTasksSince(events, repo, boundary)
	} else {
		lv := toView(latest)
		view.Latest = &lv
	}
	// Lag and the merged-since list share one boundary (the last successful
	// deploy), the same one the operator-desk notice uses. A service without
	// one has unknown lag, not all-time lag.
	st, err := h.store.DeployLagStatus(r.Context(), svc)
	if err != nil {
		return deployServiceView{}, err
	}
	view.LagCount = st.LagCount
	view.Lagging = st.Lagging
	sort.Slice(parsed, func(i, j int) bool { return deployRecordLess(parsed[j], parsed[i]) })
	for _, p := range parsed {
		if len(view.Recent) == 10 {
			break
		}
		view.Recent = append(view.Recent, toView(p))
	}
	view.State = deployServiceState(view.DocsCapped, latest.rec, len(view.MergedSince))
	return view, nil
}

// deployServiceState names what the newest record and the merged-since list
// prove. A success with merges waiting is "pending" — the next deploy has
// work, not that this one failed. A capped scan cannot name the newest
// attempt at all, so it stays "partial" and the panel scopes every value.
// An interrupted tick wrote a failed step-6 record without knowing which
// services flipped (deploy/hk-follow/README.md §Report shape item 2), so it
// gets "serving_unknown" rather than a state that claims the old version
// serves.
func deployServiceState(docsCapped bool, latest *store.DeployRecord, mergedSince int) string {
	if docsCapped {
		return DeployStatePartial
	}
	switch {
	case latest.Result == "success":
		if mergedSince > 0 {
			return DeployStatePending
		}
		return DeployStateDeployed
	case latest.FailedStepName != nil && *latest.FailedStepName == "interrupted":
		return DeployStateServingUnknown
	case latest.Result == "rolled_back":
		return DeployStateHealthFailed
	case latest.FailedStep != nil && *latest.FailedStep >= 7:
		// 7 (healthz, rolled back) and 8 (rollback failed) both mean the new
		// version served and failed its health check. 6 is the deploy command
		// itself failing or an interrupted tick (deploy/hk-follow/README.md
		// §Report shape), whose serving state the record does not prove.
		return DeployStateHealthFailed
	default:
		return DeployStateDeployFailed
	}
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

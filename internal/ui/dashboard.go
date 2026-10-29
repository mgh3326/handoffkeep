package ui

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// dashboard.go — GET /ui/api/dashboard is the read-only aggregation behind the
// console home page (hk 2280). It joins what hk already stores:
//
//   - quota_latest / quota_observations (pacelever's shared quota publishes)
//   - tasks in an active state, and unfinished tasks that carry refs.pr
//   - the latest merge event of recently merged tasks (ListMergedTaskEvents)
//
// Hosts and running hub jobs are not repeated here: the page reads them from
// /ui/api/live, which already joins hub /v1/nodes and /v1/jobs to tasks.
// Sources hk does not have are listed in Missing rather than filled in.

const (
	dashboardQuotaLimit  = 1000
	dashboardTaskLimit   = 500
	dashboardMergedScan  = 200
	dashboardMergedMax   = 30
	dashboardMergedSince = 7 * 24 * time.Hour
)

// dashboardActiveStates are the task states shown as running work.
var dashboardActiveStates = map[string]bool{"claimed": true, "in_progress": true, "verifying": true, "join": true}

type dashboardResponse struct {
	GeneratedAt    string             `json:"generated_at"`
	Quota          []dashboardAccount `json:"quota"`
	QuotaTruncated bool               `json:"quota_truncated,omitempty"`
	Work           []dashboardTask    `json:"work"`
	PRs            []dashboardRepo    `json:"prs"`
	TasksTruncated bool               `json:"tasks_truncated,omitempty"`
	Missing        []dashboardMissing `json:"missing"`
}

// dashboardAccount is one quota account key (<pool>:<fingerprint>). Auto is
// the latest automatic measurement; Manual holds the per-window manual entries.
type dashboardAccount struct {
	AccountKey string             `json:"account_key"`
	Pool       string             `json:"pool"`
	Auto       *dashboardQuotaObs `json:"auto"`
	Manual     []dashboardManual  `json:"manual"`
}

type dashboardQuotaObs struct {
	MeasuredAt  time.Time         `json:"measured_at"`
	ConfirmedAt *time.Time        `json:"confirmed_at,omitempty"`
	Plan        string            `json:"plan,omitempty"`
	Buckets     []dashboardBucket `json:"buckets"`
}

// dashboardBucket adds the derived pace fields to a stored bucket. ElapsedPct
// is how much of the window had passed when the bucket was measured; Pace is
// used_pct divided by that share (1.0 = on track to reach 100% exactly at the
// reset). Both stay null when the window length or reset time is unknown, and
// ResetPassed marks a measurement whose window has since reset.
type dashboardBucket struct {
	store.QuotaBucket
	ElapsedPct  *float64 `json:"elapsed_pct"`
	Pace        *float64 `json:"pace"`
	ResetPassed bool     `json:"reset_passed,omitempty"`
}

type dashboardManual struct {
	Window     string     `json:"window"`
	MeasuredAt time.Time  `json:"measured_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Expired    bool       `json:"expired"`
	UsedPct    *float64   `json:"used_pct"`
	ResetsAt   *time.Time `json:"resets_at,omitempty"`
}

type dashboardTask struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Lane      string    `json:"lane"`
	State     string    `json:"state"`
	ClaimedBy string    `json:"claimed_by,omitempty"`
	JobID     string    `json:"job_id,omitempty"`
	Tier      string    `json:"tier,omitempty"`
	PR        string    `json:"pr,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// dashboardRepo groups refs.pr values by GitHub repository. Open holds
// unfinished tasks with a PR; Merged holds tasks merged in the last week.
// Repo is empty for refs.pr values that are not a GitHub pull URL.
type dashboardRepo struct {
	Repo   string        `json:"repo"`
	Open   []dashboardPR `json:"open"`
	Merged []dashboardPR `json:"merged"`
}

type dashboardPR struct {
	PR       string     `json:"pr"`
	Number   int        `json:"number,omitempty"`
	TaskID   int64      `json:"task_id"`
	Title    string     `json:"title"`
	State    string     `json:"state"`
	Lane     string     `json:"lane,omitempty"`
	HeadSHA  string     `json:"head_sha,omitempty"`
	At       time.Time  `json:"at"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
}

type dashboardMissing struct {
	Section string `json:"section"`
	Source  string `json:"source"`
	Note    string `json:"note"`
}

// dashboardMissingSources names what the page would show if hk recorded it.
var dashboardMissingSources = []dashboardMissing{
	{"prs", "PR review / CI / mergeability state", "hk stores only refs.pr, refs.head_sha and the task state; no GitHub review or check status is recorded"},
	{"quota_stops", "quota-stop decisions", "no hk table or document records pacelever gate verdicts"},
	{"hosts", "per-account host presence (PUT /v1/quota/presence)", "pacelever publishes it best-effort but hk has no endpoint or table; hosts come from the hub via /ui/api/live"},
	{"jobs", "per-job model", "hub jobs report a tier only; tasks show refs.linear.tier when set"},
}

func (h *Handler) dashboardAPI(w http.ResponseWriter, r *http.Request) {
	quota, quotaTruncated, err := h.store.ListLatestQuota(r.Context(), dashboardQuotaLimit)
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	tasks, tasksTruncated, err := h.store.ListDashboardTasks(r.Context(), dashboardTaskLimit)
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	merged, err := h.store.ListMergedTaskEvents(r.Context(), dashboardMergedScan)
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	response := buildDashboard(time.Now().UTC(), quota, tasks, merged)
	response.QuotaTruncated = quotaTruncated
	response.TasksTruncated = tasksTruncated
	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func (h *Handler) dashboardPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, "dashboard_page", nil)
}

func buildDashboard(now time.Time, quota []store.QuotaAccountLatest, tasks []store.Task, merged []store.MergedTaskEvent) dashboardResponse {
	out := dashboardResponse{
		GeneratedAt: now.Format(time.RFC3339),
		Quota:       make([]dashboardAccount, 0, len(quota)),
		Work:        []dashboardTask{},
		Missing:     dashboardMissingSources,
	}
	for _, account := range quota {
		out.Quota = append(out.Quota, dashboardQuotaAccount(now, account))
	}

	repos := map[string]*dashboardRepo{}
	repo := func(name string) *dashboardRepo {
		if repos[name] == nil {
			repos[name] = &dashboardRepo{Repo: name, Open: []dashboardPR{}, Merged: []dashboardPR{}}
		}
		return repos[name]
	}
	// The fleet's deployed repos always appear, so an empty pipeline reads as
	// zero open PRs rather than an absent repo.
	for _, svc := range deployServices {
		repo(svc.Repo)
	}
	for _, task := range tasks {
		if dashboardActiveStates[task.State] {
			item := dashboardTask{
				ID: task.ID, Title: task.Title, Lane: task.Lane, State: task.State,
				ClaimedBy: task.ClaimedBy, JobID: task.Refs.JobID, PR: task.Refs.PR, UpdatedAt: task.UpdatedAt,
			}
			if task.Refs.Linear != nil {
				item.Tier = task.Refs.Linear.Tier
			}
			out.Work = append(out.Work, item)
		}
		if task.Refs.PR == "" || task.State == "merged" || task.State == "dropped" {
			continue
		}
		name, number := dashboardPRRepo(task.Refs.PR)
		r := repo(name)
		r.Open = append(r.Open, dashboardPR{
			PR: task.Refs.PR, Number: number, TaskID: task.ID, Title: task.Title, State: task.State,
			Lane: task.Lane, HeadSHA: task.Refs.HeadSHA, At: task.UpdatedAt,
		})
	}
	mergedCount := 0
	for _, event := range merged {
		if mergedCount == dashboardMergedMax || now.Sub(event.At) > dashboardMergedSince {
			break
		}
		if event.Refs == nil || event.Refs.PR == "" {
			continue
		}
		mergedCount++
		at := event.At
		name, number := dashboardPRRepo(event.Refs.PR)
		r := repo(name)
		r.Merged = append(r.Merged, dashboardPR{
			PR: event.Refs.PR, Number: number, TaskID: event.TaskID, Title: event.Title, State: "merged",
			HeadSHA: event.Refs.HeadSHA, At: at, MergedAt: &at,
		})
	}
	out.PRs = make([]dashboardRepo, 0, len(repos))
	for _, r := range repos {
		out.PRs = append(out.PRs, *r)
	}
	// Repos with open PRs first, then by name; unrecognized refs last.
	sort.Slice(out.PRs, func(i, j int) bool {
		a, b := out.PRs[i], out.PRs[j]
		if (a.Repo == "") != (b.Repo == "") {
			return b.Repo == ""
		}
		if (len(a.Open) > 0) != (len(b.Open) > 0) {
			return len(a.Open) > 0
		}
		return a.Repo < b.Repo
	})
	return out
}

func dashboardQuotaAccount(now time.Time, account store.QuotaAccountLatest) dashboardAccount {
	pool, _, _ := strings.Cut(account.AccountKey, ":")
	out := dashboardAccount{AccountKey: account.AccountKey, Pool: pool, Manual: []dashboardManual{}}
	if obs := account.Observation; obs != nil {
		auto := &dashboardQuotaObs{MeasuredAt: obs.MeasuredAt, ConfirmedAt: obs.ConfirmedAt, Plan: obs.Plan, Buckets: []dashboardBucket{}}
		for _, bucket := range obs.Buckets {
			auto.Buckets = append(auto.Buckets, quotaPace(now, obs.MeasuredAt, bucket))
		}
		out.Auto = auto
	}
	for _, manual := range account.Manual {
		item := dashboardManual{Window: manual.Window, MeasuredAt: manual.MeasuredAt, ExpiresAt: manual.ExpiresAt}
		item.Expired = manual.ExpiresAt != nil && !manual.ExpiresAt.After(now)
		if len(manual.Buckets) == 1 {
			used := manual.Buckets[0].UsedPct
			item.UsedPct = &used
			item.ResetsAt = manual.Buckets[0].ResetsAt
		}
		out.Manual = append(out.Manual, item)
	}
	return out
}

// quotaPace derives the window share elapsed at measurement time and the
// resulting pace. The window must have a fixed length and a reset time, and
// the measurement must fall inside that window; otherwise both stay null.
func quotaPace(now, measuredAt time.Time, bucket store.QuotaBucket) dashboardBucket {
	out := dashboardBucket{QuotaBucket: bucket}
	if bucket.ResetsAt == nil {
		return out
	}
	out.ResetPassed = !bucket.ResetsAt.After(now)
	length, ok := quotaWindowLength(bucket.Window)
	if !ok {
		return out
	}
	start := bucket.ResetsAt.Add(-length)
	elapsed := measuredAt.Sub(start)
	if elapsed <= 0 || elapsed > length {
		return out
	}
	share := float64(elapsed) / float64(length)
	elapsedPct := share * 100
	pace := bucket.UsedPct / elapsedPct
	out.ElapsedPct = &elapsedPct
	out.Pace = &pace
	return out
}

// quotaWindowLength parses the stored window key ([1-9][0-9]{0,5}[smhdw]).
// "credit" and anything else has no fixed length.
func quotaWindowLength(window string) (time.Duration, bool) {
	if len(window) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(window[:len(window)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[window[len(window)-1]]
	if unit == 0 {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

// dashboardPRRepo returns owner/repo and the PR number for an exact GitHub
// pull URL, and "" for any other refs.pr value.
func dashboardPRRepo(pr string) (string, int) {
	match := deployPRRepoRE.FindStringSubmatch(pr)
	if match == nil {
		return "", 0
	}
	number, _ := strconv.Atoi(pr[strings.LastIndex(pr, "/")+1:])
	return match[1] + "/" + match[2], number
}

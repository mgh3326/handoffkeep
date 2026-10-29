package ui

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestQuotaWindowLength(t *testing.T) {
	for window, want := range map[string]time.Duration{
		"5h": 5 * time.Hour, "7d": 7 * 24 * time.Hour, "1w": 7 * 24 * time.Hour, "30m": 30 * time.Minute, "90s": 90 * time.Second,
	} {
		if got, ok := quotaWindowLength(window); !ok || got != want {
			t.Fatalf("%s = %v,%v want %v", window, got, ok, want)
		}
	}
	for _, window := range []string{"credit", "", "h", "0h", "5x", "-1h"} {
		if _, ok := quotaWindowLength(window); ok {
			t.Fatalf("%q parsed as a fixed window", window)
		}
	}
}

func TestQuotaPace(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	resets := now.Add(3 * time.Hour)
	// A 5h window resetting in 3h started 2h before now; measured 1h ago it
	// was 20% elapsed, so 30% used is 1.5x the linear pace.
	got := quotaPace(now, now.Add(-time.Hour), store.QuotaBucket{Window: "5h", UsedPct: 30, ResetsAt: &resets, Scope: "account"})
	if got.ElapsedPct == nil || math.Abs(*got.ElapsedPct-20) > 1e-9 || got.Pace == nil || math.Abs(*got.Pace-1.5) > 1e-9 || got.ResetPassed {
		t.Fatalf("pace=%+v", got)
	}
	if got.UsedPct != 30 || got.Window != "5h" {
		t.Fatalf("bucket fields not carried: %+v", got)
	}

	// No reset time, a credit window, or a measurement outside the window
	// leave pace unknown rather than zero.
	for name, bucket := range map[string]store.QuotaBucket{
		"no reset": {Window: "5h", UsedPct: 30},
		"credit":   {Window: "credit", UsedPct: 30, ResetsAt: &resets},
	} {
		if got := quotaPace(now, now, bucket); got.Pace != nil || got.ElapsedPct != nil {
			t.Fatalf("%s: pace=%+v", name, got)
		}
	}
	if got := quotaPace(now, now.Add(-3*time.Hour), store.QuotaBucket{Window: "5h", UsedPct: 30, ResetsAt: &resets}); got.Pace != nil {
		t.Fatalf("measurement before window start has pace %v", *got.Pace)
	}

	past := now.Add(-time.Minute)
	got = quotaPace(now, now.Add(-2*time.Hour), store.QuotaBucket{Window: "5h", UsedPct: 90, ResetsAt: &past})
	if !got.ResetPassed || got.Pace == nil {
		t.Fatalf("reset-passed measurement=%+v", got)
	}
}

func TestBuildDashboard(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	resets := now.Add(time.Hour)
	expired := now.Add(-time.Minute)
	quota := []store.QuotaAccountLatest{{
		AccountKey: "claude:0123456789abcdef",
		QuotaLatest: store.QuotaLatest{
			Observation: &store.QuotaObservation{Kind: "auto", MeasuredAt: now, Plan: "max", Buckets: []store.QuotaBucket{{Window: "5h", UsedPct: 40, ResetsAt: &resets, Scope: "account"}}},
			Manual:      []store.QuotaObservation{{Kind: "manual", Window: "7d", MeasuredAt: now.Add(-time.Hour), ExpiresAt: &expired, Buckets: []store.QuotaBucket{{Window: "7d", UsedPct: 55, Scope: "account"}}}},
		},
	}, {
		AccountKey:  "codex:fedcba9876543210",
		QuotaLatest: store.QuotaLatest{Manual: []store.QuotaObservation{}},
	}}
	tasks := []store.Task{
		{ID: 1, Title: "running", Lane: "lane-a", State: "in_progress", Refs: store.TaskRefs{JobID: "job-1", Linear: &store.TaskLinear{Tier: "opus"}}},
		{ID: 2, Title: "review", Lane: "lane-a", State: "verifying", Refs: store.TaskRefs{PR: "https://github.com/mgh3326/handoffkeep/pull/94", HeadSHA: "abc"}},
		{ID: 3, Title: "parked", Lane: "lane-b", State: "hold", Refs: store.TaskRefs{PR: "https://github.com/acme/widgets/pull/7"}},
		{ID: 4, Title: "odd ref", Lane: "lane-b", State: "backlog", Refs: store.TaskRefs{PR: "#12"}},
	}
	merged := []store.MergedTaskEvent{
		{TaskID: 9, Title: "landed", At: now.Add(-time.Hour), Refs: &store.TaskRefs{PR: "https://github.com/mgh3326/panewire/pull/5"}},
		{TaskID: 8, Title: "no pr", At: now.Add(-2 * time.Hour), Refs: &store.TaskRefs{}},
		{TaskID: 7, Title: "old", At: now.Add(-8 * 24 * time.Hour), Refs: &store.TaskRefs{PR: "https://github.com/mgh3326/panewire/pull/1"}},
	}
	got := buildDashboard(now, quota, tasks, merged)

	if len(got.Quota) != 2 || got.Quota[0].Pool != "claude" || got.Quota[1].Pool != "codex" || got.Quota[1].Auto != nil {
		t.Fatalf("quota=%+v", got.Quota)
	}
	if auto := got.Quota[0].Auto; auto == nil || auto.Plan != "max" || len(auto.Buckets) != 1 || auto.Buckets[0].Pace == nil {
		t.Fatalf("auto=%+v", got.Quota[0].Auto)
	}
	if manual := got.Quota[0].Manual; len(manual) != 1 || !manual[0].Expired || manual[0].UsedPct == nil || *manual[0].UsedPct != 55 {
		t.Fatalf("manual=%+v", manual)
	}

	if len(got.Work) != 2 || got.Work[0].ID != 1 || got.Work[0].JobID != "job-1" || got.Work[0].Tier != "opus" || got.Work[1].ID != 2 {
		t.Fatalf("work=%+v", got.Work)
	}

	repos := map[string]dashboardRepo{}
	for _, r := range got.PRs {
		repos[r.Repo] = r
	}
	hk := repos["mgh3326/handoffkeep"]
	if len(hk.Open) != 1 || hk.Open[0].Number != 94 || hk.Open[0].State != "verifying" || hk.Open[0].HeadSHA != "abc" {
		t.Fatalf("handoffkeep=%+v", hk)
	}
	if pw := repos["mgh3326/panewire"]; len(pw.Merged) != 1 || pw.Merged[0].TaskID != 9 || pw.Merged[0].MergedAt == nil {
		t.Fatalf("panewire=%+v", pw)
	}
	if at, ok := repos["mgh3326/auto_trader"]; !ok || len(at.Open) != 0 || at.Merged == nil {
		t.Fatalf("fleet repo without PRs is absent: %+v", got.PRs)
	}
	if w := repos["acme/widgets"]; len(w.Open) != 1 || w.Open[0].State != "hold" {
		t.Fatalf("widgets=%+v", w)
	}
	if odd := repos[""]; len(odd.Open) != 1 || odd.Open[0].PR != "#12" || odd.Open[0].Number != 0 {
		t.Fatalf("unrecognized ref=%+v", odd)
	}
	if last := got.PRs[len(got.PRs)-1]; last.Repo != "" {
		t.Fatalf("unrecognized refs must sort last: %+v", got.PRs)
	}
	if got.PRs[0].Repo != "acme/widgets" || got.PRs[1].Repo != "mgh3326/handoffkeep" {
		t.Fatalf("repos with open PRs must sort first by name: %+v", got.PRs)
	}
	if len(got.Missing) == 0 {
		t.Fatal("missing sources are not reported")
	}
}

func TestDashboardAPIRefusesServicePrincipals(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.serveAPI(rec, httptest.NewRequest(http.MethodGet, "/ui/api/dashboard", nil), cfaccess.Identity{ServiceName: "svc"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
}

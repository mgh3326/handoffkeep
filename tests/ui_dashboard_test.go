package tests

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The home dashboard aggregates the shared quota store, active tasks and the
// PR refs hk already records. It is read-only and operator-only.
type dashboardPayload struct {
	GeneratedAt string `json:"generated_at"`
	Quota       []struct {
		AccountKey string `json:"account_key"`
		Pool       string `json:"pool"`
		Auto       *struct {
			Plan    string `json:"plan"`
			Buckets []struct {
				Window     string   `json:"window"`
				UsedPct    float64  `json:"used_pct"`
				ElapsedPct *float64 `json:"elapsed_pct"`
				Pace       *float64 `json:"pace"`
			} `json:"buckets"`
		} `json:"auto"`
		Manual []struct {
			Window  string   `json:"window"`
			UsedPct *float64 `json:"used_pct"`
		} `json:"manual"`
	} `json:"quota"`
	Work []struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
		JobID string `json:"job_id"`
	} `json:"work"`
	PRs []struct {
		Repo string `json:"repo"`
		Open []struct {
			TaskID int64  `json:"task_id"`
			Number int    `json:"number"`
			State  string `json:"state"`
		} `json:"open"`
		Merged []struct {
			TaskID int64 `json:"task_id"`
		} `json:"merged"`
	} `json:"prs"`
	Missing []struct {
		Section string `json:"section"`
	} `json:"missing"`
}

func dashboardHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func TestUIDashboard(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	ctx := t.Context()

	key := "dash:" + dashboardHex(t, 8)
	holder := dashboardHex(t, 16)
	if _, err := s.LeaseQuota(ctx, key, "dash-client", holder, 60); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	resets := now.Add(3 * time.Hour)
	if _, _, err := s.PutQuota(ctx, key, "dash-client", holder, store.QuotaObservation{
		Kind: "auto", MeasuredAt: now, Plan: "max",
		Buckets: []store.QuotaBucket{{Window: "5h", UsedPct: 20, ResetsAt: &resets, Scope: "account"}},
	}); err != nil {
		t.Fatal(err)
	}
	manualExpires := now.Add(time.Hour)
	if _, _, err := s.PutQuota(ctx, key, "dash-client", holder, store.QuotaObservation{
		Kind: "manual", Window: "7d", MeasuredAt: now, ExpiresAt: &manualExpires,
		Buckets: []store.QuotaBucket{{Window: "7d", UsedPct: 61, Scope: "account"}},
	}); err != nil {
		t.Fatal(err)
	}

	lane := uiLane(t, "dashboard")
	repo := fmt.Sprintf("dash-%d/repo", time.Now().UnixNano())
	running, err := s.CreateTask(ctx, store.Task{Lane: lane, Title: "running task", Kind: "implement", CreatedBy: "test-node", Project: projectRef("experiment"),
		Refs: store.TaskRefs{PR: "https://github.com/" + repo + "/pull/3"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, running.ID, "worker-a", "job-dash-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, running.ID, "in_progress", "worker-a", "started", nil, ""); err != nil {
		t.Fatal(err)
	}
	landed, err := s.CreateTask(ctx, store.Task{Lane: lane, Title: "landed task", Kind: "implement", CreatedBy: "test-node", Project: projectRef("experiment"),
		Refs: store.TaskRefs{PR: "https://github.com/" + repo + "/pull/2"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, landed.ID, "worker-b", "job-dash-2", ""); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"in_progress", "verifying", "merged"} {
		if _, err := s.TransitionTask(ctx, landed.ID, to, "worker-b", to, nil, ""); err != nil {
			t.Fatal(err)
		}
	}

	var got dashboardPayload
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/dashboard", assertion, &got); status != http.StatusOK {
		t.Fatalf("dashboard status=%d", status)
	}

	foundQuota := false
	for _, account := range got.Quota {
		if account.AccountKey != key {
			continue
		}
		foundQuota = true
		if account.Pool != "dash" || account.Auto == nil || account.Auto.Plan != "max" || len(account.Auto.Buckets) != 1 {
			t.Fatalf("account=%+v", account)
		}
		bucket := account.Auto.Buckets[0]
		// 5h window resetting in 3h: 40% elapsed at measurement, 20% used.
		if bucket.ElapsedPct == nil || bucket.Pace == nil || *bucket.Pace < 0.49 || *bucket.Pace > 0.51 {
			t.Fatalf("bucket=%+v", bucket)
		}
		if len(account.Manual) != 1 || account.Manual[0].Window != "7d" || account.Manual[0].UsedPct == nil || *account.Manual[0].UsedPct != 61 {
			t.Fatalf("manual=%+v", account.Manual)
		}
	}
	if !foundQuota {
		t.Fatalf("quota account %s missing from %d accounts", key, len(got.Quota))
	}

	foundWork := false
	for _, task := range got.Work {
		if task.ID == landed.ID {
			t.Fatal("merged task listed as running work")
		}
		if task.ID == running.ID {
			foundWork = task.State == "in_progress" && task.JobID == "job-dash-1"
		}
	}
	if !foundWork {
		t.Fatalf("running task %d missing from work", running.ID)
	}

	foundRepo := false
	for _, r := range got.PRs {
		if r.Repo != repo {
			continue
		}
		foundRepo = true
		if len(r.Open) != 1 || r.Open[0].TaskID != running.ID || r.Open[0].Number != 3 || r.Open[0].State != "in_progress" {
			t.Fatalf("open=%+v", r.Open)
		}
		if len(r.Merged) != 1 || r.Merged[0].TaskID != landed.ID {
			t.Fatalf("merged=%+v", r.Merged)
		}
	}
	if !foundRepo {
		t.Fatalf("repo %s missing from prs", repo)
	}
	if len(got.Missing) == 0 {
		t.Fatal("missing sources are not reported")
	}

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/home", assertion, "")
	if body := responseText(t, response); response.StatusCode != http.StatusOK || !strings.Contains(body, "/ui/static/console/dashboard.js") || !strings.Contains(body, `id="dashboard-root"`) {
		t.Fatalf("home page status=%d", response.StatusCode)
	}
	if csp := response.Header.Get("Content-Security-Policy"); csp == "" {
		t.Fatal("home page has no console CSP")
	}
}

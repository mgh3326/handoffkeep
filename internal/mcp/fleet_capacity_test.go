package mcp

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

type fleetCapacityWindowOut struct {
	Kind      string   `json:"kind"`
	Window    string   `json:"window"`
	State     string   `json:"state"`
	UsedPct   *float64 `json:"used_pct"`
	AgeSecond int64    `json:"age_seconds"`
	ExpiresAt string   `json:"expires_at"`
	Expired   *bool    `json:"expired"`
}

type fleetCapacityAccountOut struct {
	AccountKey string                   `json:"account_key"`
	Pool       string                   `json:"pool"`
	Windows    []fleetCapacityWindowOut `json:"windows"`
}

type fleetCapacityOut struct {
	Accounts []fleetCapacityAccountOut `json:"accounts"`
	Presence []struct {
		HostID   string `json:"host_id"`
		Accounts []struct {
			AccountKey string `json:"account_key"`
			AgeSeconds int64  `json:"age_seconds"`
		} `json:"accounts"`
	} `json:"presence"`
	Placements []struct {
		JobID      string `json:"job_id"`
		Outcome    string `json:"outcome"`
		Reason     string `json:"reason"`
		ChosenHost string `json:"chosen_host"`
		Profile    string `json:"profile"`
		Model      string `json:"model"`
		Candidates []struct {
			Host    string `json:"host"`
			Outcome string `json:"outcome"`
			Reason  string `json:"reason"`
		} `json:"candidates"`
	} `json:"placements"`
	PlacementNote string `json:"placement_note"`
}

func TestFleetCapacityAccessShapeFiltersAndLimit(t *testing.T) {
	st, _ := newMetaStore(t)
	ctx := context.Background()
	key := "claude:0123456789abcdef"
	holder := "0123456789abcdef0123456789abcdef"
	if _, err := st.LeaseQuota(ctx, key, "test", holder, 600); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().UTC().Add(time.Hour)
	if _, _, err := st.PutQuota(ctx, key, "test", holder, store.QuotaObservation{
		Kind: "auto", MeasuredAt: time.Now().UTC(), Buckets: []store.QuotaBucket{{Window: "7d", UsedPct: 66, ResetsAt: &reset, Scope: "account"}},
	}); err != nil {
		t.Fatal(err)
	}
	manualExpiry := time.Now().UTC().Add(-time.Minute)
	manualMeasured := manualExpiry.Add(-time.Hour)
	if _, _, err := st.PutQuota(ctx, key, "test", holder, store.QuotaObservation{
		Kind: "manual", Window: "7d", MeasuredAt: manualMeasured, ExpiresAt: &manualExpiry,
		Buckets: []store.QuotaBucket{{Window: "7d", UsedPct: 95, Scope: "account"}},
	}); err != nil {
		t.Fatal(err)
	}
	manualEmptyKey := "claude:1123456789abcdef"
	if _, err := st.LeaseQuota(ctx, manualEmptyKey, "test", holder, 600); err != nil {
		t.Fatal(err)
	}
	manualEmptyExpiry := time.Now().UTC().Add(-time.Minute)
	if _, _, err := st.PutQuota(ctx, manualEmptyKey, "test", holder, store.QuotaObservation{
		Kind: "manual", Window: "1h", MeasuredAt: manualEmptyExpiry.Add(-time.Hour), ExpiresAt: &manualEmptyExpiry,
	}); err != nil {
		t.Fatal(err)
	}
	devinKey := "devin:abcdef0123456789"
	if _, err := st.LeaseQuota(ctx, devinKey, "test", holder, 600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.PutQuota(ctx, devinKey, "test", holder, store.QuotaObservation{
		Kind: "auto", MeasuredAt: time.Now().UTC(), FreeBucket: &store.QuotaFreeBucket{State: "unknown"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutQuotaPresence(ctx, store.QuotaPresence{HostID: "host-a", AccountKey: key, LastSeen: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 101; i++ {
		jobID := fmt.Sprintf("fleet-%03d", i)
		host := "host-a"
		if i == 100 {
			host = "host-b"
		}
		_, _, err := st.RecordPlacement(ctx, store.PlacementDecision{
			JobID: jobID, Attempt: 1, DecidedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
			DecidedOn: "node-a", Source: "auto", Ranking: "score", StateSource: "hub", Outcome: "refused",
			Reason: "quota_refused", Profile: "claude-opus", Model: "opus", Candidates: []store.PlacementCandidate{{Host: host, Outcome: "refused", Reason: "quota_refused"}},
		})
		if err != nil {
			t.Fatalf("record placement %s: %v", jobID, err)
		}
	}

	fixture := newAccessFixture(t)
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{}, fixture.verifier))
	t.Cleanup(srv.Close)
	session := connectMCP(t, srv.URL, map[string]string{"Cf-Access-Jwt-Assertion": fixture.token(t, jwt.MapClaims{"email": "operator@example.com"})})
	out := callTool[fleetCapacityOut](t, session, "fleet_capacity", map[string]any{"limit": 101})
	if len(out.Accounts) != 3 {
		t.Fatalf("account section=%+v", out.Accounts)
	}
	accountsByKey := make(map[string]fleetCapacityAccountOut)
	for _, account := range out.Accounts {
		accountsByKey[account.AccountKey] = account
	}
	claude := accountsByKey[key]
	if claude.Pool != "claude" || len(claude.Windows) != 2 {
		t.Fatalf("account=%+v", claude)
	}
	var auto, manual bool
	for _, window := range claude.Windows {
		switch window.Kind {
		case "manual":
			manual = window.UsedPct != nil && *window.UsedPct == 95 && window.Expired != nil && *window.Expired && window.ExpiresAt != ""
		case "auto":
			auto = window.UsedPct != nil && *window.UsedPct == 66 && window.Window == "7d"
		}
	}
	if !manual || !auto {
		t.Fatalf("auto=%v manual=%v windows=%+v", auto, manual, claude.Windows)
	}
	devin := accountsByKey[devinKey]
	if len(devin.Windows) != 1 || devin.Windows[0].State != "unknown" {
		t.Fatalf("free bucket state=%+v", devin)
	}
	manualEmpty := accountsByKey[manualEmptyKey]
	if len(manualEmpty.Windows) != 1 || manualEmpty.Windows[0].Kind != "manual" || manualEmpty.Windows[0].Window != "1h" || manualEmpty.Windows[0].UsedPct != nil || manualEmpty.Windows[0].Expired == nil || !*manualEmpty.Windows[0].Expired || manualEmpty.Windows[0].ExpiresAt == "" {
		t.Fatalf("empty manual observation=%+v", manualEmpty)
	}
	if len(out.Presence) != 1 || out.Presence[0].HostID != "host-a" || len(out.Presence[0].Accounts) != 1 || out.Presence[0].Accounts[0].AccountKey != key {
		t.Fatalf("presence section=%+v", out.Presence)
	}
	if len(out.Placements) != 100 || out.Placements[0].JobID != "fleet-100" || out.Placements[0].Candidates[0].Reason != "quota_refused" || out.Placements[0].Profile != "claude-opus" || out.Placements[0].Model != "opus" {
		t.Fatalf("placement section len=%d first=%+v", len(out.Placements), out.Placements[:min(len(out.Placements), 1)])
	}
	defaultLimit := callTool[fleetCapacityOut](t, session, "fleet_capacity", map[string]any{})
	if len(defaultLimit.Placements) != fleetCapacityDefaultLimit {
		t.Fatalf("default placement count=%d want %d", len(defaultLimit.Placements), fleetCapacityDefaultLimit)
	}
	filtered := callTool[fleetCapacityOut](t, session, "fleet_capacity", map[string]any{"job": "fleet-100", "host": "host-b", "since": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "limit": 20})
	// Job/host/since filter placement attempts only; quota and presence remain account-wide.
	if len(filtered.Accounts) != 3 || len(filtered.Placements) != 1 || filtered.Placements[0].JobID != "fleet-100" {
		t.Fatalf("filtered placements=%+v", filtered.Placements)
	}
	missing := callTool[fleetCapacityOut](t, session, "fleet_capacity", map[string]any{"job": "fleet-000", "host": "host-b"})
	if len(missing.Accounts) != 3 || len(missing.Presence) != 1 || len(missing.Placements) != 0 || missing.PlacementNote != "no placement records" {
		t.Fatalf("empty placement result lost other sections or note: %+v", missing)
	}
}

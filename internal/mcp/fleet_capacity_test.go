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

type fleetCapacityOut struct {
	Accounts []struct {
		AccountKey string `json:"account_key"`
		Pool       string `json:"pool"`
		Windows    []struct {
			Kind      string  `json:"kind"`
			Window    string  `json:"window"`
			UsedPct   float64 `json:"used_pct"`
			AgeSecond int64   `json:"age_seconds"`
		} `json:"windows"`
	} `json:"accounts"`
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
	if len(out.Accounts) != 1 || out.Accounts[0].AccountKey != key || out.Accounts[0].Pool != "claude" || len(out.Accounts[0].Windows) != 1 || out.Accounts[0].Windows[0].UsedPct != 66 || out.Accounts[0].Windows[0].Window != "7d" {
		t.Fatalf("account section=%+v", out.Accounts)
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
	if len(filtered.Placements) != 1 || filtered.Placements[0].JobID != "fleet-100" {
		t.Fatalf("filtered placements=%+v", filtered.Placements)
	}
	missing := callTool[fleetCapacityOut](t, session, "fleet_capacity", map[string]any{"job": "fleet-000", "host": "host-b"})
	if len(missing.Accounts) != 1 || len(missing.Presence) != 1 || len(missing.Placements) != 0 || missing.PlacementNote != "no placement records" {
		t.Fatalf("empty placement result lost other sections or note: %+v", missing)
	}
}

package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func fleetStatusGolden(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("testdata/fleet_status_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestFleetStatusSnapshotGoldenParses(t *testing.T) {
	snap, err := parseFleetStatusSnapshot(fleetStatusGolden(t))
	if err != nil {
		t.Fatalf("golden snapshot: %v", err)
	}
	if snap.V != 1 {
		t.Fatalf("v=%d, want 1", snap.V)
	}
	wantAt := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	if !snap.At.Equal(wantAt) {
		t.Fatalf("at=%s, want %s", snap.At, wantAt)
	}
	if len(snap.Hosts) != 2 || snap.Hosts[0].Host != "node-a" || !snap.Hosts[0].Accepting || snap.Hosts[1].Connected {
		t.Fatalf("hosts=%+v", snap.Hosts)
	}
	if len(snap.Quota) != 1 {
		t.Fatalf("quota=%+v", snap.Quota)
	}
	row := snap.Quota[0]
	if row.Host != "node-a" || row.Pool != "claude" || row.Account != "claude:0123456789abcdef" || !row.Fresh {
		t.Fatalf("quota row=%+v", row)
	}
	if len(row.Windows) != 2 || row.Windows[0].Window != "5h" || row.Windows[0].UsedPct == nil || *row.Windows[0].UsedPct != 41 {
		t.Fatalf("windows=%+v", row.Windows)
	}
	if len(snap.Plan) != 2 || snap.Plan[0].Outcome != "ADMITTED" || snap.Plan[1].Outcome != "NO_ELIGIBLE_HOST" {
		t.Fatalf("plan=%+v", snap.Plan)
	}
}

func TestFleetStatusSnapshotToleratesUnknownFields(t *testing.T) {
	body := `{"v":1,"at":"2026-11-15T12:00:00Z","future":{"nested":[1,2]},"hosts":[],"quota":[],"plan":[],"extra":"x"}`
	snap, err := parseFleetStatusSnapshot(body)
	if err != nil {
		t.Fatalf("unknown fields: %v", err)
	}
	if snap.Hosts == nil || snap.Quota == nil || snap.Plan == nil {
		t.Fatalf("empty lists must normalize to arrays: %+v", snap)
	}
}

func TestFleetStatusSnapshotRejectsMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"not-json":          `not json`,
		"json-array":        `[]`,
		"trailing-data":     `{"v":1,"at":"2026-11-15T12:00:00Z","hosts":[],"quota":[],"plan":[]} {}`,
		"bad-version":       `{"v":2,"at":"2026-11-15T12:00:00Z","hosts":[],"quota":[],"plan":[]}`,
		"missing-at":        `{"v":1,"hosts":[],"quota":[],"plan":[]}`,
		"bad-at":            `{"v":1,"at":"soon","hosts":[],"quota":[],"plan":[]}`,
		"host-no-name":      `{"v":1,"at":"2026-11-15T12:00:00Z","hosts":[{"host":""}],"quota":[],"plan":[]}`,
		"quota-no-account":  `{"v":1,"at":"2026-11-15T12:00:00Z","hosts":[],"quota":[{"host":"h","pool":"p","windows":[],"measured_at":"2026-11-15T11:00:00Z"}],"plan":[]}`,
		"quota-no-measured": `{"v":1,"at":"2026-11-15T12:00:00Z","hosts":[],"quota":[{"host":"h","pool":"p","account":"p:0123456789abcdef","windows":[]}],"plan":[]}`,
		"window-no-name":    `{"v":1,"at":"2026-11-15T12:00:00Z","hosts":[],"quota":[{"host":"h","pool":"p","account":"p:0123456789abcdef","windows":[{"used_pct":1}],"measured_at":"2026-11-15T11:00:00Z"}],"plan":[]}`,
		"plan-no-outcome":   `{"v":1,"at":"2026-11-15T12:00:00Z","hosts":[],"quota":[],"plan":[{"grade":"A","t":"T1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFleetStatusSnapshot(body); err == nil {
				t.Fatalf("body accepted: %s", body)
			}
		})
	}
}

func fleetStatusHandler(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	_, url := testdb.Schema(t, "ui_fleet_status")
	st, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return &Handler{store: st, csrfKey: []byte("fleet-status-test")}, st
}

func fleetStatusGet(h *Handler, identity cfaccess.Identity) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/ui/api/fleet-status", nil)
	w := httptest.NewRecorder()
	h.serveAPI(w, r, identity)
	return w
}

func TestFleetStatusAPINoSnapshotYet(t *testing.T) {
	h, _ := fleetStatusHandler(t)
	w := fleetStatusGet(h, cfaccess.Identity{Email: "operator@example.com"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var response fleetStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Snapshot != nil {
		t.Fatalf("snapshot=%+v, want null before the first write", response.Snapshot)
	}
}

func TestFleetStatusAPIServesGoldenSnapshot(t *testing.T) {
	h, st := fleetStatusHandler(t)
	if _, _, err := st.PutDocument(context.Background(), store.Document{
		Key: fleetStatusDocKey, Kind: "note", Body: fleetStatusGolden(t), CreatedBy: "tester",
	}); err != nil {
		t.Fatal(err)
	}
	w := fleetStatusGet(h, cfaccess.Identity{Email: "operator@example.com"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var response fleetStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Snapshot == nil || len(response.Snapshot.Hosts) != 2 || len(response.Snapshot.Quota) != 1 || len(response.Snapshot.Plan) != 2 {
		t.Fatalf("snapshot=%+v", response.Snapshot)
	}
	if response.UpdatedAt == nil || response.UpdatedAt.IsZero() {
		t.Fatalf("updated_at=%v", response.UpdatedAt)
	}
}

func TestFleetStatusAPIMalformedBodyIsNamedError(t *testing.T) {
	h, st := fleetStatusHandler(t)
	if _, _, err := st.PutDocument(context.Background(), store.Document{
		Key: fleetStatusDocKey, Kind: "note", Body: `{"v":1,"at":"not-a-time"}`, CreatedBy: "tester",
	}); err != nil {
		t.Fatal(err)
	}
	w := fleetStatusGet(h, cfaccess.Identity{Email: "operator@example.com"})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != "fleet_status_invalid" {
		t.Fatalf("body=%s err=%v", w.Body.String(), err)
	}
}

func TestFleetStatusAPIRefusesServicePrincipals(t *testing.T) {
	h := &Handler{}
	w := fleetStatusGet(h, cfaccess.Identity{ServiceName: "svc"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", w.Code)
	}
}

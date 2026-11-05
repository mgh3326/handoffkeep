package tests

// GET /ui/api/deploy-pending, byte for byte (hk 3141). The body was recorded
// from the handler before pendingDeploys was split out of it, and must not
// change: /ui/api/attention reads the same document. Times that follow the
// clock (generated_at, recorded_at) are masked; everything else is compared
// as written.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// attentionSchemaStore is a store on a throwaway schema, with a connection to
// the same schema for rows the store API does not write.
func attentionSchemaStore(t *testing.T, prefix string) (*store.Store, *pgx.Conn) {
	t.Helper()
	_, url := testdb.Schema(t, prefix)
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	db, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	return s, db
}

// seedDeployGolden is the fixed deploy state both the golden and the
// attention tests read: handoffkeep with a success, a later failure and three
// merges, auto_trader with an unrecorded deploy time, panewire-hub with only
// a failure, and one invalid document.
func seedDeployGolden(t *testing.T, s *store.Store, db *pgx.Conn) {
	t.Helper()
	step := 7
	seedDeployDoc(t, s, "deploy/handoffkeep/20260920T010000Z", deployFixtureBody(t, "handoffkeep", "success", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2026-09-20T01:00:00Z", nil))
	seedDeployDoc(t, s, "deploy/handoffkeep/20260923T020000Z", deployFixtureBody(t, "handoffkeep", "failed", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "2026-09-23T02:00:00Z", &step))
	seedDeployDoc(t, s, "deploy/handoffkeep/20260924T000000Z", "not a deploy record")
	seedDeployDoc(t, s, "deploy/auto_trader/20260920T010000Z", deployFixtureBody(t, "auto_trader", "success", "sha256:0000000000000000000000000000000000000000000000000000000000000000", "", nil))
	seedDeployDoc(t, s, "deploy/panewire-hub/20260923T040000Z", deployFixtureBody(t, "panewire-hub", "failed", "", "2026-09-23T04:00:00Z", nil))

	base := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)
	seedMergedEvent(t, db, s, "before the deploy", "https://github.com/mgh3326/handoffkeep/pull/10", base.Add(-time.Hour))
	seedMergedEvent(t, db, s, "after the deploy", "https://github.com/mgh3326/handoffkeep/pull/11", base.Add(time.Hour))
	seedMergedEvent(t, db, s, "newer after the deploy", "https://github.com/mgh3326/handoffkeep/pull/12", base.Add(48*time.Hour))
	seedMergedEvent(t, db, s, "other service", "https://github.com/mgh3326/auto_trader/pull/3", base.Add(2*time.Hour))
	seedMergedEvent(t, db, s, "unattributed", "handoffkeep#13", base.Add(3*time.Hour))
}

// maskClock replaces the two fields that follow the clock.
func maskClock(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "generated_at" || key == "recorded_at" {
				v[key] = "<clock>"
				continue
			}
			maskClock(item)
		}
	case []any:
		for _, item := range v {
			maskClock(item)
		}
	}
}

func TestUIDeployPendingGolden(t *testing.T) {
	s, db := attentionSchemaStore(t, "ui_deploy_golden")
	seedDeployGolden(t, s, db)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/deploy-pending", assertion, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, responseText(t, response))
	}
	var body any
	if err := json.Unmarshal([]byte(responseText(t, response)), &body); err != nil {
		t.Fatal(err)
	}
	maskClock(body)
	got, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/deploy_pending.golden.json")
	if err != nil {
		t.Fatalf("golden missing: %v\nbody:\n%s", err, got)
	}
	if !bytes.Equal(bytes.TrimSpace(want), got) {
		t.Fatalf("deploy-pending body changed.\nwant:\n%s\ngot:\n%s", want, got)
	}
}

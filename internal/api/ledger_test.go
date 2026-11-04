package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// ledgerAPIServer is a server on a throwaway schema, with a pool on the same
// schema for moving the feed's reset boundary.
func ledgerAPIServer(t *testing.T) (*httptest.Server, *store.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "ledger_api")
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"tester": "tok"}}.Handler())
	t.Cleanup(h.Close)
	return h, st, pool
}

func TestLedgerAPI(t *testing.T) {
	h, st, pool := ledgerAPIServer(t)
	ctx := context.Background()
	project := "experiment"
	newTask := func(title string) store.Task {
		t.Helper()
		x, err := st.CreateTask(ctx, store.Task{Lane: "ledger-api-lane", Title: title, Kind: "implement", CreatedBy: "ledger-test", Project: &project})
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	newTask("first")

	var bad map[string]any
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/ledger", nil, &bad); status != http.StatusBadRequest || bad["error"] != "invalid_context" {
		t.Fatalf("missing consumer: %d %v", status, bad)
	}
	var l store.Ledger
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/ledger?consumer=desk-api", nil, &l); status != http.StatusOK || !l.ChangedSinceAck.NoCursor || l.Consumer != "desk-api" || l.ChangedSinceAck.MaxSyncID < 1 {
		t.Fatalf("first read: %d %+v", status, l.ChangedSinceAck)
	}

	for _, body := range []any{
		map[string]any{"consumer": "desk-api"},
		map[string]any{"sync_id": 1},
		map[string]any{"consumer": "desk-api", "sync_id": 1, "extra": true},
		map[string]any{"consumer": "bad consumer", "sync_id": 1},
	} {
		var e map[string]any
		if status := decisionAPICall(t, h, http.MethodPost, "/v1/ledger/ack", body, &e); status != http.StatusBadRequest {
			t.Fatalf("ack %v: %d %v", body, status, e)
		}
	}
	var c store.LedgerCursor
	if status := decisionAPICall(t, h, http.MethodPost, "/v1/ledger/ack", map[string]any{"consumer": "desk-api", "sync_id": 0}, &c); status != http.StatusOK || c.AckedSyncID != l.ChangedSinceAck.MaxSyncID || c.UpdatedBy != "tester" {
		t.Fatalf("first ack: %d %+v, want cursor %d", status, c, l.ChangedSinceAck.MaxSyncID)
	}
	start := c.AckedSyncID

	x := newTask("second")
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/ledger?consumer=desk-api", nil, &l); status != http.StatusOK {
		t.Fatalf("read: %d", status)
	}
	ch := l.ChangedSinceAck
	if ch.NoCursor || ch.Cursor == nil || ch.Cursor.AckedSyncID != start || len(ch.Rows) == 0 || ch.Rows[0].SyncID <= start {
		t.Fatalf("delta %+v", ch)
	}
	found := false
	for _, r := range ch.Rows {
		found = found || (r.Model == "task" && r.ModelID == itoa64(x.ID))
	}
	if !found {
		t.Fatalf("delta lacks task %d: %+v", x.ID, ch.Rows)
	}
	if status := decisionAPICall(t, h, http.MethodPost, "/v1/ledger/ack", map[string]any{"consumer": "desk-api", "sync_id": ch.LastSyncID}, &c); status != http.StatusOK || c.AckedSyncID != ch.LastSyncID {
		t.Fatalf("forward ack: %d %+v", status, c)
	}
	var e map[string]any
	if status := decisionAPICall(t, h, http.MethodPost, "/v1/ledger/ack", map[string]any{"consumer": "desk-api", "sync_id": start}, &e); status != http.StatusConflict || e["error"] != "ledger_ack_backward" || e["reason"] == nil {
		t.Fatalf("backward ack: %d %v", status, e)
	}
	if status := decisionAPICall(t, h, http.MethodPost, "/v1/ledger/ack", map[string]any{"consumer": "desk-api", "sync_id": ch.MaxSyncID + 1000}, &e); status != http.StatusBadRequest || e["error"] != "invalid_context" {
		t.Fatalf("ack past the watermark: %d %v", status, e)
	}

	// A cursor past the reset boundary answers 410 as the feed does.
	newTask("third")
	if _, err := pool.Exec(ctx, `UPDATE sync_state SET pruned_through=max_sync_id WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	var reset map[string]any
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/ledger?consumer=desk-api", nil, &reset); status != http.StatusGone || reset["reset"] != true || reset["min_sync_id"] == nil {
		t.Fatalf("stale cursor: %d %v", status, reset)
	}
	var feedReset map[string]any
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/sync/delta?after="+itoa64(ch.LastSyncID), nil, &feedReset); status != http.StatusGone || feedReset["min_sync_id"] != reset["min_sync_id"] {
		t.Fatalf("feed reset %d %v, ledger %v", status, feedReset, reset)
	}
}

func TestStatusUpdatesAPI(t *testing.T) {
	h, _, _ := ledgerAPIServer(t)
	var e map[string]any
	if status := decisionAPICall(t, h, http.MethodPost, "/v1/projects/experiment/status-updates", map[string]string{"health": "green", "body": "x"}, &e); status != http.StatusBadRequest || e["error"] != "invalid_status_update" || e["reason"] == nil {
		t.Fatalf("invalid health: %d %v", status, e)
	}
	if status := decisionAPICall(t, h, http.MethodPost, "/v1/projects/no-such-project/status-updates", map[string]string{"health": "on_track", "body": "x"}, &e); status != http.StatusNotFound || e["error"] != "project_not_found" {
		t.Fatalf("unknown project: %d %v", status, e)
	}
	if status := decisionAPICall(t, h, http.MethodPost, "/v1/projects/experiment/status-updates", map[string]any{"health": "on_track", "body": "x", "extra": 1}, &e); status != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %v", status, e)
	}
	var x store.StatusUpdate
	if status := decisionAPICall(t, h, http.MethodPost, "/v1/projects/experiment/status-updates", map[string]string{"health": "at_risk", "body": "slipping"}, &x); status != http.StatusCreated || x.ID < 1 || x.Health != "at_risk" || x.CreatedBy != "tester" || x.Diff.PreviousID != nil {
		t.Fatalf("create: %d %+v", status, x)
	}
	var list store.StatusUpdateList
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/projects/experiment/status-updates?limit=10", nil, &list); status != http.StatusOK || len(list.Updates) != 1 || list.Updates[0].ID != x.ID || list.Stale {
		t.Fatalf("list: %d %+v", status, list)
	}
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/projects/experiment/status-updates?limit=0", nil, &e); status != http.StatusBadRequest {
		t.Fatalf("bad limit: %d %v", status, e)
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

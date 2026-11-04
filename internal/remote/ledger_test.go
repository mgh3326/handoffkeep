package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ledgerFake answers every request with status and response.
func ledgerFake(t *testing.T, status int, response string) (*httptest.Server, *[]decisionCall) {
	t.Helper()
	calls := []decisionCall{}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := decisionCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			c.body = map[string]any{"raw": string(raw)}
		}
		calls = append(calls, c)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(h.Close)
	return h, &calls
}

func TestLedgerRead(t *testing.T) {
	h, calls := ledgerFake(t, http.StatusOK, `{"consumer":"desk","in_flight":{"projects":[],"tasks":[{"id":4,"state":"developing","next_trigger":"review"}]},"changed_since_ack":{"no_cursor":true,"max_sync_id":9,"summary":[],"rows":[]}}`)
	c := Client{URL: h.URL, Token: "tok"}
	l, err := c.Ledger(context.Background(), "desk")
	if err != nil || l.Consumer != "desk" || len(l.InFlight.Tasks) != 1 || l.InFlight.Tasks[0].NextTrigger != "review" || !l.ChangedSinceAck.NoCursor || l.ChangedSinceAck.MaxSyncID != 9 {
		t.Fatalf("ledger=%+v err=%v", l, err)
	}
	if g := (*calls)[0]; g.method != "GET" || g.path != "/v1/ledger" || g.query != "consumer=desk" || g.auth != "Bearer tok" {
		t.Fatalf("call=%+v", g)
	}
}

func TestLedgerResetIsTyped(t *testing.T) {
	h, _ := ledgerFake(t, http.StatusGone, `{"reset":true,"min_sync_id":123}`)
	c := Client{URL: h.URL, Token: "tok"}
	_, err := c.Ledger(context.Background(), "desk")
	var reset *store.SyncReset
	if !errors.As(err, &reset) || reset.MinSyncID != 123 {
		t.Fatalf("err=%v, want *store.SyncReset", err)
	}
	if _, err = c.AckLedger(context.Background(), "desk", 5); !errors.As(err, &reset) {
		t.Fatalf("ack err=%v", err)
	}
	// A 410 without the reset body stays an HTTP error.
	h, _ = ledgerFake(t, http.StatusGone, `{"error":"gone"}`)
	c = Client{URL: h.URL, Token: "tok"}
	if _, err = c.Ledger(context.Background(), "desk"); errors.As(err, &reset) || ErrorCode(err) != "gone" {
		t.Fatalf("plain 410 err=%v", err)
	}
}

func TestAckLedgerBodyAndBackward(t *testing.T) {
	h, calls := ledgerFake(t, http.StatusOK, `{"consumer":"desk","acked_sync_id":42,"updated_by":"tester"}`)
	c := Client{URL: h.URL, Token: "tok"}
	cur, err := c.AckLedger(context.Background(), "desk", 42)
	if err != nil || cur.AckedSyncID != 42 {
		t.Fatalf("cursor=%+v err=%v", cur, err)
	}
	if g := (*calls)[0]; g.method != "POST" || g.path != "/v1/ledger/ack" || g.body["raw"] != `{"consumer":"desk","sync_id":42}` {
		t.Fatalf("call=%+v", g)
	}
	h, _ = ledgerFake(t, http.StatusConflict, `{"error":"ledger_ack_backward","reason":"the cursor is at 50"}`)
	c = Client{URL: h.URL, Token: "tok"}
	if _, err = c.AckLedger(context.Background(), "desk", 42); ErrorCode(err) != "ledger_ack_backward" {
		t.Fatalf("backward err=%v", err)
	}
}

func TestStatusUpdatesClient(t *testing.T) {
	h, calls := ledgerFake(t, http.StatusCreated, `{"id":3,"project":"exp","health":"at_risk","body":"slipping","snapshot":{"started":2},"diff":{"previous_id":null}}`)
	c := Client{URL: h.URL, Token: "tok"}
	x, err := c.CreateStatusUpdate(context.Background(), "exp", "at_risk", "slipping")
	if err != nil || x.ID != 3 || x.Snapshot.Started != 2 || x.Diff.PreviousID != nil {
		t.Fatalf("update=%+v err=%v", x, err)
	}
	if g := (*calls)[0]; g.method != "POST" || g.path != "/v1/projects/exp/status-updates" || g.body["raw"] != `{"body":"slipping","health":"at_risk"}` {
		t.Fatalf("call=%+v", g)
	}
	h, calls = ledgerFake(t, http.StatusOK, `{"project":"exp","stale":true,"last_update_at":null,"updates":[]}`)
	c = Client{URL: h.URL, Token: "tok"}
	list, err := c.ListStatusUpdates(context.Background(), "exp", 5)
	if err != nil || !list.Stale || list.Project != "exp" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if g := (*calls)[0]; g.method != "GET" || g.path != "/v1/projects/exp/status-updates" || g.query != "limit=5" {
		t.Fatalf("call=%+v", g)
	}
}

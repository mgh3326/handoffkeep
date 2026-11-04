package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type decisionCall struct {
	method, path, query, auth string
	body                      map[string]any
}

func decisionFake(t *testing.T, response string) (*httptest.Server, *[]decisionCall) {
	t.Helper()
	calls := []decisionCall{}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := decisionCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &c.body)
		}
		calls = append(calls, c)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(h.Close)
	return h, &calls
}

func TestListDecisionRecordsQuery(t *testing.T) {
	h, calls := decisionFake(t, `{"server_time":"2026-11-05T00:00:00Z","decision_requests":[{"id":7,"source":"chat","source_ref":"Q-20261105-01","status":"open"}]}`)
	c := Client{URL: h.URL, Token: "tok"}
	got, err := c.ListDecisionRecords(context.Background(), store.DecisionRecordFilter{Status: "expired", Lane: "desk-lane", Source: "chat", IncludeSnoozed: true, Limit: 5})
	if err != nil || len(got.DecisionRequests) != 1 || got.DecisionRequests[0].ID != 7 {
		t.Fatalf("list=%+v err=%v", got, err)
	}
	if _, err := c.ListDecisionRecords(context.Background(), store.DecisionRecordFilter{}); err != nil {
		t.Fatal(err)
	}
	want := []decisionCall{
		{method: "GET", path: "/v1/decision-requests", query: "include_snoozed=true&lane=desk-lane&limit=5&source=chat&status=expired", auth: "Bearer tok"},
		{method: "GET", path: "/v1/decision-requests", query: "", auth: "Bearer tok"},
	}
	for i, w := range want {
		g := (*calls)[i]
		if g.method != w.method || g.path != w.path || g.query != w.query || g.auth != w.auth {
			t.Fatalf("call %d=%+v, want %+v", i, g, w)
		}
	}
}

func TestSnoozeDecisionRecordBody(t *testing.T) {
	h, calls := decisionFake(t, `{"id":7,"status":"open","snoozed":true}`)
	c := Client{URL: h.URL, Token: "tok"}
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.FixedZone("KST", 9*3600))
	got, err := c.SnoozeDecisionRecord(context.Background(), 7, until)
	if err != nil || !got.Snoozed {
		t.Fatalf("snooze=%+v err=%v", got, err)
	}
	g := (*calls)[0]
	if g.method != "POST" || g.path != "/v1/decision-requests/7/snooze" || g.body["until"] != "2030-01-01T18:04:05Z" || len(g.body) != 1 {
		t.Fatalf("call=%+v", g)
	}
}

func TestOpenDecisionSourcesIsAGet(t *testing.T) {
	h, calls := decisionFake(t, `{"server_time":"2026-11-05T00:00:00Z","items":[{"source":"lane_event","source_ref":"12","lane":"l","created_at":"2026-11-01T00:00:00Z"}],"truncated":[]}`)
	c := Client{URL: h.URL, Token: "tok"}
	got, err := c.OpenDecisionSources(context.Background())
	if err != nil || len(got.Items) != 1 || got.Items[0].SourceRef != "12" {
		t.Fatalf("sources=%+v err=%v", got, err)
	}
	if g := (*calls)[0]; g.method != "GET" || g.path != "/v1/decision-requests/sources" || g.body != nil {
		t.Fatalf("call=%+v", g)
	}
}

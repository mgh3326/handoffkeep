package tests

// GET /ui/api/attention (hk 3141): what is waiting on the operator, from the
// database alone.

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func getAttention(t *testing.T, client *http.Client, base, assertion string) (int, map[string]any) {
	t.Helper()
	response := uiRequest(t, client, http.MethodGet, base+"/ui/api/attention", assertion, "")
	status := response.StatusCode
	var out map[string]any
	if err := json.Unmarshal([]byte(responseText(t, response)), &out); err != nil {
		t.Fatalf("status=%d: %v", status, err)
	}
	return status, out
}

func keysOf(t *testing.T, value any) []string {
	t.Helper()
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", value)
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func wantKeys(t *testing.T, what string, value any, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := keysOf(t, value); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s keys=%v, want %v", what, got, want)
	}
}

func execAttention(t *testing.T, db *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestUIAttentionShape(t *testing.T) {
	s, db := attentionSchemaStore(t, "ui_attention")
	seedDeployGolden(t, s, db)
	now := time.Now().UTC()
	execAttention(t, db, `INSERT INTO decision_requests(source,source_ref,lane,question,due_at,status,created_at,updated_at)
		VALUES('chat','req-1','desk','Ship the console?',$1,'open',$2,$2)`, now.Add(-time.Minute), now.Add(-time.Hour))
	execAttention(t, db, `INSERT INTO tasks(lane,title,kind,state,awaiting_decision,created_by,created_at,updated_at)
		VALUES('desk','Waiting task','implement','developing',true,'test',$1,$1),
		      ('desk','Stalled task','implement','reviewing',false,'test',$2,$2)`, now.Add(-2*time.Hour), now.Add(-72*time.Hour))

	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	status, body := getAttention(t, h.Client(), h.URL, assertion)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	wantKeys(t, "document", body, "generated_at", "decisions", "tasks", "deploys")
	if _, err := time.Parse(time.RFC3339, body["generated_at"].(string)); err != nil {
		t.Fatalf("generated_at=%v", body["generated_at"])
	}

	decisions := body["decisions"].(map[string]any)
	wantKeys(t, "decisions", decisions, "count", "overdue", "items")
	items := decisions["items"].([]any)
	if decisions["count"] != float64(1) || decisions["overdue"] != float64(1) || len(items) != 1 {
		t.Fatalf("decisions=%v", decisions)
	}
	wantKeys(t, "decision item", items[0], "key", "source", "id", "lane", "question", "created_at", "due_at", "overdue", "read", "woken")
	d := items[0].(map[string]any)
	if d["source"] != "request" || d["lane"] != "desk" || d["question"] != "Ship the console?" || d["overdue"] != true || d["due_at"] == nil {
		t.Fatalf("decision item=%v", d)
	}
	if want := "decision:request:" + strconv.FormatInt(int64(d["id"].(float64)), 10); d["key"] != want {
		t.Fatalf("decision key=%v, want %q", d["key"], want)
	}
	if d["read"] != false || d["woken"] != false {
		t.Fatalf("decision read=%v woken=%v, want false false", d["read"], d["woken"])
	}

	tasks := body["tasks"].(map[string]any)
	wantKeys(t, "tasks", tasks, "count", "awaiting_decision", "stalled", "items")
	if tasks["count"] != float64(2) || tasks["awaiting_decision"] != float64(1) || tasks["stalled"] != float64(1) {
		t.Fatalf("tasks=%v", tasks)
	}
	taskItems := tasks["items"].([]any)
	if len(taskItems) != 2 {
		t.Fatalf("task items=%v", taskItems)
	}
	wantKeys(t, "task item", taskItems[0], "key", "id", "title", "lane", "state", "priority", "reason", "since", "read", "woken")
	first, second := taskItems[0].(map[string]any), taskItems[1].(map[string]any)
	if first["title"] != "Waiting task" || first["reason"] != "awaiting_decision" || second["title"] != "Stalled task" || second["reason"] != "stalled" || second["state"] != "reviewing" {
		t.Fatalf("task items=%v", taskItems)
	}
	for _, item := range []map[string]any{first, second} {
		if want := "task:" + strconv.FormatInt(int64(item["id"].(float64)), 10); item["key"] != want {
			t.Fatalf("task key=%v, want %q", item["key"], want)
		}
		if item["read"] != false || item["woken"] != false {
			t.Fatalf("task %v read=%v woken=%v, want false false", item["title"], item["read"], item["woken"])
		}
	}

	deploys := body["deploys"].(map[string]any)
	wantKeys(t, "deploys", deploys, "count", "events_capped", "items")
	deployItems := deploys["items"].([]any)
	// auto_trader's record has no deployed_at, so its merge counts from the
	// record key: three merges wait, newest first.
	if deploys["count"] != float64(3) || deploys["events_capped"] != false || len(deployItems) != 3 {
		t.Fatalf("deploys=%v", deploys)
	}
	wantKeys(t, "deploy item", deployItems[0], "key", "service", "task_id", "title", "pr", "merged_at", "read", "woken")
	newest, keyed, older := deployItems[0].(map[string]any), deployItems[1].(map[string]any), deployItems[2].(map[string]any)
	if newest["title"] != "newer after the deploy" || newest["service"] != "handoffkeep" || newest["pr"] != "https://github.com/mgh3326/handoffkeep/pull/12" ||
		keyed["title"] != "other service" || keyed["service"] != "auto_trader" ||
		older["title"] != "after the deploy" {
		t.Fatalf("deploy items=%v", deployItems)
	}
	for _, item := range []map[string]any{newest, keyed, older} {
		if want := "deploy:" + item["service"].(string) + ":" + strconv.FormatInt(int64(item["task_id"].(float64)), 10); item["key"] != want {
			t.Fatalf("deploy key=%v, want %q", item["key"], want)
		}
		if item["read"] != false || item["woken"] != false {
			t.Fatalf("deploy %v read=%v woken=%v, want false false", item["title"], item["read"], item["woken"])
		}
	}
}

func TestUIAttentionEmptyListsAreArrays(t *testing.T) {
	s, _ := attentionSchemaStore(t, "ui_attention_empty")
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	status, body := getAttention(t, h.Client(), h.URL, assertion)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	for _, section := range []string{"decisions", "tasks", "deploys"} {
		part := body[section].(map[string]any)
		if items, ok := part["items"].([]any); !ok || len(items) != 0 || part["count"] != float64(0) {
			t.Fatalf("%s=%v, want count 0 and an empty array", section, part)
		}
	}
}

func TestUIAttentionAuthBoundary(t *testing.T) {
	s, _ := attentionSchemaStore(t, "ui_attention_auth")
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/attention", "", "")
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	}
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	response = uiRequest(t, h.Client(), http.MethodPost, h.URL+"/ui/api/attention", assertion, "")
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", response.StatusCode)
	}

	// A service principal is authenticated but not an operator.
	service := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer service.Close()
	response = p3Request(t, service.Client(), http.MethodGet, service.URL+"/ui/api/attention", p3ServiceAssertion(t, fixture), nil, "")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("service principal status=%d", response.StatusCode)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(responseText(t, response)), &body); err != nil || body["error"] != "forbidden" {
		t.Fatalf("service principal body=%v err=%v", body, err)
	}
}

func TestUIAttentionStoreFailure(t *testing.T) {
	s, _ := attentionSchemaStore(t, "ui_attention_fail")
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	// A closed pool fails every read.
	s.Close()
	status, body := getAttention(t, h.Client(), h.URL, assertion)
	if status != http.StatusInternalServerError || !reflect.DeepEqual(body, map[string]any{"error": "internal_error"}) {
		t.Fatalf("status=%d body=%v, want 500 {\"error\":\"internal_error\"}", status, body)
	}
}

type countingTransport struct{ requests atomic.Int64 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.requests.Add(1)
	return nil, errors.New("hub is not reachable in this test")
}

// The attention route never calls the hub; the glance route does. The second
// half is what shows the counter is wired to the hub client.
func TestUIAttentionMakesNoHubCalls(t *testing.T) {
	s, _ := attentionSchemaStore(t, "ui_attention_hub")
	fixture := newUIJWTFixture(t)
	counter := &countingTransport{}
	h := newUITestServerWithClient(t, s, fixture, "http://hub.invalid", "hub-token", 0, &http.Client{Transport: counter})
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	if status, body := getAttention(t, h.Client(), h.URL, assertion); status != http.StatusOK {
		t.Fatalf("attention status=%d body=%v", status, body)
	}
	if n := counter.requests.Load(); n != 0 {
		t.Fatalf("attention made %d hub requests, want 0", n)
	}
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/glance", assertion, "")
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("glance status=%d", response.StatusCode)
	}
	if counter.requests.Load() == 0 {
		t.Fatal("glance made no hub request: the counter is not wired to the hub client")
	}
}

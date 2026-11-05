package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type seenRequest struct {
	method, path string
	body         map[string]json.RawMessage
}

func fakeServer(t *testing.T, status int, reply string) (*Client, *[]seenRequest) {
	t.Helper()
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := seenRequest{method: r.Method, path: r.URL.RequestURI()}
		_ = json.NewDecoder(r.Body).Decode(&x.body)
		seen = append(seen, x)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Token: "t"}, &seen
}

func TestIntakeClient(t *testing.T) {
	c, seen := fakeServer(t, http.StatusCreated, `{"created":true,"task":{"id":5,"state":"triage"}}`)
	x, created, err := c.Intake(context.Background(), store.IntakeInput{Source: "dots", SourceRef: "r", Title: "t", Body: "b"})
	if err != nil || !created || x.ID != 5 {
		t.Fatalf("intake x=%+v created=%t err=%v", x, created, err)
	}
	req := (*seen)[0]
	if req.method != http.MethodPost || req.path != "/v1/intake" || string(req.body["source_ref"]) != `"r"` || string(req.body["body"]) != `"b"` {
		t.Fatalf("request=%+v", req)
	}
	if _, present := req.body["project"]; present {
		t.Fatalf("unset project sent: %s", req.body["project"])
	}
	// A 2xx without the created flag is not trusted.
	c, _ = fakeServer(t, http.StatusOK, `{"id":5}`)
	if _, _, err := c.Intake(context.Background(), store.IntakeInput{Source: "dots", SourceRef: "r", Title: "t"}); err == nil || !strings.Contains(err.Error(), "intake_response_malformed") {
		t.Fatalf("malformed err=%v", err)
	}
}

func TestEmitTicketSetClient(t *testing.T) {
	c, seen := fakeServer(t, http.StatusOK, `{"id":1,"source":"repo-session","source_ref":"s","parent_id":10,"children":[{"ref":"a","id":11}],"created":false}`)
	in := store.TicketSetInput{Source: "repo-session", SourceRef: "s", Parent: store.Ticket{Title: "p", Body: "b"}, Children: []store.Ticket{{Ref: "a", Title: "a", Body: "b", BlockedBy: []store.TicketBlocker{{TaskID: 3}}}}}
	set, created, err := c.EmitTicketSet(context.Background(), in)
	if err != nil || created || set.ParentID != 10 || set.Children[0].ID != 11 {
		t.Fatalf("set=%+v created=%t err=%v", set, created, err)
	}
	req := (*seen)[0]
	if req.path != "/v1/ticket-sets" || !strings.Contains(string(req.body["children"]), `"blocked_by":[3]`) {
		t.Fatalf("request=%+v children=%s", req, req.body["children"])
	}
	c, _ = fakeServer(t, http.StatusConflict, `{"error":"ticket_set_conflict","reason":"this source_ref was emitted with a different payload"}`)
	if _, _, err := c.EmitTicketSet(context.Background(), in); ErrorCode(err) != "ticket_set_conflict" {
		t.Fatalf("conflict err=%v", err)
	}
}

func TestListTriageClient(t *testing.T) {
	c, seen := fakeServer(t, http.StatusOK, `{"triage":[{"task":{"id":3,"title":"p"},"ticket_set":true,"child_count":2,"children_in_triage":2,"age_seconds":5,"similar":[]}]}`)
	rows, err := c.ListTriage(context.Background(), 50)
	if err != nil || len(rows) != 1 || !rows[0].TicketSet || rows[0].ChildCount != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if (*seen)[0].path != "/v1/triage?limit=50" {
		t.Fatalf("path=%s", (*seen)[0].path)
	}
	c, _ = fakeServer(t, http.StatusOK, `{}`)
	if _, err := c.ListTriage(context.Background(), 50); err == nil {
		t.Fatal("a body without the triage list was accepted")
	}
}

// link and new are sent only when set, so a client that answers nothing
// keeps working against a server that predates the duplicate check; a 409
// similar_tasks carries the candidates.
func TestCreateTaskDuplicateFlagsWireShape(t *testing.T) {
	c, seen := fakeServer(t, http.StatusCreated, `{"id":9,"project":"experiment"}`)
	x := store.Task{Lane: "d", Title: "x", Kind: "fix", Project: areaStr("experiment")}
	if _, err := c.CreateTask(context.Background(), x); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTaskChecked(context.Background(), x, store.DuplicateCheck{Link: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTaskChecked(context.Background(), x, store.DuplicateCheck{New: true}); err != nil {
		t.Fatal(err)
	}
	plain, linked, fresh := (*seen)[0].body, (*seen)[1].body, (*seen)[2].body
	if _, ok := plain["link"]; ok {
		t.Fatalf("plain create sent link: %v", plain)
	}
	if _, ok := plain["new"]; ok {
		t.Fatalf("plain create sent new: %v", plain)
	}
	if string(linked["link"]) != "4" || string(fresh["new"]) != "true" {
		t.Fatalf("link=%s new=%s", linked["link"], fresh["new"])
	}
	c, _ = fakeServer(t, http.StatusConflict, `{"error":"similar_tasks","similar_tasks":[{"id":4,"title":"x again","state":"backlog","lane":"d","similarity":0.9}]}`)
	_, err := c.CreateTask(context.Background(), x)
	var he *HTTPError
	if ErrorCode(err) != "similar_tasks" || !errors.As(err, &he) || len(he.SimilarTasks) != 1 || he.SimilarTasks[0].ID != 4 || !strings.Contains(err.Error(), `#4 "x again" (backlog)`) {
		t.Fatalf("similar err=%v", err)
	}
}

// The triage fields of a transition are sent only when set.
func TestTransitionTriageFieldsWireShape(t *testing.T) {
	c, seen := fakeServer(t, http.StatusOK, `{"id":3,"state":"backlog"}`)
	priority, project := 2, "experiment"
	if _, err := c.TransitionTaskWith(context.Background(), 3, store.TransitionInput{To: "backlog", Priority: &priority, Kind: "fix", Project: &project, Set: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TransitionTaskWith(context.Background(), 3, store.TransitionInput{To: "backlog"}); err != nil {
		t.Fatal(err)
	}
	with, without := (*seen)[0].body, (*seen)[1].body
	if string(with["priority"]) != "2" || string(with["kind"]) != `"fix"` || string(with["project"]) != `"experiment"` || string(with["set"]) != "true" {
		t.Fatalf("with=%v", with)
	}
	for _, k := range []string{"priority", "kind", "project", "set"} {
		if _, ok := without[k]; ok {
			t.Fatalf("unset %s sent: %v", k, without)
		}
	}
}

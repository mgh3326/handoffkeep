package clientparity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/mgh3326/handoffkeep/client"
	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// recorder answers every request with one fixed body and keeps the request
// lines it saw.
type recorder struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newRecorder(t *testing.T, body []byte) *recorder {
	t.Helper()
	r := &recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, req.Method+" "+req.URL.RequestURI())
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.seen
	r.seen = nil
	return out
}

// The list calls send the request internal/remote (the hk CLI's client)
// sends for the same filters, and read the answer envelope the same way: a
// server-shaped list decodes through both clients to the same values.
func TestListCallsMatchRemote(t *testing.T) {
	ctx := context.Background()

	var task store.Task
	fill(reflect.ValueOf(&task).Elem(), "task", 0)
	taskBody, err := json.Marshal(map[string]any{"tasks": []store.Task{task, task}})
	if err != nil {
		t.Fatal(err)
	}
	hk := newRecorder(t, taskBody)
	rc, cc := remote.Client{URL: hk.URL, Token: "t"}, client.Client{URL: hk.URL, Token: "t"}
	for _, in := range []client.ListTasksInput{
		{Limit: 1000},
		{Lane: "panewire-design", State: "developing", Limit: 20},
		{Lane: "a b&c", State: "backlog", ParentLane: "operator/desk", Limit: 1},
	} {
		want, err := rc.ListTasks(ctx, in.Lane, in.State, in.ParentLane, nil, nil, in.Limit)
		if err != nil {
			t.Fatalf("remote ListTasks %+v: %v", in, err)
		}
		wantReq := hk.take()
		got, err := cc.ListTasks(ctx, in)
		if err != nil {
			t.Fatalf("client ListTasks %+v: %v", in, err)
		}
		if gotReq := hk.take(); !reflect.DeepEqual(gotReq, wantReq) {
			t.Fatalf("ListTasks %+v sent %v, internal/remote sends %v", in, gotReq, wantReq)
		}
		if len(got) != len(want) || len(got) != 2 {
			t.Fatalf("ListTasks decoded %d tasks, internal/remote %d", len(got), len(want))
		}
		for i := range got {
			for _, bad := range subset(generic(t, got[i]), generic(t, want[i]), "client.Task") {
				t.Error(bad)
			}
		}
		// The fields the fleet's task lookups read, named explicitly.
		if got[0].Priority != want[0].Priority || got[0].Expected == nil || *got[0].Expected != *want[0].Expected ||
			got[0].Refs.JobID != want[0].Refs.JobID || got[0].Refs.ReportPath != want[0].Refs.ReportPath {
			t.Fatalf("client task %+v does not carry the remote task's values %+v", got[0], want[0])
		}
	}

	var doc store.Document
	fill(reflect.ValueOf(&doc).Elem(), "doc", 0)
	docBody, err := json.Marshal(map[string]any{"documents": []store.Document{doc}})
	if err != nil {
		t.Fatal(err)
	}
	hk = newRecorder(t, docBody)
	rc, cc = remote.Client{URL: hk.URL, Token: "t"}, client.Client{URL: hk.URL, Token: "t"}
	for _, in := range []struct {
		prefix string
		limit  int
	}{
		{"artifacts/handoffkeep/42/job-7", 100},
		{"", 1000},
		{"a b&c=d/é", 1},
	} {
		want, err := rc.ListDocuments(ctx, in.prefix, "", "", 0, in.limit)
		if err != nil {
			t.Fatalf("remote ListDocuments %+v: %v", in, err)
		}
		wantReq := hk.take()
		got, err := cc.ListDocuments(ctx, in.prefix, in.limit)
		if err != nil {
			t.Fatalf("client ListDocuments %+v: %v", in, err)
		}
		if gotReq := hk.take(); !reflect.DeepEqual(gotReq, wantReq) {
			t.Fatalf("ListDocuments %+v sent %v, internal/remote sends %v", in, gotReq, wantReq)
		}
		if len(got) != 1 || len(want) != 1 {
			t.Fatalf("ListDocuments decoded %d documents, internal/remote %d", len(got), len(want))
		}
		for _, bad := range subset(generic(t, got[0]), generic(t, want[0]), "client.Document") {
			t.Error(bad)
		}
		if got[0].Key != want[0].Key || got[0].Key == "" {
			t.Fatalf("document key %q, internal/remote %q", got[0].Key, want[0].Key)
		}
	}
}

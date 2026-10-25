package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func areaStr(v string) *string { return &v }

// CreateTask omits the area key when the task has none, so a new client does
// not trip the unknown-field decoder of a server that predates the column;
// with an area it sends the value and checks the server recorded it.
func TestCreateTaskAreaWireShape(t *testing.T) {
	var body map[string]json.RawMessage
	respond := func(area string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body = nil
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1,"project":"experiment"` + area + `}`))
		}))
	}
	srv := respond("")
	defer srv.Close()
	c := Client{URL: srv.URL, Token: "t"}
	if _, err := c.CreateTask(context.Background(), store.Task{Lane: "d", Title: "x", Kind: "implement", Project: areaStr("experiment")}); err != nil {
		t.Fatal(err)
	}
	if _, present := body["area"]; present {
		t.Fatalf("area key sent for an untagged task: %s", body["area"])
	}
	// Tagged task against a server that echoes the area.
	ok := respond(`,"area":"docs"`)
	defer ok.Close()
	c = Client{URL: ok.URL, Token: "t"}
	if _, err := c.CreateTask(context.Background(), store.Task{Lane: "d", Title: "x", Kind: "implement", Project: areaStr("experiment"), Area: areaStr("docs")}); err != nil {
		t.Fatal(err)
	}
	if string(body["area"]) != `"docs"` {
		t.Fatalf("area on the wire = %s", body["area"])
	}
	// Tagged task against a server that silently drops the field.
	c = Client{URL: srv.URL, Token: "t"}
	_, err := c.CreateTask(context.Background(), store.Task{Lane: "d", Title: "x", Kind: "implement", Project: areaStr("experiment"), Area: areaStr("docs")})
	if err == nil || !strings.Contains(err.Error(), "create_area_not_recorded") {
		t.Fatalf("dropped area err=%v, want create_area_not_recorded", err)
	}
}

func TestSetTaskAreaWireShape(t *testing.T) {
	var path string
	var body map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"id": 7, "area": nil}
		if string(body["area"]) != "null" {
			resp["area"] = "ops"
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	c := Client{URL: srv.URL, Token: "t"}
	x, err := c.SetTaskArea(context.Background(), 7, areaStr("ops"), "why")
	if err != nil || x.Area == nil || *x.Area != "ops" || path != "/v1/tasks/7/area" || string(body["area"]) != `"ops"` || string(body["note"]) != `"why"` {
		t.Fatalf("set: x=%+v err=%v path=%s body=%v", x, err, path, body)
	}
	x, err = c.SetTaskArea(context.Background(), 7, nil, "")
	if err != nil || x.Area != nil || string(body["area"]) != "null" {
		t.Fatalf("clear: x=%+v err=%v body=%v", x, err, body)
	}
}

func TestListTaskAreasAndFilterEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/task-areas":
			_, _ = w.Write([]byte(`{"areas":["blender","unity"]}`))
		default:
			if r.URL.Query().Get("area") == "docs" {
				_, _ = w.Write([]byte(`{"tasks":[{"id":1,"area":"ops"}],"area":"docs"}`))
				return
			}
			_, _ = w.Write([]byte(`{"tasks":[]}`))
		}
	}))
	defer srv.Close()
	c := Client{URL: srv.URL, Token: "t"}
	got, err := c.ListTaskAreas(context.Background())
	if err != nil || strings.Join(got, ",") != "blender,unity" {
		t.Fatalf("areas=%v err=%v", got, err)
	}
	// A row that does not carry the requested area exposes a server that
	// ignored the filter even when the echo is present.
	if _, err = c.ListTasks(context.Background(), "", "", "", nil, areaStr("docs"), 5); err == nil || !strings.Contains(err.Error(), "list_area_filter_ignored") {
		t.Fatalf("mismatched row err=%v", err)
	}
	if _, err = c.ListTasks(context.Background(), "", "", "", nil, areaStr("ops"), 5); err == nil || !strings.Contains(err.Error(), "list_area_filter_ignored") {
		t.Fatalf("missing echo err=%v", err)
	}
	if _, err = c.ListTasks(context.Background(), "", "", "", nil, nil, 5); err != nil {
		t.Fatalf("unfiltered list err=%v", err)
	}
}

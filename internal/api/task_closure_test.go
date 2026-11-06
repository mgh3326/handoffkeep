package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestTaskTransitionClosureAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	for _, tc := range []struct {
		kind, body string
		status     int
		code       string
	}{
		{"decide", `{"to":"completed","evidence":"https://example.com/r"}`, http.StatusOK, ""},
		{"decide", `{"to":"completed"}`, http.StatusBadRequest, "closure_invalid"},
		{"implement", `{"to":"completed","evidence":"https://example.com/r"}`, http.StatusConflict, "task_conflict"},
		{"decide", `{"to":"completed","closure":"unknown","evidence":"https://example.com/r"}`, http.StatusBadRequest, "closure_invalid"},
	} {
		x, err := st.CreateTask(t.Context(), store.Task{Lane: relaneLane(t, "closure"), Title: "closure", Kind: tc.kind, CreatedBy: "tester", Project: &apiTestProject})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/v1/tasks/%d/transition", h.URL, x.ID), bytes.NewBufferString(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Error  string         `json:"error"`
			Reason string         `json:"reason"`
			Refs   store.TaskRefs `json:"refs"`
		}
		err = json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != tc.status || out.Error != tc.code {
			t.Fatalf("status=%d out=%+v", res.StatusCode, out)
		}
		if tc.code == "closure_invalid" && out.Reason == "" {
			t.Fatal("missing reason")
		}
		if tc.status == http.StatusOK && (out.Refs.Closure == nil || out.Refs.Closure.Type != "resolved") {
			t.Fatalf("refs=%+v", out.Refs)
		}
	}
}

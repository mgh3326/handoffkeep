package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestTaskReopenAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	x := relaneAPITask(t, st, relaneLane(t, "reopen"), "failed release")
	if _, err := st.TransitionTask(context.Background(), x.ID, "dropped", "tester", "closed", nil, ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body   string
		status int
	}{{`{}`, 400}, {`{"reason":"  "}`, 400}, {`{"reason":"reverted"}`, 200}, {`{"reason":"again"}`, 409}} {
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/tasks/%d/reopen", h.URL, x.ID), bytes.NewBufferString(tc.body))
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: status %d want %d", tc.body, resp.StatusCode, tc.status)
		}
	}
}

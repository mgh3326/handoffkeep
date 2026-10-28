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
		if tc.status == 200 {
			// v86-pi mutant 2: the reopen event must record the token's
			// client and the caller's reason, not a constant.
			got, ok, err := st.GetTask(context.Background(), x.ID)
			if err != nil || !ok || len(got.Events) == 0 {
				t.Fatalf("reread task: ok=%v err=%v", ok, err)
			}
			last := got.Events[len(got.Events)-1]
			if last.To != "reopened" || last.By != "tester" || last.Note != "reverted" {
				t.Fatalf("reopen caller binding: to=%q by=%q note=%q, want reopened/tester/reverted", last.To, last.By, last.Note)
			}
		}
	}
}

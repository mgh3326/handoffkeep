package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// A fence refusal answers 409 job_owner_mismatch and names the record's
// current owner (hk 5276). The bare sentinel maps through errorRules too.
func TestAppErrJobOwnerMismatch(t *testing.T) {
	refusal := &store.JobOwnerMismatchError{Owner: store.JobOwner{Node: "node-a", Epoch: 3}}
	for _, e := range []error{refusal, fmt.Errorf("update: %w", refusal)} {
		rec := httptest.NewRecorder()
		appErr(rec, httptest.NewRequest(http.MethodPut, "/v1/jobs/job-x", nil), e)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%v: body %q: %v", e, rec.Body.String(), err)
		}
		if rec.Code != http.StatusConflict || body["error"] != "job_owner_mismatch" || body["owner_node"] != "node-a" || body["owner_epoch"] != float64(3) {
			t.Fatalf("%v: %d %v", e, rec.Code, body)
		}
	}

	rec := httptest.NewRecorder()
	appErr(rec, httptest.NewRequest(http.MethodPut, "/v1/jobs/job-x", nil), store.ErrJobOwnerMismatch)
	if rec.Code != http.StatusConflict || rec.Body.String() != `{"error":"job_owner_mismatch"}`+"\n" {
		t.Fatalf("bare sentinel: %d %q", rec.Code, rec.Body.String())
	}
}

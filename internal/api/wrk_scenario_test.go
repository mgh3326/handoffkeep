package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// scenario drives the real /v1/wrk handler over a fresh memory journal for
// design-derived tests that do not come from the M1 pack.
type scenario struct {
	*packEnv
	mu   sync.Mutex
	keys int
}

func standardPolicy(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(packRoot, "vectors", "transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vf struct {
		Cases []struct {
			Given struct {
				Policy json.RawMessage `json:"policy"`
			} `json:"given"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &vf); err != nil {
		t.Fatal(err)
	}
	return vf.Cases[0].Given.Policy
}

func newScenario(t *testing.T) *scenario {
	t.Helper()
	c := &packCase{File: "scenario", Name: t.Name()}
	c.Given.Policy = standardPolicy(t)
	env := newPackEnv(t, c, nil, false)
	for id := int64(2400); id <= 2430; id++ {
		env.mem.AddTask(id)
	}
	return &scenario{packEnv: env}
}

// key returns a fresh Idempotency-Key; racing goroutines may call it.
func (s *scenario) key() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", s.keys)
}

// cas returns the job's current (claim_seq, tail).
func (s *scenario) cas(job string) (int64, int64) {
	recs := s.records(job)
	var claim, tail int64
	for _, r := range recs {
		tail = r.Seq
		if r.Kind == "claim" {
			claim = r.Seq
		}
	}
	return claim, tail
}

func (s *scenario) postBody(actor, job, key string, body map[string]any) packResponse {
	s.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		s.t.Fatal(err)
	}
	return s.do(actor, "POST", "/v1/wrk/jobs/"+job+"/records", map[string]string{"Idempotency-Key": key}, b)
}

// post appends with the current CAS and a fresh key.
func (s *scenario) post(actor, job, kind string, payload map[string]any) packResponse {
	s.t.Helper()
	claim, tail := s.cas(job)
	return s.postBody(actor, job, s.key(), map[string]any{"kind": kind, "claim_seq": claim, "expected_seq": tail, "payload": payload})
}

func (s *scenario) ok(r packResponse, what string) map[string]any {
	s.t.Helper()
	if r.status != 201 && r.status != 200 {
		s.t.Fatalf("%s: status %d %s", what, r.status, r.raw)
	}
	return r.body
}

func (s *scenario) refused(r packResponse, code string, what string) {
	s.t.Helper()
	if r.body["error"] != code {
		s.t.Fatalf("%s: got %d %s, want %s", what, r.status, r.raw, code)
	}
}

func head(c byte) string { return strings.Repeat(string(c), 40) }

func claimPayload(job, owner, parent string, task int64, host, role, tier string) map[string]any {
	return map[string]any{"job_id": job, "owner_lane": owner, "parent_lane": parent, "task_id": task, "host_client_id": host, "role": role, "tier": tier, "review_of": nil, "predecessor_job_id": nil, "supersedes": nil}
}

func reviewPayload(job, lane string, task int64, host, role, tier, target string, round int64) map[string]any {
	p := claimPayload(job, lane, lane, task, host, role, tier)
	p["review_of"] = map[string]any{"job_id": target, "round_no": round}
	return p
}

// activeBuilder creates and activates a desktop builder job.
func (s *scenario) activeBuilder(job, lane string, task int64, tier string) {
	s.t.Helper()
	s.ok(s.post("director-1", job, "claim", claimPayload(job, lane, "director-1", task, "desktop", "builder", tier)), "claim "+job)
	s.ok(s.post("director-1", job, "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "activate "+job)
}

func openPayload(h string, tester, ext, rep any) map[string]any {
	return map[string]any{"head": h, "tester_job_id": tester, "extension_grant_seq": ext, "repeat_grant_seq": rep}
}

func verdictPayload(round int64, verdict, h, path string, tester any, content []byte) map[string]any {
	return map[string]any{"round_no": round, "head": h, "verdict": verdict, "path": path, "content_sha256": wrk.ContentSHA256(content), "content_b64": base64.StdEncoding.EncodeToString(content), "tester_job_id": tester}
}

func verdictBytes(verdict, h, note string) []byte {
	return []byte("VERDICT: " + verdict + " @" + h + "\n" + note + "\n")
}

func grantPayload(typ string, round any, h any, extra int, earlier, tester any) map[string]any {
	return map[string]any{"grant_type": typ, "round_no": round, "head": h, "extra": extra, "earlier_round_no": earlier, "tester_job_id": tester, "expires_at": wrk.FormatTime(packNow.Add(72 * time.Hour)), "reason_code": "test_grant"}
}

func reportPayload(h string, waiver any) map[string]any {
	return map[string]any{"head": h, "pr": "https://pr.invalid/handoffkeep/pull/1", "report_b64": base64.StdEncoding.EncodeToString(packReportBytes), "report_sha256": wrk.ContentSHA256(packReportBytes), "waiver_grant_seq": waiver}
}

// summaryOf takes a packResponse or an ok() body.
func summaryOf(v any) map[string]any {
	body, _ := v.(map[string]any)
	if r, ok := v.(packResponse); ok {
		body = r.body
	}
	m, _ := body["summary"].(map[string]any)
	return m
}

func num(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return -1
}

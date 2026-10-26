package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// deskOwned lists vectors whose only observable is a desk operation
// (design §9 D1/D2), not an S1 server or CLI behaviour.
var deskOwned = map[string]string{
	"r4-scoped-claim.json/p2-gate-unverified-durability-refused": "§9 D1/D2: the P2 gate is a desk operation, not an API call",
}

// m1Counts is the M1 harness's per-file case list
// (internal/o3contract expectedVectors).
var m1Counts = map[string]int{
	"section4-rules.json": 46, "transition.json": 27, "migration.json": 10, "legacy-route-fence.json": 12,
	"r3-regression.json": 13, "r4-scoped-claim.json": 39, "r4-review-lifecycle.json": 10,
	"head-repeat-binding.json": 7, "transport-intents.json": 14, "failure-abuse.json": 16,
}

// inCI reports a CI run, where every store-backed vector must execute.
func inCI() bool { return os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" }

// runHTTPVector executes an http/http_raw vector and returns its observed
// status, stable code (or "") and a mismatch description against the
// pack's expectation ("" when everything the vector states matched).
func runHTTPVector(t *testing.T, c *packCase, env *packEnv) (status int, code string, mismatch string) {
	t.Helper()
	method, _ := c.Request["method"].(string)
	path, _ := c.Request["path"].(string)
	actor, _ := c.Request["actor"].(string)
	if q, ok := c.Request["query"].(map[string]any); ok {
		v := url.Values{}
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v.Set(k, fmt.Sprint(q[k]))
		}
		path += "?" + v.Encode()
	}
	job := c.Given.JobID
	if seg := pathJob(path); seg != "" {
		job = seg
	}
	before := len(env.records(job))
	header := map[string]string{}
	if k, ok := c.Request["idempotency_key"].(string); ok {
		header["Idempotency-Key"] = k
	}
	var body []byte
	if method == "POST" {
		body = packRequestBody(t, c)
	}
	resp := env.do(actor, method, path, header, body)
	status = resp.status
	if e, ok := resp.body["error"].(string); ok && status >= 400 {
		code = e
		if len(resp.body) != 1 {
			return status, code, "error body carries more than the error field"
		}
	}
	exp := c.Expect
	var miss []string
	if st, ok := exp["http_status"].(json.Number); ok {
		if want, _ := st.Int64(); int(want) != status {
			miss = append(miss, fmt.Sprintf("status %d want %d", status, want))
		}
	}
	if e, ok := exp["error"].(string); ok && e != code {
		miss = append(miss, fmt.Sprintf("error %q want %q", code, e))
	}
	if cr, ok := exp["created"].(bool); ok {
		if got, _ := resp.body["created"].(bool); got != cr || resp.body["created"] == nil {
			miss = append(miss, fmt.Sprintf("created %v want %v", resp.body["created"], cr))
		}
	}
	if rec, ok := exp["record"].(map[string]any); ok {
		got, _ := resp.body["record"].(map[string]any)
		if m := subsetMismatch(rec, got); m != "" {
			miss = append(miss, "record: "+m)
		}
	}
	if sum, ok := exp["summary"].(map[string]any); ok {
		got, _ := resp.body["summary"].(map[string]any)
		if got == nil && status >= 400 {
			// A refusal carries no summary: read the current one.
			got, _ = env.do(actor, "GET", "/v1/wrk/jobs/"+job, nil, nil).body["summary"].(map[string]any)
		}
		if m := subsetMismatch(sum, got); m != "" {
			miss = append(miss, "summary: "+m)
		}
	}
	if ref, ok := exp["record_ref"].(map[string]any); ok {
		got, _ := resp.body["record"].(map[string]any)
		if m := subsetMismatch(ref, got); m != "" {
			miss = append(miss, "record_ref: "+m)
		}
	}
	if nn, _ := exp["no_new_records"].(bool); nn || status >= 400 {
		if after := len(env.records(job)); after != before {
			miss = append(miss, fmt.Sprintf("journal grew %d -> %d", before, after))
		}
	}
	if rr, ok := exp["records_response"].(map[string]any); ok {
		want := map[string]any{}
		for k, v := range rr {
			if k != "record_seqs" {
				want[k] = v
			}
		}
		if m := subsetMismatch(want, resp.body); m != "" {
			miss = append(miss, "records_response: "+m)
		}
		var seqs []any
		recs, _ := resp.body["records"].([]any)
		for _, r := range recs {
			seqs = append(seqs, r.(map[string]any)["seq"])
		}
		if !jsonEqual(seqs, rr["record_seqs"]) {
			miss = append(miss, fmt.Sprintf("record_seqs %v want %v", seqs, rr["record_seqs"]))
		}
	}
	if cp, ok := exp["capabilities"].(map[string]any); ok {
		if m := subsetMismatch(cp, resp.body); m != "" {
			miss = append(miss, "capabilities: "+m)
		}
	}
	if len(miss) > 0 {
		return status, code, fmt.Sprint(miss)
	}
	return status, code, ""
}

func pathJob(path string) string {
	const pre = "/v1/wrk/jobs/"
	if !strings.HasPrefix(path, pre) {
		return ""
	}
	rest := path[len(pre):]
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// checkHTTPVector executes one http vector and requires every observable it
// states. The M1 pack revision hk 2049 fixed every earlier pack defect, so
// there is no divergence table: any mismatch is a failure.
func checkHTTPVector(t *testing.T, c *packCase, newEnv func(t *testing.T, c *packCase) *packEnv) {
	t.Helper()
	env := newEnv(t, c)
	if status, code, miss := runHTTPVector(t, c, env); miss != "" {
		t.Fatalf("status=%d code=%q: %s", status, code, miss)
	}
}

func TestO3PackHTTPVectorsMemory(t *testing.T) {
	legacy := legacyBySHA(t)
	for _, c := range loadPackCases(t) {
		if rt := c.reqType(); rt != "http" && rt != "http_raw" {
			continue
		}
		c := c
		t.Run(c.id(), func(t *testing.T) {
			checkHTTPVector(t, c, func(t *testing.T, c *packCase) *packEnv { return newPackEnv(t, c, legacy, false) })
		})
	}
}

// TestO3PackGivenHistoriesValidate: every case with a given history passes
// the startup validation (the fixture_incomplete set was emptied when the hk
// 2059 pack revision landed).
func TestO3PackGivenHistoriesValidate(t *testing.T) {
	for _, c := range loadPackCases(t) {
		if len(c.Given.Records) == 0 {
			continue
		}
		policy, err := wrk.ParsePolicy(c.Given.Policy)
		if err != nil {
			t.Fatalf("%s: %v", c.id(), err)
		}
		if err := validateGivenHistory(policy, c.Given.Records); err != nil {
			t.Errorf("%s: %v", c.id(), err)
		}
	}
}

// TestO3PackDeskOwned pins every desk-owned entry to a real pack case.
func TestO3PackDeskOwned(t *testing.T) {
	cases := map[string]*packCase{}
	for _, c := range loadPackCases(t) {
		cases[c.id()] = c
	}
	for id := range deskOwned {
		if c, ok := cases[id]; !ok || c.reqType() != "none" {
			t.Errorf("deskOwned names %s, which is not a type-none pack case", id)
		}
	}
}

// TestO3PackAccounting asserts every pack case is consumed by exactly one
// runner of this harness (or by cmd/handoffkeep for the 6b cli cases) and
// that the per-file counts equal the M1 harness's list.
func TestO3PackAccounting(t *testing.T) {
	perFile := map[string]int{}
	for _, c := range loadPackCases(t) {
		perFile[c.File]++
		switch rt := c.reqType(); rt {
		case "http", "http_raw", "legacy", "policy", "meta", "cli":
		case "none":
			if _, ok := deskOwned[c.id()]; !ok {
				t.Errorf("%s: type none is not listed desk-owned", c.id())
			}
		default:
			t.Errorf("%s: no runner for type %q", c.id(), rt)
		}
	}
	for f, n := range m1Counts {
		if perFile[f] != n {
			t.Errorf("%s: %d cases, M1 harness lists %d", f, perFile[f], n)
		}
	}
	if len(perFile) != len(m1Counts) {
		t.Errorf("pack has %d vector files, M1 lists %d", len(perFile), len(m1Counts))
	}
}

func TestO3PackPolicyVectors(t *testing.T) {
	for _, c := range loadPackCases(t) {
		if c.reqType() != "policy" {
			continue
		}
		c := c
		t.Run(c.id(), func(t *testing.T) {
			doc, err := json.Marshal(c.Request["document"])
			if err != nil {
				t.Fatal(err)
			}
			_, err = wrk.ParsePolicy(doc)
			want, _ := c.Expect["policy_valid"].(bool)
			if (err == nil) != want {
				t.Fatalf("ParsePolicy valid=%v, vector says %v", err == nil, want)
			}
		})
	}
}

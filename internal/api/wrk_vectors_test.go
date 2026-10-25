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

// packDivergenceEntry records one pack vector that contradicts the fixed
// design r4 (sha ce4f6efb). Category design_result: the harness executes the
// vector and asserts the design outcome (Status, Code). Category
// fixture_invalid: the vector's seeded history is unreachable (an import
// seal with numbering_floor below baseline_used, design §5), so the request
// is not executed; the harness asserts the broken invariant statically.
// Both categories are pinned: a listed case that stops diverging (the hk
// 2049 pack revision) fails until its entry is removed, and an unlisted
// divergent case fails. Rulings: director-1 on b1925-opus esc-1 (E1-E6
// design_result; E7 fixture_invalid) and esc-2 (verdict order B,
// provisional).
type packDivergenceEntry struct {
	Category string
	Status   int
	Code     string
	Cite     string
}

const (
	divDesignResult   = "design_result"
	divFixtureInvalid = "fixture_invalid"
)

var packDivergence = map[string]packDivergenceEntry{
	// E1: the seeded verdict's request_sha256 is not SHA256(RFC8785({method,path,body})) of the replayed body (design §5 L4048, §4 L125).
	"failure-abuse.json/replay-old-verdict-identical-200":          {divDesignResult, 409, "idempotency_conflict", "E1 §5 request digest"},
	"failure-abuse.json/committed-write-lost-reply-replay-repairs": {divDesignResult, 409, "idempotency_conflict", "E1 §5 request digest"},
	"transition.json/replay-verdict-after-end-200":                 {divDesignResult, 409, "idempotency_conflict", "E1 §5 request digest"},
	// E2: stale expected_seq; CAS precedes business rules (design §4 L125).
	"failure-abuse.json/grant-consumer-race-void-reuse":                 {divDesignResult, 409, "sequence_conflict", "E2a §4 CAS (expected_seq 6, tail 9)"},
	"head-repeat-binding.json/voided-open-verdict-cannot-copy-old-path": {divDesignResult, 409, "sequence_conflict", "E2c §4 CAS (expected_seq 5, tail 6)"},
	"head-repeat-binding.json/reviewer-end-after-pass-join-unaffected":  {divDesignResult, 409, "sequence_conflict", "E2d §4 CAS (expected_seq 5, tail 4)"},
	"r4-scoped-claim.json/scoped-mint-reused-job-id-claim-conflict":     {divDesignResult, 409, "sequence_conflict", "E2e §4 CAS (expected_seq 5, tail 2)"},
	// E4: repeat_grant_seq names the verdict record, not the grant (design §4 L131).
	"failure-abuse.json/two-testers-one-head-hk1901": {divDesignResult, 409, "grant_invalid", "E4 §4 grant validation"},
	// E5: targets round 1, which already holds a verdict; under verdict order B the reused bytes are duplicate_content (§4 L79, esc-2 A).
	"head-repeat-binding.json/exact-b-open-verdict-null-tester-fails-assignment": {divDesignResult, 409, "duplicate_content", "E5 §2 L79 content identity, esc-2 order B"},
	// E6: the reviewed job is absent from given; R4 orders not_found first (design §5 L4088).
	"r4-scoped-claim.json/scoped-mint-foreign-job-host-mismatch": {divDesignResult, 404, "not_found", "E6 R4 acceptance order"},
	// E7: given seal numbering_floor 1 < baseline_used 3 (design §5 L4028).
	"migration.json/post-activate-verdict-old-round-not-open":        {divFixtureInvalid, 0, "", "E7 §5 seal invariant"},
	"migration.json/post-activate-void-old-round-not-open":           {divFixtureInvalid, 0, "", "E7 §5 seal invariant"},
	"migration.json/post-activate-extension-open-round-4":            {divFixtureInvalid, 0, "", "E7 §5 seal invariant"},
	"migration.json/void-postactivate-round-4-used-3":                {divFixtureInvalid, 0, "", "E7 §5 seal invariant"},
	"r3-regression.json/postactivate-open-historical-head-no-repeat": {divFixtureInvalid, 0, "", "E7 §5 seal invariant"},
	"transition.json/imported-activated-join-zero-rounds":            {divFixtureInvalid, 0, "", "E7 §5 seal invariant (also E3 actor)"},
	"transition.json/imported-activated-open-needs-extension":        {divFixtureInvalid, 0, "", "E7 §5 seal invariant"},
	"failure-abuse.json/planted-legacy-events-import-untrusted":      {divFixtureInvalid, 0, "", "E7 §5 seal invariant (also E2b CAS)"},
}

// sealInvariantBroken reports a seeded import_seal whose numbering_floor
// is below its baseline_used: a history the design (§5) cannot produce.
func sealInvariantBroken(c *packCase) bool {
	for _, r := range c.Given.Records {
		if r.Kind != "import_seal" {
			continue
		}
		b, _ := r.Payload["baseline_used"].(int64)
		f, _ := r.Payload["numbering_floor"].(int64)
		if f < b {
			return true
		}
	}
	return false
}

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

// checkHTTPVector applies the pinned divergence table to one executed (or
// statically checked) http vector.
func checkHTTPVector(t *testing.T, c *packCase, newEnv func(t *testing.T, c *packCase) *packEnv) {
	t.Helper()
	div, listed := packDivergence[c.id()]
	broken := sealInvariantBroken(c)
	if listed && div.Category == divFixtureInvalid {
		if !broken {
			t.Fatalf("fixture_invalid entry is stale: the seeded seal now satisfies numbering_floor >= baseline_used; remove it")
		}
		return
	}
	if broken {
		t.Fatalf("seeded seal breaks numbering_floor >= baseline_used but the case is not listed fixture_invalid")
	}
	env := newEnv(t, c)
	status, code, miss := runHTTPVector(t, c, env)
	switch {
	case listed:
		if miss == "" {
			t.Fatalf("design_result entry is stale: the vector now matches the pack; remove it")
		}
		if status != div.Status || code != div.Code {
			t.Fatalf("design result: got %d %q, want %d %q (%s)", status, code, div.Status, div.Code, div.Cite)
		}
	case miss != "":
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

// TestO3PackDivergenceTable pins every table entry to a real pack case of an
// executable type.
func TestO3PackDivergenceTable(t *testing.T) {
	cases := map[string]*packCase{}
	for _, c := range loadPackCases(t) {
		cases[c.id()] = c
	}
	for id, d := range packDivergence {
		c, ok := cases[id]
		if !ok {
			t.Errorf("pack_divergence names unknown case %s", id)
			continue
		}
		if rt := c.reqType(); rt != "http" && rt != "http_raw" {
			t.Errorf("pack_divergence %s has type %s", id, rt)
		}
		if d.Category != divDesignResult && d.Category != divFixtureInvalid {
			t.Errorf("pack_divergence %s: unknown category %q", id, d.Category)
		}
		if d.Category == divDesignResult {
			if st, ok := wrk.Status(wrk.Code(d.Code)); !ok || st != d.Status {
				t.Errorf("pack_divergence %s: %d %q is not a §4 status/code pair", id, d.Status, d.Code)
			}
		}
	}
	for id := range deskOwned {
		if _, ok := cases[id]; !ok {
			t.Errorf("deskOwned names unknown case %s", id)
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

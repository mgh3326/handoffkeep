package o3contract

// Guards over the O3 vector pack for the class of defects fixed in hk
// 2049 (M1r).  Each guard is a pure function over loaded cases so the
// mutants below exercise the real call site, not a copy of the rule.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type namedCase struct {
	file string
	c    *vectorCase
}

func guardPackDir() string {
	if d := os.Getenv("O3CONTRACT_PACK"); d != "" {
		return d
	}
	return packDir
}

func loadAllCases(t *testing.T) []namedCase {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(guardPackDir(), "vectors", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var out []namedCase
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var vf vectorFile
		if err := json.Unmarshal(b, &vf); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for i := range vf.Cases {
			out = append(out, namedCase{filepath.Base(path), &vf.Cases[i]})
		}
	}
	return out
}

func (n namedCase) label() string { return n.file + " case " + n.c.Name }

// canonicalJSON is RFC8785 for the value shapes in this pack: ASCII
// object keys, strings, integers, booleans, null, arrays and objects.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func digest(v any) (string, error) {
	b, err := canonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// requestDigest is the design's L4048 rule.
func requestDigest(job string, body any) (string, error) {
	return digest(map[string]any{
		"method": "POST",
		"path":   "/v1/wrk/jobs/" + job + "/records",
		"body":   body,
	})
}

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), x == float64(int64(x))
	case int64:
		return x, true
	case int:
		return int64(x), true
	}
	return 0, false
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asString(v any) string      { s, _ := v.(string); return s }

func requestJob(c *vectorCase) string {
	p := asString(c.Request["path"])
	parts := strings.Split(p, "/")
	if len(parts) >= 5 && parts[1] == "v1" && parts[2] == "wrk" &&
		parts[3] == "jobs" {
		return parts[4]
	}
	return ""
}

func isPOST(c *vectorCase) bool {
	t := asString(c.Request["type"])
	return (t == "http" || t == "http_raw") &&
		asString(c.Request["method"]) == "POST"
}

func postBody(c *vectorCase) map[string]any {
	return asMap(c.Request["body"])
}

func expectError(c *vectorCase) string { return asString(c.Expect["error"]) }

func jobTail(recs []map[string]any, job string) (tail, claim int64) {
	for _, r := range recs {
		if asString(r["job_id"]) != job {
			continue
		}
		if s, ok := asInt(r["seq"]); ok && s > tail {
			tail = s
		}
		if asString(r["kind"]) == "claim" {
			if s, ok := asInt(r["seq"]); ok && s > claim {
				claim = s
			}
		}
	}
	return
}

func claimHost(recs []map[string]any, job string) string {
	host := ""
	for _, r := range recs {
		if asString(r["job_id"]) == job && asString(r["kind"]) == "claim" {
			host = asString(asMap(r["payload"])["host_client_id"])
		}
	}
	return host
}

// ---------------------------------------------------------------- guards

// guardCAS: every POST's expected_seq and claim_seq equal the target
// job's tail and claim.  An exact successful replay and an
// idempotency_conflict legitimately carry an older expected_seq: L125
// checks replay before CAS.
func guardCAS(cases []namedCase) []string {
	var out []string
	for _, n := range cases {
		if !isPOST(n.c) {
			continue
		}
		body := postBody(n.c)
		if body == nil {
			continue
		}
		if _, ok := body["expected_seq"]; !ok {
			continue
		}
		switch expectError(n.c) {
		case "sequence_conflict", "idempotency_conflict":
			continue
		}
		job := requestJob(n.c)
		expSeq, _ := asInt(body["expected_seq"])
		claimSeq, _ := asInt(body["claim_seq"])
		tail, claim := jobTail(n.c.Given.Records, job)
		// Exact successful replay: same actor/job/key and same digest.
		replay := false
		if d, err := requestDigest(job, body); err == nil {
			for _, r := range n.c.Given.Records {
				if asString(r["job_id"]) == job &&
					asString(r["actor_client_id"]) ==
						asString(n.c.Request["actor"]) &&
					asString(r["idempotency_key"]) ==
						asString(n.c.Request["idempotency_key"]) &&
					asString(r["request_sha256"]) == d {
					replay = true
				}
			}
		}
		if replay {
			continue
		}
		if expSeq != tail {
			out = append(out, fmt.Sprintf("%s: expected_seq=%d, job %q tail=%d",
				n.label(), expSeq, job, tail))
		}
		if claimSeq != claim {
			out = append(out, fmt.Sprintf("%s: claim_seq=%d, job %q claim=%d",
				n.label(), claimSeq, job, claim))
		}
	}
	return out
}

// guardReviewer: every reviewer named by a given round_open, verdict or
// grant has a claim earlier in array order and received_at, and is not
// ended before that record.  A request naming a reviewer must have the
// claim in the given (a request after an end is a deliberate
// round_not_open case).
func guardReviewer(cases []namedCase) []string {
	var out []string
	for _, n := range cases {
		recs := n.c.Given.Records
		for i, r := range recs {
			kind := asString(r["kind"])
			if kind != "round_open" && kind != "verdict" && kind != "grant" {
				continue
			}
			tester := asString(asMap(r["payload"])["tester_job_id"])
			if tester == "" {
				continue
			}
			ts := asString(r["received_at"])
			earlier := false
			for j, c := range recs {
				if j >= i || asString(c["kind"]) != "claim" ||
					asString(c["job_id"]) != tester {
					continue
				}
				if asString(c["received_at"]) <= ts {
					earlier = true
				}
			}
			if !earlier {
				out = append(out, fmt.Sprintf(
					"%s: %s@%v names reviewer %q with no earlier claim "+
						"(array+received_at)", n.label(), kind, r["seq"],
					tester))
			}
			for j, e := range recs {
				if asString(e["kind"]) != "end" ||
					asString(e["job_id"]) != tester {
					continue
				}
				if j < i || asString(e["received_at"]) <= ts {
					out = append(out, fmt.Sprintf(
						"%s: %s@%v names reviewer %q after its end",
						n.label(), kind, r["seq"], tester))
				}
			}
		}
		if !isPOST(n.c) {
			continue
		}
		kind := asString(postBody(n.c)["kind"])
		if kind != "round_open" && kind != "verdict" {
			continue
		}
		tester := asString(asMap(postBody(n.c)["payload"])["tester_job_id"])
		if tester == "" {
			continue
		}
		found := false
		for _, c := range recs {
			if asString(c["kind"]) == "claim" &&
				asString(c["job_id"]) == tester {
				found = true
			}
		}
		if !found {
			out = append(out, fmt.Sprintf(
				"%s: request %s names reviewer %q with no claim in given",
				n.label(), kind, tester))
		}
	}
	return out
}

// guardSealFloor: numbering_floor = max(baseline_used, ...) per L4028.
func guardSealFloor(cases []namedCase) []string {
	var out []string
	for _, n := range cases {
		for _, r := range n.c.Given.Records {
			if asString(r["kind"]) != "import_seal" {
				continue
			}
			pl := asMap(r["payload"])
			floor, _ := asInt(pl["numbering_floor"])
			base, _ := asInt(pl["baseline_used"])
			if floor < base {
				out = append(out, fmt.Sprintf(
					"%s: seal@%v job %q numbering_floor=%d < "+
						"baseline_used=%d", n.label(), r["seq"],
					asString(r["job_id"]), floor, base))
			}
		}
	}
	return out
}

// inputView reconstructs the Input_* body from a persisted payload for
// kinds whose body carries no content bytes.
func inputView(kind string, pl map[string]any) map[string]any {
	keep := map[string][]string{
		"claim": {"job_id", "owner_lane", "parent_lane", "task_id",
			"host_client_id", "role", "tier", "review_of",
			"predecessor_job_id", "supersedes"},
		"activate":     {"reason_code", "seal_seq"},
		"round_open":   {"head", "tester_job_id", "extension_grant_seq", "repeat_grant_seq"},
		"void":         {"round_no", "grant_seq", "reason_code"},
		"grant_cancel": {"grant_seq", "reason_code"},
		"end":          {"end_type", "reason_code"},
		"import_begin": {"reason_code"},
		"import_seal": {"begin_seq", "expected_import_count",
			"manifest_sha256", "reason_code"},
		"grant": {"grant_type", "round_no", "head", "extra",
			"earlier_round_no", "tester_job_id", "expires_at",
			"reason_code"},
	}
	keys, ok := keep[kind]
	if !ok {
		return nil
	}
	m := map[string]any{}
	for _, k := range keys {
		if v, ok := pl[k]; ok {
			m[k] = v
		}
	}
	return m
}

// guardDigest: every seeded request_sha256 equals the L4048 digest of
// its Input_* body; a POST that reuses a given record's key must match
// it unless the case is the changed-body idempotency_conflict.
//
// Content-bearing kinds (verdict/completed/joined/import) hash a
// content_b64 that the persisted payload does not carry, so their
// seeded digests are covered only through a replayed request with the
// same key; the other kinds are checked exhaustively.
func guardDigest(cases []namedCase) []string {
	var out []string
	for _, n := range cases {
		for _, r := range n.c.Given.Records {
			kind := asString(r["kind"])
			if imported, _ := r["imported"].(bool); imported {
				continue
			}
			pl := inputView(kind, asMap(r["payload"]))
			if pl == nil {
				continue
			}
			job := asString(r["job_id"])
			seq, _ := asInt(r["seq"])
			claim, _ := asInt(r["claim_seq"])
			if kind == "claim" {
				claim = 0
			}
			d, err := requestDigest(job, map[string]any{
				"kind": kind, "claim_seq": claim,
				"expected_seq": seq - 1, "payload": pl})
			if err != nil {
				out = append(out, fmt.Sprintf("%s: digest: %v",
					n.label(), err))
				continue
			}
			if d != asString(r["request_sha256"]) {
				out = append(out, fmt.Sprintf(
					"%s: %s@%v job %q request_sha256 is not the "+
						"L4048 digest of its Input_%s body",
					n.label(), kind, seq, job, kind))
			}
		}
		if !isPOST(n.c) {
			continue
		}
		job := requestJob(n.c)
		d, err := requestDigest(job, postBody(n.c))
		if err != nil {
			continue
		}
		for _, r := range n.c.Given.Records {
			if asString(r["job_id"]) != job ||
				asString(r["actor_client_id"]) !=
					asString(n.c.Request["actor"]) ||
				asString(r["idempotency_key"]) !=
					asString(n.c.Request["idempotency_key"]) {
				continue
			}
			if expectError(n.c) == "idempotency_conflict" {
				if d == asString(r["request_sha256"]) {
					out = append(out, fmt.Sprintf(
						"%s: idempotency_conflict but the seeded "+
							"digest equals the changed request",
						n.label()))
				}
				continue
			}
			if d != asString(r["request_sha256"]) {
				out = append(out, fmt.Sprintf(
					"%s: replayed request digest does not match "+
						"seeded record %v", n.label(), r["seq"]))
			}
		}
	}
	return out
}

var central = map[string]bool{"director-1": true, "operator-desk": true}

func isReviewRole(role string) bool {
	return role == "tester" || role == "reviewer" || role == "counsel"
}

// guardPermission: the request actor holds the section 3 matrix
// permission for its kind.  scope_denied/host_mismatch cases assert the
// denial instead.
func guardPermission(cases []namedCase) []string {
	var out []string
	for _, n := range cases {
		if !isPOST(n.c) {
			continue
		}
		body := postBody(n.c)
		kind := asString(body["kind"])
		actor := asString(n.c.Request["actor"])
		if kind == "" {
			continue
		}
		if e := expectError(n.c); e == "scope_denied" || e == "host_mismatch" {
			continue
		}
		job := requestJob(n.c)
		recs := n.c.Given.Records
		pl := asMap(body["payload"])
		ok := true
		switch kind {
		case "activate", "grant", "grant_cancel", "import_begin",
			"import_seal", "import":
			ok = central[actor]
		case "round_open", "void", "joined", "completed":
			ok = actor == claimHost(recs, job)
		case "end":
			ok = actor == claimHost(recs, job) || central[actor]
		case "claim":
			if central[actor] {
				break
			}
			ok = isReviewRole(asString(pl["role"]))
		case "verdict":
			if actor == claimHost(recs, job) {
				break
			}
			round, _ := asInt(pl["round_no"])
			ok = false
			for _, r := range recs {
				if asString(r["kind"]) != "claim" {
					continue
				}
				cp := asMap(r["payload"])
				ro := asMap(cp["review_of"])
				if ro == nil || asString(ro["job_id"]) != job {
					continue
				}
				if rn, _ := asInt(ro["round_no"]); rn != round {
					continue
				}
				if asString(cp["host_client_id"]) == actor &&
					isReviewRole(asString(cp["role"])) {
					ok = true
				}
			}
		}
		if !ok {
			out = append(out, fmt.Sprintf(
				"%s: actor %q lacks the section 3 permission for %s "+
					"on job %q", n.label(), actor, kind, job))
		}
	}
	return out
}

// guardGrantPointers: every grant pointer names a grant of the right
// type for that job.
func guardGrantPointers(cases []namedCase) []string {
	want := map[string]string{
		"extension_grant_seq": "extension",
		"repeat_grant_seq":    "head_repeat",
		"waiver_grant_seq":    "join_waiver",
		"grant_seq":           "",
	}
	var out []string
	check := func(label, job, field string, v any, kind string,
		recs []map[string]any) {
		seq, ok := asInt(v)
		if !ok {
			return
		}
		gtype := ""
		for _, r := range recs {
			if asString(r["job_id"]) == job && asString(r["kind"]) == "grant" {
				if s, _ := asInt(r["seq"]); s == seq {
					gtype = asString(asMap(r["payload"])["grant_type"])
				}
			}
		}
		if gtype == "" {
			out = append(out, fmt.Sprintf(
				"%s: %s names seq %d with no grant record for job %q",
				label, field, seq, job))
			return
		}
		// grant_cancel may cancel any grant type.
		if kind == "grant_cancel" {
			return
		}
		if w := want[field]; w != "" && gtype != w {
			out = append(out, fmt.Sprintf(
				"%s: %s names a %s grant for job %q, want %s",
				label, field, gtype, job, w))
		}
	}
	// Only fields that the kind's Input shape defines are grant pointers.
	fieldsFor := map[string][]string{
		"round_open":   {"extension_grant_seq", "repeat_grant_seq"},
		"void":         {"grant_seq"},
		"grant_cancel": {"grant_seq"},
		"joined":       {"waiver_grant_seq"},
	}
	for _, n := range cases {
		if e := expectError(n.c); e == "grant_invalid" || e == "scope_denied" {
			continue
		}
		for _, r := range n.c.Given.Records {
			kind := asString(r["kind"])
			pl := asMap(r["payload"])
			for _, field := range fieldsFor[kind] {
				if v, ok := pl[field]; ok {
					check(n.label(), asString(r["job_id"]), field, v,
						kind, n.c.Given.Records)
				}
			}
		}
		if isPOST(n.c) {
			kind := asString(postBody(n.c)["kind"])
			pl := asMap(postBody(n.c)["payload"])
			for _, field := range fieldsFor[kind] {
				if v, ok := pl[field]; ok {
					check(n.label(), requestJob(n.c), field, v,
						kind, n.c.Given.Records)
				}
			}
		}
	}
	return out
}

// guardJobRefs: every job id a request path, a review_of, a grant pointer
// or a seeded record names exists in that case's given records.  A claim
// request creates its own job, and a case expecting not_found is the
// absence case.  A legacy task row's refs.job_id names the canonical
// subject job that protects it, so it is a seeded-record reference too.
func guardJobRefs(cases []namedCase) []string {
	var out []string
	for _, n := range cases {
		if expectError(n.c) == "not_found" {
			continue
		}
		jobs := map[string]bool{}
		for _, r := range n.c.Given.Records {
			if j := asString(r["job_id"]); j != "" {
				jobs[j] = true
			}
		}
		flag := func(where, job string) {
			out = append(out, fmt.Sprintf(
				"%s: %s names job %q with no record in given",
				n.label(), where, job))
		}
		job := requestJob(n.c)
		kind := asString(postBody(n.c)["kind"])
		creates := isPOST(n.c) && kind == "claim"
		if job != "" && !jobs[job] && !creates {
			flag("request path", job)
		}
		for _, r := range n.c.Given.Records {
			if asString(r["kind"]) != "claim" {
				continue
			}
			ro := asMap(asMap(r["payload"])["review_of"])
			if ro == nil {
				continue
			}
			if t := asString(ro["job_id"]); t != "" && !jobs[t] {
				flag("review_of of claim "+asString(r["job_id"]), t)
			}
		}
		if creates {
			ro := asMap(asMap(postBody(n.c)["payload"])["review_of"])
			// A self-reference is claim_conflict before target lookup.
			if t := asString(ro["job_id"]); t != "" && t != job &&
				!jobs[t] {
				flag("request review_of", t)
			}
		}
		for _, r := range n.c.Given.Records {
			if asString(r["kind"]) != "grant" {
				continue
			}
			t := asString(asMap(r["payload"])["tester_job_id"])
			if t != "" && !jobs[t] {
				flag("grant tester_job_id", t)
			}
		}
		for _, r := range n.c.Given.Legacy.Tasks {
			refs := asMap(r["refs"])
			if t := asString(refs["job_id"]); t != "" && !jobs[t] {
				flag(fmt.Sprintf("legacy task %v refs.job_id", r["id"]), t)
			}
		}
	}
	return out
}

// guardClaimOrder: a claim naming review_of may not precede the claim of
// the job it reviews.  A reviewed claim that is wholly absent is
// guardJobRefs' finding, not an ordering one.
func guardClaimOrder(cases []namedCase) []string {
	var out []string
	for _, n := range cases {
		seen := map[string]bool{}
		for i, r := range n.c.Given.Records {
			if asString(r["kind"]) != "claim" {
				continue
			}
			job := asString(r["job_id"])
			ro := asMap(asMap(r["payload"])["review_of"])
			if t := asString(ro["job_id"]); t != "" && !seen[t] {
				later := false
				for _, c := range n.c.Given.Records[i+1:] {
					if asString(c["kind"]) == "claim" &&
						asString(c["job_id"]) == t {
						later = true
					}
				}
				if later {
					out = append(out, fmt.Sprintf(
						"%s: claim %q (record %d) reviews %q "+
							"before its claim", n.label(), job, i, t))
				}
			}
			seen[job] = true
		}
	}
	return out
}

func laneNames(lanes map[string]string) []string {
	names := make([]string, 0, len(lanes))
	for n := range lanes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func laneCycle(lanes map[string]string) string {
	for _, start := range laneNames(lanes) {
		seen := map[string]bool{}
		for n := start; n != ""; n = lanes[n] {
			if seen[n] {
				return n
			}
			seen[n] = true
			if _, ok := lanes[n]; !ok {
				break
			}
		}
	}
	return ""
}

// guardLaneGraph: every owner and parent lane in a seeded claim resolves
// in the case's lane graph — policy lanes plus the lanes canonical
// builder claims introduce when claim_lane_creation_enabled is true
// (design L4018).  Review claims are routing data, not graph edges, but
// their owner/parent pair must still resolve.  Missing parents,
// conflicting parent edges and cycles are failures.
func guardLaneGraph(cases []namedCase) []string {
	var out []string
	for _, n := range cases {
		var pol map[string]any
		if len(n.c.Given.Policy) > 0 {
			if err := json.Unmarshal(n.c.Given.Policy, &pol); err != nil {
				out = append(out, fmt.Sprintf("%s: given.policy: %v",
					n.label(), err))
				continue
			}
		}
		lanes := map[string]string{}
		for name, parent := range asMap(pol["lanes"]) {
			lanes[name] = asString(parent)
		}
		flag := func(format string, args ...any) {
			out = append(out, n.label()+": "+fmt.Sprintf(format, args...))
		}
		if _, ok := lanes["director-1"]; !ok {
			flag("lane graph has no director-1 root")
		}
		for _, name := range laneNames(lanes) {
			parent := lanes[name]
			if parent == "" {
				continue
			}
			if _, ok := lanes[parent]; !ok {
				flag("policy lane %q has missing parent %q", name, parent)
			}
		}
		if c := laneCycle(lanes); c != "" {
			flag("policy lane graph has a cycle through %q", c)
		}
		feature := false
		if f := asMap(pol["features"]); f != nil {
			feature, _ = f["claim_lane_creation_enabled"].(bool)
		}
		for _, r := range n.c.Given.Records {
			if asString(r["kind"]) != "claim" {
				continue
			}
			pl := asMap(r["payload"])
			job := asString(r["job_id"])
			owner := asString(pl["owner_lane"])
			parent := asString(pl["parent_lane"])
			role := asString(pl["role"])
			actor := asString(r["actor_client_id"])
			switch {
			case isReviewRole(role):
				if owner != parent {
					flag("review claim %q owner %q != parent %q",
						job, owner, parent)
				}
				if _, ok := lanes[owner]; !ok {
					flag("review claim %q lane %q is neither a "+
						"configured lane nor a canonical builder lane",
						job, owner)
				}
				if _, ok := lanes[parent]; !ok {
					flag("review claim %q parent %q is neither a "+
						"configured lane nor a canonical builder lane",
						job, parent)
				}
			case role == "builder" && central[actor]:
				if have, ok := lanes[owner]; ok {
					if have != parent {
						flag("builder claim %q lane %q already has "+
							"parent %q, claim says %q",
							job, owner, have, parent)
					}
				} else if !feature {
					flag("builder claim %q introduces lane %q with "+
						"claim_lane_creation_enabled=false", job, owner)
				} else if _, ok := lanes[parent]; !ok {
					flag("builder claim %q lane %q has missing "+
						"parent %q", job, owner, parent)
				} else {
					lanes[owner] = parent
				}
			default:
				if have, ok := lanes[owner]; !ok {
					flag("%s claim %q lane %q is neither a "+
						"configured lane nor a canonical builder lane",
						role, job, owner)
				} else if have != parent {
					flag("%s claim %q lane %q has parent %q, claim "+
						"says %q", role, job, owner, have, parent)
				}
			}
		}
	}
	return out
}

// legacyIDs returns the legacy task and disposition ids a legacy request
// path or body targets.
func legacyIDs(c *vectorCase) (tasks, disps []int64) {
	parts := strings.Split(asString(c.Request["path"]), "/")
	switch {
	case len(parts) >= 6 && parts[1] == "v1" && parts[2] == "tasks" &&
		parts[3] == "dispositions":
		if id, err := strconv.ParseInt(parts[4], 10, 64); err == nil {
			disps = append(disps, id)
		}
	case len(parts) >= 5 && parts[1] == "v1" && parts[2] == "tasks":
		if id, err := strconv.ParseInt(parts[3], 10, 64); err == nil {
			tasks = append(tasks, id)
		}
	}
	if len(parts) >= 3 && parts[1] == "v1" &&
		(parts[2] == "decisions" || parts[2] == "assistant") {
		if id, ok := asInt(asMap(c.Request["body"])["task_id"]); ok {
			tasks = append(tasks, id)
		}
	}
	return
}

// guardLegacy: every legacy task or disposition id a legacy request
// targets is seeded in given.legacy, and a seeded disposition's task_id
// is seeded too.  A case expecting not_found is the absence case.
func guardLegacy(cases []namedCase) []string {
	var out []string
	for _, n := range cases {
		if asString(n.c.Request["type"]) != "legacy" ||
			expectError(n.c) == "not_found" {
			continue
		}
		tasks, disps := legacyIDs(n.c)
		seededT := map[int64]bool{}
		seededD := map[int64]bool{}
		for _, r := range n.c.Given.Legacy.Tasks {
			if id, ok := asInt(r["id"]); ok {
				seededT[id] = true
			}
		}
		for _, r := range n.c.Given.Legacy.Dispositions {
			if id, ok := asInt(r["id"]); ok {
				seededD[id] = true
			}
		}
		for _, id := range tasks {
			if !seededT[id] {
				out = append(out, fmt.Sprintf(
					"%s: legacy request targets task %d, not in "+
						"given.legacy.tasks", n.label(), id))
			}
		}
		for _, id := range disps {
			if !seededD[id] {
				out = append(out, fmt.Sprintf(
					"%s: legacy request targets disposition %d, not "+
						"in given.legacy.dispositions", n.label(), id))
			}
		}
		for _, r := range n.c.Given.Legacy.Dispositions {
			if tid, ok := asInt(r["task_id"]); ok && !seededT[tid] {
				out = append(out, fmt.Sprintf(
					"%s: seeded disposition %v targets task %d, not "+
						"in given.legacy.tasks", n.label(), r["id"], tid))
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- tests

type guardFn struct {
	name string
	fn   func([]namedCase) []string
}

var guards = []guardFn{
	{"cas", guardCAS},
	{"reviewer", guardReviewer},
	{"seal_floor", guardSealFloor},
	{"digest", guardDigest},
	{"permission", guardPermission},
	{"grant_pointer", guardGrantPointers},
	{"job_ref", guardJobRefs},
	{"claim_order", guardClaimOrder},
	{"lane_graph", guardLaneGraph},
	{"legacy", guardLegacy},
}

func TestPackGuards(t *testing.T) {
	cases := loadAllCases(t)
	for _, g := range guards {
		for _, v := range g.fn(cases) {
			t.Errorf("%s guard: %s", g.name, v)
		}
	}
}

func findCase(t *testing.T, name string) *vectorCase {
	t.Helper()
	for _, n := range loadAllCases(t) {
		if n.c.Name == name {
			return n.c
		}
	}
	t.Fatalf("case %q not in pack", name)
	return nil
}

func mutateCase(t *testing.T, name string, fn func(*vectorCase)) []namedCase {
	t.Helper()
	cases := loadAllCases(t)
	found := false
	for _, n := range cases {
		if n.c.Name == name {
			fn(n.c)
			found = true
		}
	}
	if !found {
		t.Fatalf("case %q not in pack", name)
	}
	return cases
}

func requireRed(t *testing.T, guard func([]namedCase) []string,
	cases []namedCase, what string) {
	t.Helper()
	if got := guard(cases); len(got) == 0 {
		t.Fatalf("%s mutant not caught by the guard", what)
	}
}

// Each mutant is applied to a real pack case and run through the real
// guard, so a guard cannot pass by checking a private copy of the rule.
func TestGuardMutants(t *testing.T) {
	cases := loadAllCases(t)
	for _, g := range guards {
		if got := g.fn(cases); len(got) != 0 {
			t.Fatalf("%s guard is not green before mutation: %v",
				g.name, got)
		}
	}

	requireRed(t, guardCAS, mutateCase(t,
		"grant-consumer-race-void-reuse", func(c *vectorCase) {
			asMap(c.Request["body"])["expected_seq"] = float64(6)
		}), "stale expected_seq")

	requireRed(t, guardReviewer, mutateCase(t,
		"join-after-tester-reaped-201", func(c *vectorCase) {
			for _, r := range c.Given.Records {
				if asString(r["kind"]) == "end" {
					r["received_at"] = "2026-10-24T00:00:00.000000Z"
				}
			}
		}), "reviewer end before the record")

	requireRed(t, guardSealFloor, mutateCase(t,
		"planted-legacy-events-import-untrusted", func(c *vectorCase) {
			for _, r := range c.Given.Records {
				if asString(r["kind"]) == "import_seal" {
					asMap(r["payload"])["numbering_floor"] = float64(1)
				}
			}
		}), "seal floor below baseline")

	requireRed(t, guardDigest, mutateCase(t, "verdict-pass-h-binds-round-1",
		func(c *vectorCase) {
			for _, r := range c.Given.Records {
				if asString(r["kind"]) == "round_open" {
					r["request_sha256"] = strings.Repeat("0", 64)
				}
			}
		}), "round_open digest mismatch")

	requireRed(t, guardPermission, mutateCase(t,
		"imported-activated-join-zero-rounds", func(c *vectorCase) {
			c.Request["actor"] = "director-1"
		}), "central actor on joined")

	requireRed(t, guardGrantPointers, mutateCase(t,
		"two-testers-one-head-hk1901", func(c *vectorCase) {
			asMap(asMap(c.Request["body"])["payload"])["repeat_grant_seq"] =
				float64(4)
		}), "repeat grant pointing at the verdict")

	requireRed(t, guardJobRefs, mutateCase(t,
		"r-cleanup-after-subject-join-still-receipt-required",
		func(c *vectorCase) {
			for _, r := range c.Given.Records {
				if asString(r["kind"]) != "claim" {
					continue
				}
				if ro := asMap(asMap(r["payload"])["review_of"]); ro != nil {
					ro["job_id"] = "job-missing"
				}
			}
		}), "review_of naming an absent job")

	requireRed(t, guardClaimOrder, mutateCase(t,
		"scoped-mint-reviewer-as-target-claim-conflict",
		func(c *vectorCase) {
			recs := c.Given.Records
			for i, r := range recs {
				if asString(r["kind"]) == "claim" &&
					asMap(asMap(r["payload"])["review_of"]) != nil {
					reordered := []map[string]any{r}
					reordered = append(reordered, recs[:i]...)
					c.Given.Records = append(reordered, recs[i+1:]...)
					break
				}
			}
		}), "review claim before its subject claim")

	requireRed(t, guardLaneGraph, mutateCase(t,
		"r-import-begin-scope-denied", func(c *vectorCase) {
			for _, r := range c.Given.Records {
				if asString(r["kind"]) == "claim" &&
					asString(r["job_id"]) == "job-a" {
					asMap(r["payload"])["parent_lane"] = "b2400-missing"
				}
			}
		}), "builder lane with a missing parent")

	requireRed(t, guardLegacy, mutateCase(t,
		"tasks-claim-protected-denied", func(c *vectorCase) {
			c.Given.Legacy = legacyGiven{}
		}), "legacy task row removed")
}

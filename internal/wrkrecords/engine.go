package wrkrecords

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"github.com/mgh3326/handoffkeep/internal/guard"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Engine evaluates one append on a locked, committed snapshot. It does not
// acknowledge or publish anything; Store owns commit and terminal projection.
type Engine struct {
	Policy     *Policy
	Mode       string
	Recovery   bool
	Now        time.Time
	Generation string
}

func NewUUID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("random source unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func (e Engine) Check(actor, permission string) error {
	if e.Policy == nil || e.Mode == "" || e.Mode == "off" {
		return Error("wrk_unavailable")
	}
	if e.Recovery && RecoveryReadsCentralOnly && !(Central(actor) && strings.HasSuffix(permission, ".read")) {
		return Error("wrk_recovery_required")
	}
	if !e.Policy.Allows(actor, permission) {
		return Error("scope_denied")
	}
	return nil
}
func (e Engine) time() time.Time {
	if e.Now.IsZero() {
		return time.Now().UTC().Truncate(time.Microsecond)
	}
	return e.Now.UTC().Truncate(time.Microsecond)
}

// Time is the server clock used by both writes and read projections.
func (e Engine) Time() time.Time { return e.time() }

var permissions = map[string]string{"activate": "activate.write", "round_open": "round.open", "verdict": "verdict.write", "void": "void.write", "joined": "joined.write", "completed": "completed.write", "grant": "grant.write", "grant_cancel": "grant.cancel", "end": "end.write", "import_begin": "import.begin", "import_seal": "import.seal", "import": "import.write"}

func (e Engine) Append(s Snapshot, actor, job, key string, r Request) (WriteResponse, []byte, error) {
	fail := func(code string) (WriteResponse, []byte, error) { return WriteResponse{}, nil, Error(code) }
	if !JobRE.MatchString(job) || !UUIDRE.MatchString(key) {
		return fail("invalid_request")
	}
	if err := ValidateRequest(r); err != nil {
		return WriteResponse{}, nil, err
	}
	permission := permissions[r.Kind]
	if r.Kind == "claim" {
		permission = "claim.write"
		if role := Text(r.Payload, "role"); role == "tester" || role == "reviewer" || role == "counsel" {
			permission = "reviewer.claim.write"
		}
	}
	if err := e.Check(actor, permission); err != nil {
		return WriteResponse{}, nil, err
	}
	if e.Recovery {
		return fail("wrk_recovery_required")
	}
	digest, err := Digest(Object{"method": "POST", "path": "/v1/wrk/jobs/" + job + "/records", "body": r})
	if err != nil {
		return WriteResponse{}, nil, err
	}
	rs := s.Jobs[job]
	// Authentication and static permission precede replay; mutable activeness,
	// CAS, terminal state, expiry and accounting MUST remain after this branch.
	for i, old := range rs {
		if old.Actor == actor && old.Key == key {
			if old.RequestSHA != digest {
				return fail("idempotency_conflict")
			}
			saved := s
			saved.Jobs = copyJobs(s.Jobs)
			saved.Jobs[job] = rs[:i+1]
			summary, x := saved.Summary(job, e.time())
			if x != nil {
				return WriteResponse{}, nil, x
			}
			return WriteResponse{Contract, false, old, summary}, nil, nil
		}
	}
	claim := s.Claim(job)
	tail := int64(0)
	if len(rs) > 0 {
		tail = rs[len(rs)-1].Seq
	}
	claimSeq := int64(0)
	if claim != nil {
		claimSeq = claim.Seq
	}
	if r.ExpectedSeq != tail || r.ClaimSeq != claimSeq {
		return fail("sequence_conflict")
	}
	if r.Kind != "claim" && claim == nil {
		return fail("not_found")
	}
	sum := Summary{State: "shadow", Next: 1, Open: []int64{}, Warnings: []string{}}
	if claim != nil {
		sum, err = s.Summary(job, e.time())
		if err != nil {
			return WriteResponse{}, nil, err
		}
	}
	if sum.State == "ended" {
		return fail("job_ended")
	}
	if sum.State == "joined" && r.Kind != "end" {
		return fail("review_sealed")
	}
	if Review(claim) {
		switch r.Kind {
		case "activate", "round_open", "void", "joined", "import_begin", "import", "import_seal", "grant", "grant_cancel":
			return fail("scope_denied")
		}
	}
	if r.Kind != "claim" && !Central(actor) && e.Policy.Host(actor) != Text(claim.Payload, "host_client_id") {
		if r.Kind != "verdict" || Text(r.Payload, "tester_job_id") == "" {
			return fail("host_mismatch")
		}
		reviewer := s.Claim(Text(r.Payload, "tester_job_id"))
		if reviewer == nil || Text(reviewer.Payload, "host_client_id") != e.Policy.Host(actor) {
			return fail("host_mismatch")
		}
	}
	if sum.State == "importing" && r.Kind != "import" && r.Kind != "import_seal" && r.Kind != "end" {
		return fail("migration_frozen")
	}
	if sum.State == "sealed" && r.Kind != "activate" && r.Kind != "end" {
		return fail("import_sealed")
	}
	p := Clone(r.Payload)
	var content []byte
	switch r.Kind {
	case "claim":
		if err = e.claim(s, actor, job, claim, sum, p); err != nil {
			return WriteResponse{}, nil, err
		}
		p["policy_sha256"] = e.Policy.Digest()
		p["created_at"] = e.time().Format("2006-01-02T15:04:05.000000Z")
	case "activate":
		if e.Mode != "enforce" {
			return fail("wrk_unavailable")
		}
		if s.Activation(job) > 0 {
			return fail("claim_conflict")
		}
		baseline, floor := int64(0), int64(0)
		var seal *Record
		work := false
		for _, old := range rs {
			switch old.Kind {
			case "round_open", "verdict", "void", "completed", "joined", "grant", "grant_cancel", "import":
				work = true
			case "import_seal":
				x := old
				seal = &x
			}
		}
		if work && (seal == nil || Num(p, "seal_seq") != seal.Seq) {
			return fail("import_unsealed")
		}
		if seal != nil {
			if Num(p, "seal_seq") != seal.Seq {
				return fail("import_unsealed")
			}
			baseline = Num(seal.Payload, "baseline_used")
			floor = Num(seal.Payload, "numbering_floor")
			if floor < baseline {
				return fail("import_unsealed")
			}
		} else if p["seal_seq"] != nil {
			return fail("import_unsealed")
		}
		if pred := Text(claim.Payload, "predecessor_job_id"); pred != "" {
			previous, x := s.Summary(pred, e.time())
			if x != nil {
				return WriteResponse{}, nil, x
			}
			if baseline < previous.Used {
				baseline = previous.Used
			}
		}
		p = Object{"baseline_used": baseline, "mode": "enforced", "seal_seq": p["seal_seq"], "reason_code": p["reason_code"]}
	case "round_open":
		for field, kind := range map[string]string{"extension_grant_seq": "extension", "repeat_grant_seq": "head_repeat"} {
			if p[field] != nil {
				if err = e.consume(s, job, sum, p, field, kind); err != nil {
					return WriteResponse{}, nil, err
				}
			}
		}
		if id := Text(p, "tester_job_id"); id != "" {
			if err = e.assignment(s, job, sum.Next, id, ""); err != nil {
				return WriteResponse{}, nil, err
			}
		}
		if e.headReserved(s, job, Text(p, "head")) && p["repeat_grant_seq"] == nil {
			return fail("duplicate_head")
		}
		if sum.Used < 3 && p["extension_grant_seq"] != nil {
			return fail("grant_before_cap")
		}
		if sum.Used >= 3 && p["extension_grant_seq"] == nil {
			return fail("round_cap")
		}
		if sum.Next >= MaxInt {
			return fail("invalid_request")
		}
		p["round_no"] = sum.Next
	case "verdict":
		n := Num(p, "round_no")
		o, v, x := s.Round(job, n)
		if o == nil {
			return fail("round_not_open")
		}
		if x != nil {
			return fail("round_voided")
		}
		id := Text(p, "tester_job_id")
		if Text(o.Payload, "tester_job_id") != "" && id != Text(o.Payload, "tester_job_id") {
			return fail("round_not_open")
		}
		if id != "" {
			if err = e.assignment(s, job, n, id, actor); err != nil {
				return WriteResponse{}, nil, err
			}
		}
		if !Central(actor) && e.Policy.Host(actor) != Text(claim.Payload, "host_client_id") && id == "" {
			return fail("host_mismatch")
		}
		content, err = Evidence(Text(p, "content_b64"), Text(p, "content_sha256"), false)
		if err != nil {
			return WriteResponse{}, nil, err
		}
		verdict, head, xerr := ParseVerdict(content)
		if xerr != nil {
			return WriteResponse{}, nil, xerr
		}
		if verdict != Text(p, "verdict") {
			return fail("invalid_request")
		}
		normalized, xerr := NormalizePath(Text(p, "path"))
		if xerr != nil {
			return WriteResponse{}, nil, xerr
		}
		p["path"] = normalized
		if guard.Reject(string(content)) != nil {
			return fail("invalid_request")
		}
		for _, old := range e.identities(s, job) {
			if Text(old, "path") == normalized {
				return fail("duplicate_path")
			}
		}
		for _, old := range e.identities(s, job) {
			if Text(old, "content_sha256") == Text(p, "content_sha256") {
				return fail("duplicate_content")
			}
		}
		if Text(p, "head") != head || Text(o.Payload, "head") != head {
			return fail("head_mismatch")
		}
		if v != nil {
			return fail("round_has_verdict")
		}
		delete(p, "content_b64")
		p["size_bytes"] = len(content)
	case "void":
		if err = e.consume(s, job, sum, p, "grant_seq", "void"); err != nil {
			return WriteResponse{}, nil, err
		}
		o, v, x := s.Round(job, Num(p, "round_no"))
		if o == nil {
			return fail("round_not_open")
		}
		if v != nil {
			return fail("round_has_verdict")
		}
		if x != nil {
			return fail("round_voided")
		}
	case "joined":
		if p["waiver_grant_seq"] != nil {
			if err = e.consume(s, job, sum, p, "waiver_grant_seq", "join_waiver"); err != nil {
				return WriteResponse{}, nil, err
			}
		}
		u, x := url.Parse(Text(p, "pr"))
		if x != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return fail("invalid_request")
		}
		content, err = Evidence(Text(p, "report_b64"), Text(p, "report_sha256"), true)
		if err != nil {
			return WriteResponse{}, nil, err
		}
		opens := s.opens(job)
		verdicts := []int64{}
		if len(opens) == 0 {
			if p["waiver_grant_seq"] == nil {
				return fail("zero_rounds")
			}
		} else {
			if p["waiver_grant_seq"] != nil || len(sum.Open) > 0 {
				return fail("review_incomplete")
			}
			sort.Slice(opens, func(i, j int) bool { return Num(opens[i].Payload, "round_no") < Num(opens[j].Payload, "round_no") })
			latestPass, latestBlocker := int64(0), int64(0)
			var pass *Record
			for _, open := range opens {
				_, v, _ := s.Round(job, Num(open.Payload, "round_no"))
				if v == nil || len(s.Contents[job][v.Seq]) == 0 {
					return fail("review_incomplete")
				}
				verdicts = append(verdicts, v.Seq)
				n := Num(v.Payload, "round_no")
				if Text(v.Payload, "verdict") == "BLOCKER" {
					latestBlocker = n
				}
				if Text(v.Payload, "head") == Text(p, "head") && Text(v.Payload, "verdict") == "PASS" {
					latestPass = n
					pass = v
				}
				if Text(v.Payload, "head") == Text(p, "head") && Text(v.Payload, "verdict") == "BLOCKER" {
					latestPass = 0
					pass = nil
				}
			}
			if latestBlocker > latestPass {
				return fail("blocker_unresolved")
			}
			if pass == nil {
				return fail("review_incomplete")
			}
			if (Text(claim.Payload, "tier") == "T2" || Text(claim.Payload, "tier") == "T3") && Text(pass.Payload, "tester_job_id") == "" {
				return fail("review_incomplete")
			}
		}
		delete(p, "report_b64")
		p["report_size_bytes"] = len(content)
		p["verdict_seqs"] = verdicts
	case "completed":
		content, err = Evidence(Text(p, "report_b64"), Text(p, "report_sha256"), true)
		if err != nil {
			return WriteResponse{}, nil, err
		}
		delete(p, "report_b64")
		p["report_size_bytes"] = len(content)
		warnings := []string{}
		if len(sum.Open) > 0 {
			warnings = append(warnings, "review_incomplete")
		}
		p["warnings"] = warnings
	case "grant":
		if err = e.grant(s, job, sum, p); err != nil {
			return WriteResponse{}, nil, err
		}
	case "grant_cancel":
		if err = e.consume(s, job, sum, p, "grant_seq", ""); err != nil {
			return WriteResponse{}, nil, err
		}
	case "end":
	case "import_begin":
		if s.Activation(job) > 0 {
			return fail("claim_conflict")
		}
		for _, old := range rs {
			if old.Kind == "import_begin" {
				return fail("migration_frozen")
			}
		}
		p["shadow_cutoff_seq"] = tail
	case "import":
		if sum.State != "importing" {
			return fail("import_unsealed")
		}
		content, err = Evidence(Text(p, "content_b64"), Text(p, "source_sha256"), true)
		if err != nil {
			return WriteResponse{}, nil, err
		}
		delete(p, "content_b64")
	case "import_seal":
		if sum.State != "importing" {
			return fail("import_unsealed")
		}
		var begin *Record
		manifest := []Object{}
		for _, old := range rs {
			if old.Kind == "import_begin" {
				x := old
				begin = &x
			}
			if old.Kind == "import" {
				manifest = append(manifest, Object{"seq": old.Seq, "source_locator": old.Payload["source_locator"], "source_sha256": old.Payload["source_sha256"]})
			}
		}
		d, x := Digest(manifest)
		if x != nil || begin == nil || Num(p, "begin_seq") != begin.Seq || Num(p, "expected_import_count") != int64(len(manifest)) || Text(p, "manifest_sha256") != d {
			return fail("import_unsealed")
		}
		baseline, floor, x := e.accountImport(s, job)
		if x != nil {
			return WriteResponse{}, nil, x
		}
		p["baseline_used"] = baseline
		p["numbering_floor"] = floor
		p["shadow_cutoff_seq"] = begin.Payload["shadow_cutoff_seq"]
	default:
		return fail("invalid_request")
	}
	if tail >= MaxInt {
		return fail("invalid_request")
	}
	previous := strings.Repeat("0", 64)
	if tail > 0 {
		previous = rs[len(rs)-1].RecordHash
	}
	rec := Record{JobID: job, Seq: tail + 1, RecordID: NewUUID(), Generation: e.Generation, ClaimSeq: claimSeq, Actor: actor, Key: key, RequestSHA: digest, Imported: r.Kind == "import", ReceivedAt: e.time().Format("2006-01-02T15:04:05.000000Z"), PreviousHash: previous, Kind: r.Kind, Payload: p}
	if r.Kind == "claim" {
		rec.ClaimSeq = rec.Seq
	}
	if r.Kind == "import" {
		rec.Source = Object{"source_locator": p["source_locator"], "source_sha256": p["source_sha256"]}
	}
	b, _ := json.Marshal(rec)
	var hashObject Object
	_ = json.Unmarshal(b, &hashObject)
	delete(hashObject, "record_hash")
	rec.RecordHash, err = Digest(hashObject)
	if err != nil {
		return WriteResponse{}, nil, err
	}
	updated := s
	updated.Jobs = copyJobs(s.Jobs)
	updated.Jobs[job] = append(append([]Record{}, rs...), rec)
	summary, err := updated.Summary(job, e.time())
	if err != nil {
		return WriteResponse{}, nil, err
	}
	return WriteResponse{Contract, true, rec, summary}, content, nil
}
func ValidateRequest(r Request) error {
	b, err := json.Marshal(r)
	if err != nil {
		return Error("invalid_request")
	}
	v, err := Decode(b)
	if err != nil {
		return err
	}
	if err = evidenceSize(r.Kind, r.Payload); err != nil {
		return err
	}
	return Validate("Request", v)
}
func copyJobs(m map[string][]Record) map[string][]Record {
	n := map[string][]Record{}
	for k, v := range m {
		n[k] = v
	}
	return n
}

func (e Engine) claim(s Snapshot, actor, job string, old *Record, sum Summary, p Object) error {
	fail := func() error { return Error("claim_conflict") }
	if Text(p, "job_id") != job {
		return fail()
	}
	reviewRole := Text(p, "role") == "tester" || Text(p, "role") == "reviewer" || Text(p, "role") == "counsel"
	if !Central(actor) && !reviewRole {
		return Error("scope_denied")
	}
	if reviewRole {
		if old != nil || p["predecessor_job_id"] != nil || p["supersedes"] != nil {
			return fail()
		}
		target := Obj(p, "review_of")
		if target == nil || Text(target, "job_id") == job {
			return fail()
		}
		subject := s.Claim(Text(target, "job_id"))
		if subject == nil {
			return Error("not_found")
		}
		if Review(subject) {
			return fail()
		}
		if !Central(actor) && (Text(subject.Payload, "host_client_id") != e.Policy.Host(actor) || Text(p, "host_client_id") != e.Policy.Host(actor)) {
			return Error("host_mismatch")
		}
		state, x := s.Summary(subject.JobID, e.time())
		if x != nil {
			return x
		}
		if state.State == "ended" {
			return Error("job_ended")
		}
		if state.State == "joined" {
			return Error("review_sealed")
		}
		if state.State == "importing" {
			return Error("migration_frozen")
		}
		if state.State == "sealed" {
			return Error("import_sealed")
		}
		if state.State != "active" && !(Central(actor) && CentralReviewerP1UsesSubjectPhase && state.State == "shadow") {
			return fail()
		}
		for _, key := range []string{"task_id", "tier", "owner_lane"} {
			if p[key] != subject.Payload[key] {
				return fail()
			}
		}
		if Text(p, "parent_lane") != Text(subject.Payload, "owner_lane") || p["predecessor_job_id"] != nil || p["supersedes"] != nil || old != nil {
			return fail()
		}
		n := Num(target, "round_no")
		o, v, xv := s.Round(subject.JobID, n)
		if n != state.Next && (o == nil || v != nil || xv != nil) {
			return fail()
		}
		if o != nil && Text(o.Payload, "tester_job_id") != "" && Text(o.Payload, "tester_job_id") != job {
			return fail()
		}
		return nil
	}
	if p["review_of"] != nil {
		return fail()
	}
	if old != nil {
		if Num(p, "supersedes") != old.Seq || sum.State != "shadow" {
			return fail()
		}
		for _, r := range s.Jobs[job] {
			if r.Kind != "claim" {
				return fail()
			}
		}
		for other := range s.Jobs {
			c := s.Claim(other)
			if c != nil && other != job && (Text(c.Payload, "parent_lane") == Text(old.Payload, "owner_lane") || Text(c.Payload, "owner_lane") == Text(old.Payload, "owner_lane")) {
				return fail()
			}
		}
	} else if p["supersedes"] != nil {
		return fail()
	}
	if pred := Text(p, "predecessor_job_id"); pred != "" {
		if pred == job {
			return fail()
		}
		c := s.Claim(pred)
		if c == nil || Num(c.Payload, "task_id") != Num(p, "task_id") {
			return fail()
		}
		state, x := s.Summary(pred, e.time())
		if x != nil || state.State != "ended" {
			return fail()
		}
		for id := range s.Jobs {
			c := s.Claim(id)
			if c != nil && id != job && Text(c.Payload, "predecessor_job_id") == pred {
				return fail()
			}
		}
	}
	lanes := map[string]string{}
	for lane, parent := range e.Policy.lanes {
		lanes[lane] = ""
		if parent != nil {
			lanes[lane] = *parent
		}
	}
	for id := range s.Jobs {
		if id == job {
			continue
		}
		c := s.Claim(id)
		if c != nil && Text(c.Payload, "role") == "builder" {
			owner, parent := Text(c.Payload, "owner_lane"), Text(c.Payload, "parent_lane")
			if prior, ok := lanes[owner]; ok && prior != parent {
				return fail()
			}
			lanes[owner] = parent
		}
	}
	owner, parent := Text(p, "owner_lane"), Text(p, "parent_lane")
	if _, ok := lanes[parent]; !ok {
		return fail()
	}
	if existing, ok := lanes[owner]; ok {
		if existing != parent {
			return fail()
		}
	} else if Text(p, "role") != "builder" || !e.Policy.createLanes {
		return fail()
	}
	for cursor := parent; cursor != ""; cursor = lanes[cursor] {
		if cursor == owner {
			return fail()
		}
	}
	return nil
}
func (e Engine) assignment(s Snapshot, job string, round int64, id, actor string) error {
	c, subject := s.Claim(id), s.Claim(job)
	if !Review(c) || subject == nil {
		return Error("round_not_open")
	}
	of := Obj(c.Payload, "review_of")
	if Text(of, "job_id") != job || Num(of, "round_no") != round || Num(c.Payload, "task_id") != Num(subject.Payload, "task_id") {
		return Error("round_not_open")
	}
	for _, r := range s.Jobs[id] {
		if r.Kind == "end" {
			return Error("round_not_open")
		}
	}
	state, x := s.Summary(job, e.time())
	if x != nil {
		return x
	}
	// P1 is the subject's phase. Central provenance, rather than the server's
	// global mode, permits a staged shadow assignment (dr-1925-1).
	if state.State != "active" && !(CentralReviewerP1UsesSubjectPhase && Central(c.Actor) && state.State == "shadow") {
		return Error("round_not_open")
	}
	if actor != "" && e.Policy.Host(actor) != Text(c.Payload, "host_client_id") {
		return Error("host_mismatch")
	}
	return nil
}
func (e Engine) consume(s Snapshot, job string, sum Summary, p Object, field, kind string) error {
	seq := Num(p, field)
	var grant *Record
	for _, r := range s.Jobs[job] {
		if r.Seq == seq && r.Kind == "grant" {
			x := r
			grant = &x
		}
	}
	if grant == nil || grant.ClaimSeq != sum.ClaimSeq || (s.Activation(job) > 0 && grant.Seq < s.Activation(job)) {
		return Error("grant_invalid")
	}
	// Consumption precedes target/expiry checks, including after a void.
	if s.Consumed(job, seq) {
		return Error("grant_consumed")
	}
	if kind != "" && Text(grant.Payload, "grant_type") != kind {
		return Error("grant_invalid")
	}
	if kind == "" {
		return nil
	}
	expires, x := time.Parse(time.RFC3339Nano, Text(grant.Payload, "expires_at"))
	if x != nil {
		return Error("grant_invalid")
	}
	if !expires.After(e.time()) {
		return Error("grant_expired")
	}
	if kind == "" {
		return nil
	}
	g := grant.Payload
	if kind == "void" {
		if Num(g, "round_no") != Num(p, "round_no") {
			return Error("grant_invalid")
		}
		return nil
	}
	if Text(g, "head") != Text(p, "head") {
		return Error("grant_invalid")
	}
	if kind != "join_waiver" && Num(g, "round_no") != sum.Next {
		return Error("grant_invalid")
	}
	if kind == "head_repeat" && (Text(p, "tester_job_id") == "" || Text(p, "tester_job_id") != Text(g, "tester_job_id")) {
		return Error("grant_invalid")
	}
	return nil
}
func (e Engine) grant(s Snapshot, job string, sum Summary, p Object) error {
	invalid := func() error { return Error("grant_invalid") }
	expiry, x := time.Parse(time.RFC3339Nano, Text(p, "expires_at"))
	if x != nil || !expiry.After(e.time()) || expiry.After(e.time().Add(7*24*time.Hour)) {
		return invalid()
	}
	switch Text(p, "grant_type") {
	case "extension":
		if Num(p, "extra") != 1 || Num(p, "round_no") != sum.Next || p["head"] == nil || p["earlier_round_no"] != nil || p["tester_job_id"] != nil {
			return invalid()
		}
	case "void":
		o, _, _ := s.Round(job, Num(p, "round_no"))
		if o == nil || Num(p, "extra") != 0 || p["head"] != nil || p["earlier_round_no"] != nil || p["tester_job_id"] != nil {
			return invalid()
		}
	case "head_repeat":
		if Num(p, "extra") != 0 || Num(p, "round_no") != sum.Next || p["head"] == nil || Num(p, "earlier_round_no") >= sum.Next || Text(p, "tester_job_id") == "" {
			return invalid()
		}
		found := false
		for _, r := range e.history(s, job) {
			if (r.Kind == "round_open" || r.Kind == "verdict") && Num(r.Payload, "round_no") == Num(p, "earlier_round_no") && Text(r.Payload, "head") == Text(p, "head") {
				found = true
			}
		}
		for _, identity := range e.identities(s, job) {
			n := Num(identity, "round_no")
			if n == 0 {
				n = Num(identity, "round")
			}
			if n == Num(p, "earlier_round_no") && Text(identity, "head") == Text(p, "head") {
				found = true
			}
		}
		if !found || e.assignment(s, job, sum.Next, Text(p, "tester_job_id"), "") != nil {
			return invalid()
		}
	case "join_waiver":
		if p["round_no"] != nil || p["head"] == nil || Num(p, "extra") != 0 || p["earlier_round_no"] != nil || p["tester_job_id"] != nil {
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}
func (e Engine) history(s Snapshot, job string) []Record {
	out := []Record{}
	seen := map[string]bool{}
	for job != "" && !seen[job] {
		seen[job] = true
		out = append(out, s.Jobs[job]...)
		c := s.Claim(job)
		if c == nil {
			break
		}
		job = Text(c.Payload, "predecessor_job_id")
	}
	return out
}
func (e Engine) identities(s Snapshot, job string) []Object {
	out := []Object{}
	for _, r := range e.history(s, job) {
		if r.Kind == "verdict" {
			out = append(out, r.Payload)
		}
		if r.Kind == "import" {
			v, x := Decode(s.Contents[r.JobID][r.Seq])
			if x == nil {
				m, _ := v.(map[string]any)
				if m != nil {
					if Text(m, "kind") != "job.verdict" {
						continue
					}
					if payload := Obj(m, "payload"); payload != nil {
						m = payload
					}
					m = Clone(m)
					if path, err := NormalizePath(Text(m, "path")); err == nil {
						m["path"] = path
					} else {
						delete(m, "path")
					}
					m["head"] = strings.ToLower(Text(m, "head"))
					m["content_sha256"] = strings.ToLower(Text(m, "content_sha256"))
					out = append(out, m)
				}
			}
		}
	}
	return out
}
func (e Engine) headReserved(s Snapshot, job, head string) bool {
	if claim := s.Claim(job); claim != nil {
		if predecessor := Text(claim.Payload, "predecessor_job_id"); predecessor != "" {
			if e.headReserved(s, predecessor, head) {
				return true
			}
		}
	}

	for _, p := range e.identities(s, job) {
		if Text(p, "head") == head {
			return true
		}
	}
	for _, r := range s.LiveRecords(job) {
		if r.Kind == "round_open" && Text(r.Payload, "head") == head {
			_, _, v := s.Round(job, Num(r.Payload, "round_no"))
			if v == nil {
				return true
			}
		}
	}
	return false
}
func (e Engine) accountImport(s Snapshot, job string) (int64, int64, error) {
	count, inherited, takeover, floor := int64(0), int64(0), int64(0), int64(0)
	work := false
	c := s.Claim(job)
	if c != nil && Text(c.Payload, "predecessor_job_id") != "" {
		p, x := s.Summary(Text(c.Payload, "predecessor_job_id"), e.time())
		if x != nil {
			return 0, 0, x
		}
		inherited = p.Used
		floor = p.Next - 1
	}
	for _, r := range s.Jobs[job] {
		p, kind := r.Payload, r.Kind
		if kind == "import" {
			work = true
			value, x := Decode(s.Contents[job][r.Seq])
			if x != nil {
				continue
			}
			m, ok := value.(map[string]any)
			if !ok {
				continue
			}
			kind = Text(m, "kind")
			p = m
			if payload := Obj(m, "payload"); payload != nil {
				p = payload
			}
			if kind == "job.round" {
				count++
			}
			if kind == "job.verdict" {
				n, ok := p["round"].(float64)
				if !ok || n != float64(int64(n)) {
					count++
				}
			}
			if kind == "job.takeover" {
				if n, ok := p["rounds_used"].(float64); ok && n > MaxInt {
					return 0, 0, Error("invalid_request")
				}
				if n, ok := p["rounds_used"].(float64); ok && n >= 0 && n <= MaxInt && n == float64(int64(n)) && int64(n) > takeover {
					takeover = int64(n)
				}
			}
			if kind == "job.round" || kind == "job.verdict" || kind == "job.round_void" {
				if n, ok := p["round"].(float64); ok && n > MaxInt {
					return 0, 0, Error("invalid_request")
				}
				if n, ok := p["round"].(float64); ok && n > 0 && n == float64(int64(n)) && int64(n) > floor {
					floor = int64(n)
				}
			}
		} else {
			switch kind {
			case "round_open":
				work = true
				count++
			case "verdict", "void", "completed", "joined", "grant", "grant_cancel":
				work = true
			}
			if kind == "round_open" || kind == "verdict" || kind == "void" {
				if n := Num(p, "round_no"); n > floor {
					floor = n
				}
			}
		}
	}
	if takeover > MaxInt-inherited || count > MaxInt-inherited-takeover {
		return 0, 0, Error("invalid_request")
	}
	baseline := count + inherited + takeover
	if work && baseline < 3 {
		baseline = 3
	}
	if floor < baseline {
		floor = baseline
	}
	if floor >= MaxInt {
		return 0, 0, Error("invalid_request")
	}
	return baseline, floor, nil
}

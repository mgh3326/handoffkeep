package wrkrecords

import (
	"encoding/json"
	"sort"
	"time"
)

type Object = map[string]any

func Text(m Object, k string) string { s, _ := m[k].(string); return s }
func Num(m Object, k string) int64 {
	switch n := m[k].(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}
func Obj(m Object, k string) Object { o, _ := m[k].(map[string]any); return o }
func Clone(m Object) Object {
	b, _ := json.Marshal(m)
	var n Object
	_ = json.Unmarshal(b, &n)
	return n
}

type Request struct {
	Kind        string `json:"kind"`
	ClaimSeq    int64  `json:"claim_seq"`
	ExpectedSeq int64  `json:"expected_seq"`
	Payload     Object `json:"payload"`
}

func ParseRequest(raw []byte) (Request, error) {
	v, e := Decode(raw)
	if e != nil {
		return Request{}, e
	}
	if e = Validate("Request", v); e != nil {
		return Request{}, e
	}
	b, _ := json.Marshal(v)
	var r Request
	e = json.Unmarshal(b, &r)
	return r, e
}

type Record struct {
	JobID        string `json:"job_id"`
	Seq          int64  `json:"seq"`
	RecordID     string `json:"record_id"`
	Generation   string `json:"store_generation"`
	ClaimSeq     int64  `json:"claim_seq"`
	Actor        string `json:"actor_client_id"`
	Key          string `json:"idempotency_key"`
	RequestSHA   string `json:"request_sha256"`
	Imported     bool   `json:"imported"`
	Source       Object `json:"source"`
	ReceivedAt   string `json:"received_at"`
	PreviousHash string `json:"previous_hash"`
	RecordHash   string `json:"record_hash"`
	Kind         string `json:"kind"`
	Payload      Object `json:"payload"`
}
type Summary struct {
	JobID       string   `json:"job_id"`
	ClaimSeq    int64    `json:"claim_seq"`
	SnapshotSeq int64    `json:"snapshot_seq"`
	State       string   `json:"state"`
	Baseline    int64    `json:"baseline_used"`
	Used        int64    `json:"used"`
	Cap         int      `json:"cap"`
	Next        int64    `json:"next_round_no"`
	Open        []int64  `json:"open_rounds"`
	LastVerdict *int64   `json:"last_verdict_seq"`
	Grants      []int64  `json:"active_grants"`
	Warnings    []string `json:"warnings"`
	Generation  string   `json:"store_generation"`
	TailHash    string   `json:"tail_hash"`
}
type WriteResponse struct {
	Contract string  `json:"contract"`
	Created  bool    `json:"created"`
	Record   Record  `json:"record"`
	Summary  Summary `json:"summary"`
}
type JobResponse struct {
	Contract string  `json:"contract"`
	Claim    Record  `json:"claim"`
	Summary  Summary `json:"summary"`
}
type RecordsResponse struct {
	Contract string   `json:"contract"`
	Snapshot int64    `json:"snapshot_seq"`
	Next     int64    `json:"next_after_seq"`
	More     bool     `json:"has_more"`
	Records  []Record `json:"records"`
	Summary  Summary  `json:"summary"`
}
type ContentResponse struct {
	JobID  string `json:"job_id"`
	Seq    int64  `json:"seq"`
	Base64 string `json:"content_b64"`
	SHA    string `json:"content_sha256"`
	Size   int    `json:"size_bytes"`
}
type Capabilities struct {
	Contract   string `json:"contract"`
	Generation string `json:"store_generation"`
	ClientID   string `json:"client_id"`
	PolicySHA  string `json:"policy_sha256"`
	Writes     bool   `json:"writes_available"`
}
type Receipt struct {
	JobID      string `json:"job_id"`
	Seq        int64  `json:"seq"`
	Hash       string `json:"record_hash"`
	Generation string `json:"store_generation"`
}

func (r Record) Receipt() Receipt { return Receipt{r.JobID, r.Seq, r.RecordHash, r.Generation} }

type Snapshot struct {
	Jobs     map[string][]Record
	Contents map[string]map[int64][]byte
}

func (s Snapshot) Claim(job string) *Record {
	rs := s.Jobs[job]
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Kind == "claim" {
			r := rs[i]
			return &r
		}
	}
	return nil
}
func Review(r *Record) bool {
	return r != nil && (Text(r.Payload, "role") == "tester" || Text(r.Payload, "role") == "reviewer" || Text(r.Payload, "role") == "counsel") && Obj(r.Payload, "review_of") != nil
}
func (s Snapshot) ReceiptRequired(job string) bool {
	for _, r := range s.Jobs[job] {
		if r.Kind == "activate" || (r.Kind == "claim" && Review(&r)) {
			return true
		}
	}
	return false
}
func (s Snapshot) Activation(job string) int64 {
	for _, r := range s.Jobs[job] {
		if r.Kind == "activate" {
			return r.Seq
		}
	}
	return 0
}
func (s Snapshot) LiveRecords(job string) []Record {
	a := s.Activation(job)
	out := []Record{}
	for _, r := range s.Jobs[job] {
		if a == 0 || r.Seq > a {
			out = append(out, r)
		}
	}
	return out
}
func (s Snapshot) Round(job string, n int64) (open, verdict, void *Record) {
	for _, r := range s.LiveRecords(job) {
		if Num(r.Payload, "round_no") != n {
			continue
		}
		c := r
		switch r.Kind {
		case "round_open":
			open = &c
		case "verdict":
			verdict = &c
		case "void":
			void = &c
		}
	}
	return
}
func (s Snapshot) Consumed(job string, seq int64) bool {
	for _, r := range s.Jobs[job] {
		for _, field := range []string{"extension_grant_seq", "repeat_grant_seq", "waiver_grant_seq", "grant_seq"} {
			if Num(r.Payload, field) == seq {
				return true
			}
		}
	}
	return false
}
func (s Snapshot) Summary(job string, now time.Time) (Summary, error) {
	return s.summary(job, now, map[string]bool{})
}
func (s Snapshot) summary(job string, now time.Time, seen map[string]bool) (Summary, error) {
	sum := Summary{JobID: job, State: "shadow", Cap: 3, Next: 1, Open: []int64{}, Grants: []int64{}, Warnings: []string{}}
	rs := s.Jobs[job]
	if len(rs) == 0 {
		return sum, Error("not_found")
	}
	if seen[job] {
		return sum, Error("claim_conflict")
	}
	seen[job] = true
	for _, r := range rs {
		sum.SnapshotSeq = r.Seq
		sum.Generation = r.Generation
		sum.TailHash = r.RecordHash
		switch r.Kind {
		case "claim":
			sum.ClaimSeq = r.Seq
		case "activate":
			sum.State = "active"
			sum.Baseline = Num(r.Payload, "baseline_used")
			sum.Next = 1
			sum.LastVerdict = nil
		case "import_begin":
			sum.State = "importing"
		case "import_seal":
			sum.State = "sealed"
			sum.Baseline = Num(r.Payload, "baseline_used")
			sum.Next = Num(r.Payload, "numbering_floor") + 1
		case "joined":
			sum.State = "joined"
		case "end":
			sum.State = "ended"
		}
	}
	// The seal's numbering floor is pinned at sealing, never inferred from an
	// asserted lower value. Production seals compute it before persistence.
	for _, r := range rs {
		if r.Kind == "import_seal" {
			sum.Next = Num(r.Payload, "numbering_floor") + 1
		}
	}
	c := s.Claim(job)
	if c != nil && Text(c.Payload, "predecessor_job_id") != "" {
		p, e := s.summary(Text(c.Payload, "predecessor_job_id"), now, seen)
		if e != nil {
			return sum, e
		}
		if sum.Next < p.Next {
			sum.Next = p.Next
		}
	}
	sum.Used = sum.Baseline
	for _, r := range s.LiveRecords(job) {
		switch r.Kind {
		case "round_open":
			sum.Used++
			n := Num(r.Payload, "round_no")
			if n >= sum.Next {
				sum.Next = n + 1
			}
			_, v, x := s.Round(job, n)
			if v == nil && x == nil {
				sum.Open = append(sum.Open, n)
			}
		case "void":
			o, v, _ := s.Round(job, Num(r.Payload, "round_no"))
			if o != nil && v == nil {
				sum.Used--
			}
		case "verdict":
			q := r.Seq
			sum.LastVerdict = &q
		}
	}
	for _, r := range s.LiveRecords(job) {
		if r.Kind != "grant" || s.Consumed(job, r.Seq) || r.ClaimSeq != sum.ClaimSeq {
			continue
		}
		t, e := time.Parse(time.RFC3339Nano, Text(r.Payload, "expires_at"))
		if e != nil || !t.After(now) {
			continue
		}
		usable := sum.State == "active" || sum.State == "shadow"
		switch Text(r.Payload, "grant_type") {
		case "extension", "head_repeat":
			usable = usable && Num(r.Payload, "round_no") == sum.Next
		case "void":
			o, v, x := s.Round(job, Num(r.Payload, "round_no"))
			usable = usable && o != nil && v == nil && x == nil
		case "join_waiver":
			usable = usable && len(s.opens(job)) == 0
		}
		if usable {
			sum.Grants = append(sum.Grants, r.Seq)
		} else {
			sum.Warnings = appendWarning(sum.Warnings, "grant_stranded")
		}
	}
	sort.Slice(sum.Open, func(i, j int) bool { return sum.Open[i] < sum.Open[j] })
	return sum, nil
}
func appendWarning(a []string, s string) []string {
	for _, x := range a {
		if x == s {
			return a
		}
	}
	return append(a, s)
}
func (s Snapshot) opens(job string) []Record {
	a := []Record{}
	for _, r := range s.LiveRecords(job) {
		if r.Kind == "round_open" {
			_, _, v := s.Round(job, Num(r.Payload, "round_no"))
			if v == nil {
				a = append(a, r)
			}
		}
	}
	return a
}

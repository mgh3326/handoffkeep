package wrk

import (
	"encoding/json"
	"sort"
	"time"
)

// Cap is the ordinary tester round cap (design §4).
const Cap = 3

// Journal is one job's committed history at a snapshot: records in seq
// order plus the archived bytes of its import records (needed for the
// conservative legacy accounting and negative identity).
type Journal struct {
	JobID   string
	Records []Record
	Imports map[int64][]byte
}

// Tail returns the last seq and hash (0 and ZeroHash for an empty journal).
func (j *Journal) Tail() (int64, string) {
	if j == nil || len(j.Records) == 0 {
		return 0, ZeroHash
	}
	r := j.Records[len(j.Records)-1]
	return r.Seq, r.RecordHash
}

// Prefix returns the journal truncated at seq (inclusive).
func (j *Journal) Prefix(seq int64) *Journal {
	out := &Journal{JobID: j.JobID, Imports: j.Imports}
	for _, r := range j.Records {
		if r.Seq <= seq {
			out.Records = append(out.Records, r)
		}
	}
	return out
}

type round struct {
	no      int64
	openSeq int64
	head    string
	tester  string
	live    bool
	verdict *Record
	void    *Record
}

type grant struct {
	rec        Record
	typ        string
	roundNo    *int64
	head       *string
	earlier    *int64
	tester     *string
	expires    time.Time
	consumedBy int64
	cancelled  bool
}

type legacyEvent struct {
	kind       string
	round      *int64
	roundBad   bool
	head       string
	path       string
	roundsUsed *int64
	verdict    string
}

// state is a job's journal folded into the facts the rules need.
type state struct {
	jobID       string
	tail        int64
	tailHash    string
	claims      []Record
	claim       *Record
	claimSeq    int64
	role        string
	reviewOf    *ReviewOf
	host        string
	taskID      int64
	tier        string
	ownerLane   string
	parentLane  string
	predecessor string

	activate *Record
	baseline int64
	floor    int64
	begin    *Record
	seal     *Record
	end      *Record
	joined   *Record

	rounds     map[int64]*round
	opens      []*round
	maxRoundNo int64
	grants     map[int64]*grant
	verdicts   []Record

	paths        map[string]bool
	contents     map[string]bool
	verdictHeads map[string]bool
	legacyRounds map[int64]string // imported legacy round number -> head
	// inheritedRounds maps every round number of the trusted predecessor
	// lineage to its head: negative identity a head_repeat grant may cite,
	// never positive evidence.
	inheritedRounds map[int64]string
	// inheritedReserved holds heads of the lineage's nonvoid
	// post-activation opens: still reserved, since only a verdict-free void
	// (never an end or a successor) releases a reservation.
	inheritedReserved map[string]bool
	predSealBroken    bool
	// predBudgetInvalid marks a lineage whose effective budget overflows
	// the safe-integer range: it stays invalid_request and never activates.
	predBudgetInvalid bool
	legacy            []legacyEvent
	workEvidence      bool
	historyBad        bool
	receiptReq        bool
	sealBroken        bool

	// inherited from a trusted predecessor
	inheritedUsed  int64
	inheritedFloor int64
}

// ReviewOf is a claim's review_of target.
type ReviewOf struct {
	JobID   string
	RoundNo int64
}

func claimReviewOf(p map[string]any) *ReviewOf {
	m, ok := p["review_of"].(map[string]any)
	if !ok {
		return nil
	}
	n, _ := pInt(m, "round_no")
	return &ReviewOf{JobID: pStr(m, "job_id"), RoundNo: n}
}

func reviewRole(role string) bool { return role == "tester" || role == "reviewer" || role == "counsel" }

// buildState folds a journal. pred is the trusted predecessor's state (nil
// for a root job); its budget, numbering and identity sets are inherited.
func buildState(j *Journal, pred *state) *state {
	s := &state{jobID: j.JobID, rounds: map[int64]*round{}, grants: map[int64]*grant{}, paths: map[string]bool{}, contents: map[string]bool{}, verdictHeads: map[string]bool{}, legacyRounds: map[int64]string{}, inheritedRounds: map[int64]string{}, inheritedReserved: map[string]bool{}}
	s.tail, s.tailHash = j.Tail()
	if pred != nil {
		// A successor inherits the predecessor's whole effective budget and
		// numbering, including a committed seal or unsealed work evidence
		// (design §2/§4/§5): never a fresh zero.
		var ok bool
		s.inheritedUsed, s.inheritedFloor, ok = pred.effectiveBudget()
		s.predBudgetInvalid = !ok
		s.predSealBroken = pred.brokenSeal()
		for h := range pred.inheritedReserved {
			s.inheritedReserved[h] = true
		}
		if pred.activate != nil {
			for _, rd := range pred.liveOpens() {
				if rd.void == nil {
					s.inheritedReserved[rd.head] = true
				}
			}
		}
		for no, h := range pred.inheritedRounds {
			s.inheritedRounds[no] = h
		}
		for no, h := range pred.legacyRounds {
			s.inheritedRounds[no] = h
		}
		for no, rd := range pred.rounds {
			s.inheritedRounds[no] = rd.head
		}
		for k := range pred.paths {
			s.paths[k] = true
		}
		for k := range pred.contents {
			s.contents[k] = true
		}
		for k := range pred.verdictHeads {
			s.verdictHeads[k] = true
		}
	}
	for i := range j.Records {
		r := j.Records[i]
		p := r.Payload
		switch r.Kind {
		case "claim":
			s.claims = append(s.claims, r)
			s.claim = &s.claims[len(s.claims)-1]
			s.claimSeq = r.Seq
			s.role = pStr(p, "role")
			s.reviewOf = claimReviewOf(p)
			s.host = pStr(p, "host_client_id")
			s.taskID, _ = pInt(p, "task_id")
			s.tier = pStr(p, "tier")
			s.ownerLane = pStr(p, "owner_lane")
			s.parentLane = pStr(p, "parent_lane")
			s.predecessor = pStr(p, "predecessor_job_id")
			if reviewRole(s.role) && s.reviewOf != nil {
				s.receiptReq = true
			}
		case "activate":
			rr := r
			s.activate = &rr
			s.baseline, _ = pInt(p, "baseline_used")
			s.receiptReq = true
			if s.seal != nil {
				s.floor, _ = pInt(s.seal.Payload, "numbering_floor")
			} else {
				s.floor = s.inheritedFloor
			}
		case "import_begin":
			rr := r
			s.begin = &rr
		case "import_seal":
			rr := r
			s.seal = &rr
			b, _ := pInt(p, "baseline_used")
			f, _ := pInt(p, "numbering_floor")
			s.sealBroken = f < b
		case "end":
			rr := r
			s.end = &rr
		case "joined":
			rr := r
			s.joined = &rr
			if s.activate == nil {
				s.workEvidence = true
			}
		case "completed":
			if s.activate == nil {
				s.workEvidence = true
			}
		case "round_open":
			no, _ := pInt(p, "round_no")
			rd := &round{no: no, openSeq: r.Seq, head: pStr(p, "head"), tester: pStr(p, "tester_job_id"), live: true}
			s.rounds[no] = rd
			s.opens = append(s.opens, rd)
			if no > s.maxRoundNo {
				s.maxRoundNo = no
			}
			if s.activate == nil {
				s.workEvidence = true
			}
			for _, k := range []string{"extension_grant_seq", "repeat_grant_seq"} {
				if g, ok := pInt(p, k); ok {
					if gs := s.grants[g]; gs != nil {
						gs.consumedBy = r.Seq
					}
				}
			}
		case "verdict":
			no, _ := pInt(p, "round_no")
			rr := r
			if rd := s.rounds[no]; rd != nil {
				rd.verdict = &rr
			}
			s.verdicts = append(s.verdicts, r)
			s.paths[pStr(p, "path")] = true
			s.contents[pStr(p, "content_sha256")] = true
			s.verdictHeads[pStr(p, "head")] = true
			if no > s.maxRoundNo {
				s.maxRoundNo = no
			}
			if s.activate == nil {
				s.workEvidence = true
			}
		case "void":
			no, _ := pInt(p, "round_no")
			rr := r
			if rd := s.rounds[no]; rd != nil {
				rd.void = &rr
			}
			if g, ok := pInt(p, "grant_seq"); ok {
				if gs := s.grants[g]; gs != nil {
					gs.consumedBy = r.Seq
				}
			}
			if s.activate == nil {
				s.workEvidence = true
			}
		case "grant":
			g := &grant{rec: r, typ: pStr(p, "grant_type"), roundNo: pIntPtr(p, "round_no"), head: pStrPtr(p, "head"), earlier: pIntPtr(p, "earlier_round_no"), tester: pStrPtr(p, "tester_job_id")}
			g.expires, _ = parseDateTime(pStr(p, "expires_at"))
			s.grants[r.Seq] = g
			if s.activate == nil {
				s.workEvidence = true
			}
		case "grant_cancel":
			if g, ok := pInt(p, "grant_seq"); ok {
				if gs := s.grants[g]; gs != nil {
					gs.cancelled = true
				}
			}
			if s.activate == nil {
				s.workEvidence = true
			}
		case "import":
			s.workEvidence = true
			ev := parseLegacy(j.Imports[r.Seq])
			if ev == nil {
				s.historyBad = true
				continue
			}
			if ev.kind != pStr(p, "legacy_kind") {
				s.historyBad = true
			}
			s.legacy = append(s.legacy, *ev)
			if ev.kind == "job.verdict" {
				if ev.path != "" {
					if np, ok := NormalizePath(ev.path); ok {
						s.paths[np] = true
					}
				}
				if ev.head != "" {
					s.verdictHeads[ev.head] = true
				}
			}
			if ev.round != nil && (ev.kind == "job.round" || ev.kind == "job.verdict") && ev.head != "" {
				s.legacyRounds[*ev.round] = ev.head
			}
			if ev.roundBad {
				s.historyBad = true
			}
		}
	}
	// Opens accepted before activation are historical once the job
	// activates: never pending, never voidable, never head-reserving.
	if s.activate != nil {
		for _, rd := range s.opens {
			rd.live = rd.openSeq > s.activate.Seq
		}
	} else {
		s.baseline = s.inheritedUsed
		s.floor = s.inheritedFloor
	}
	return s
}

// parseLegacy reads one archived legacy event for conservative accounting
// only. Nil means unparseable.
func parseLegacy(b []byte) *legacyEvent {
	var raw map[string]any
	if len(b) == 0 || json.Unmarshal(b, &raw) != nil {
		return nil
	}
	ev := &legacyEvent{}
	ev.kind, _ = raw["kind"].(string)
	if v, present := raw["round"]; present && v != nil {
		if f, ok := v.(float64); ok && f == float64(int64(f)) && f >= 1 && f <= MaxSafeInteger {
			n := int64(f)
			ev.round = &n
		} else {
			ev.roundBad = true
		}
	}
	if h, ok := raw["head"].(string); ok && sha40(h) {
		ev.head = toLowerASCII(h)
	}
	ev.path, _ = raw["path"].(string)
	ev.verdict, _ = raw["verdict"].(string)
	if v, ok := raw["rounds_used"].(float64); ok && v == float64(int64(v)) && v >= 0 && v <= MaxSafeInteger {
		n := int64(v)
		ev.roundsUsed = &n
	}
	return ev
}

func sha40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// lifecycle state of the Summary enum.
func (s *state) lifecycle() string {
	switch {
	case s.end != nil:
		return "ended"
	case s.joined != nil:
		return "joined"
	case s.activate != nil:
		return "active"
	case s.seal != nil:
		return "sealed"
	case s.begin != nil:
		return "importing"
	}
	return "shadow"
}

// liveOpens returns post-activation (or, before activation, shadow) opens.
func (s *state) liveOpens() []*round {
	var out []*round
	for _, rd := range s.opens {
		if rd.live {
			out = append(out, rd)
		}
	}
	return out
}

// used = baseline + live opens - live verdict-free voids (design §4/§5).
func (s *state) used() int64 {
	n := s.baseline
	for _, rd := range s.liveOpens() {
		if rd.void == nil {
			n++
		}
	}
	return n
}

func (s *state) nextRoundNo() int64 {
	m := s.floor
	if s.maxRoundNo > m {
		m = s.maxRoundNo
	}
	return m + 1
}

// pending lists live, nonvoid rounds without a verdict.
func (s *state) pending() []int64 {
	out := []int64{}
	for _, rd := range s.liveOpens() {
		if rd.void == nil && rd.verdict == nil {
			out = append(out, rd.no)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// headReserved: a nonvoid live open on head, or any actual verdict on it
// (live, historical, imported or inherited).
func (s *state) headReserved(head string) bool {
	if s.verdictHeads[head] || s.inheritedReserved[head] {
		return true
	}
	for _, rd := range s.liveOpens() {
		if rd.head == head && rd.void == nil {
			return true
		}
	}
	return false
}

// brokenSeal reports a seal in the job or its lineage whose numbering_floor
// is below its baseline_used: an integrity error that fails closed.
func (s *state) brokenSeal() bool {
	return (s.seal != nil && s.sealBroken) || s.predSealBroken
}

// conservative computes the design §5 sealed budget and numbering floor
// from the job's own history: L counts server shadow opens, unbound shadow
// verdicts and imported legacy rounds and unbound legacy verdicts; I is the
// inherited budget plus the largest archived takeover rounds_used. With
// work evidence the baseline is max(3, L+I), otherwise I; the floor is the
// largest round number seen, never below the baseline or the inherited
// floor. ok is false on a safe-integer overflow, including a floor whose
// next round number would leave the wire range, and for an invalid lineage.
func (s *state) conservative() (baseline, floor int64, ok bool) {
	var l, takeover int64
	for _, rd := range s.opens {
		l++
		if rd.no > floor {
			floor = rd.no
		}
	}
	for _, v := range s.verdicts {
		n, valid := pInt(v.Payload, "round_no")
		if !valid || s.rounds[n] == nil {
			l++
		}
		if valid && n > floor {
			floor = n
		}
	}
	for _, ev := range s.legacy {
		if ev.kind == "job.round" || (ev.kind == "job.verdict" && ev.round == nil) {
			l++
		}
		if ev.round != nil && (ev.kind == "job.round" || ev.kind == "job.verdict" || ev.kind == "job.round_void") && *ev.round > floor {
			floor = *ev.round
		}
		if ev.kind == "job.takeover" && ev.roundsUsed != nil && *ev.roundsUsed > takeover {
			takeover = *ev.roundsUsed
		}
	}
	if s.predBudgetInvalid || s.inheritedUsed+takeover > MaxSafeInteger {
		return 0, 0, false
	}
	i := s.inheritedUsed + takeover
	baseline = i
	if s.workEvidence {
		if l+i > MaxSafeInteger || l+i < 0 {
			return 0, 0, false
		}
		baseline = l + i
		if baseline < Cap {
			baseline = Cap
		}
	}
	if baseline > floor {
		floor = baseline
	}
	if s.inheritedFloor > floor {
		floor = s.inheritedFloor
	}
	if floor >= MaxSafeInteger {
		return 0, 0, false
	}
	return baseline, floor, true
}

// effectiveBudget is what a successor inherits: an activated job's live
// used and numbering; a sealed job's committed baseline and floor; for
// unsealed work evidence the same conservative computation a seal would
// commit; for a claim-only job what it inherited itself. ok is false when
// that budget overflows (design §5: invalid_request, blocks activation).
func (s *state) effectiveBudget() (used, floor int64, ok bool) {
	switch {
	case s.activate != nil:
		f := s.nextRoundNo() - 1
		return s.used(), f, f < MaxSafeInteger
	case s.seal != nil:
		b, _ := pInt(s.seal.Payload, "baseline_used")
		f, _ := pInt(s.seal.Payload, "numbering_floor")
		if s.maxRoundNo > f {
			f = s.maxRoundNo
		}
		return b, f, f < MaxSafeInteger
	case s.workEvidence:
		return s.conservative()
	}
	return s.inheritedUsed, s.inheritedFloor, !s.predBudgetInvalid
}

func (s *state) lastVerdictSeq() any {
	if len(s.verdicts) == 0 {
		return nil
	}
	return s.verdicts[len(s.verdicts)-1].Seq
}

// grantUsable reports a grant the summary may list as active at now.
func (s *state) grantUsable(g *grant, now time.Time) bool {
	if g.consumedBy != 0 || g.cancelled || !now.Before(g.expires) || g.rec.ClaimSeq != s.claimSeq {
		return false
	}
	if s.activate != nil && g.rec.Seq < s.activate.Seq {
		return false
	}
	if s.end != nil || s.joined != nil {
		return false
	}
	switch g.typ {
	case "extension", "head_repeat":
		return g.roundNo != nil && *g.roundNo == s.nextRoundNo()
	case "void":
		rd := s.rounds[deref(g.roundNo)]
		return rd != nil && rd.live && rd.void == nil && rd.verdict == nil
	case "join_waiver":
		return len(s.nonvoidRounds()) == 0
	}
	return false
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *state) nonvoidRounds() []*round {
	var out []*round
	for _, rd := range s.liveOpens() {
		if rd.void == nil {
			out = append(out, rd)
		}
	}
	return out
}

// Summary is the design §5 job summary at one snapshot.
type Summary struct {
	JobID           string   `json:"job_id"`
	ClaimSeq        int64    `json:"claim_seq"`
	SnapshotSeq     int64    `json:"snapshot_seq"`
	State           string   `json:"state"`
	BaselineUsed    int64    `json:"baseline_used"`
	Used            int64    `json:"used"`
	Cap             int      `json:"cap"`
	NextRoundNo     int64    `json:"next_round_no"`
	OpenRounds      []int64  `json:"open_rounds"`
	LastVerdictSeq  any      `json:"last_verdict_seq"`
	ActiveGrants    []int64  `json:"active_grants"`
	Warnings        []string `json:"warnings"`
	StoreGeneration string   `json:"store_generation"`
	TailHash        string   `json:"tail_hash"`
}

func (s *state) summary(generation string, now time.Time) Summary {
	out := Summary{JobID: s.jobID, ClaimSeq: s.claimSeq, SnapshotSeq: s.tail, State: s.lifecycle(), BaselineUsed: s.baseline, Used: s.used(), Cap: Cap, NextRoundNo: s.nextRoundNo(), OpenRounds: s.pending(), LastVerdictSeq: s.lastVerdictSeq(), ActiveGrants: []int64{}, Warnings: []string{}, StoreGeneration: generation, TailHash: s.tailHash}
	var seqs []int64
	for seq := range s.grants {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	stranded := false
	for _, seq := range seqs {
		g := s.grants[seq]
		if s.grantUsable(g, now) {
			out.ActiveGrants = append(out.ActiveGrants, seq)
		} else if g.consumedBy == 0 && !g.cancelled && now.Before(g.expires) && s.end == nil {
			stranded = true
		}
	}
	if stranded {
		out.Warnings = append(out.Warnings, "grant_stranded")
	}
	if s.historyBad {
		out.Warnings = append(out.Warnings, "migration_history_incomplete")
	}
	return out
}

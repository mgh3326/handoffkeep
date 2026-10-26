package wrk

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// Tx is one locked journal transaction supplied by a backend. Every read
// sees the transaction's own snapshot; Append stages one record (and its
// immutable content) that commits only if the whole unit commits.
type Tx interface {
	// Journal returns a job's committed records (empty Records when the job
	// does not exist).
	Journal(jobID string) (*Journal, error)
	// CanonicalClaims returns the latest claim record of every job.
	CanonicalClaims() ([]Record, error)
	// ContentExists reports stored bytes for a content-bearing record.
	ContentExists(jobID string, seq int64) (bool, error)
	// Content returns a record's stored bytes.
	Content(jobID string, seq int64) ([]byte, bool, error)
	// BindTask moves the protected task binding of taskID to jobID (from
	// release, when a correction or successor transfers it). A missing task
	// or a conflicting legacy link is claim_conflict.
	BindTask(taskID int64, jobID, release string, releaseTask int64) error
	// Append stages a record; content is nil for kinds without bytes.
	Append(rec Record, content []byte) error
}

// Backend serialises journal units. Update acquires the global lane lock
// first when lane is true, then one lock per job id in lexicographic order,
// runs fn and commits durably before returning nil. View runs fn on a
// consistent read-only snapshot.
type Backend interface {
	Update(ctx context.Context, lane bool, jobIDs []string, fn func(Tx) error) error
	View(ctx context.Context, fn func(Tx) error) error
	// RecordAttempt appends an audit row for a refused write. Failures are
	// reported but never turn a refusal into success.
	RecordAttempt(ctx context.Context, a Attempt) error
}

// Attempt is one wrk_attempts row: no body, path, credential or task text.
type Attempt struct {
	AttemptID  string
	ClientID   string
	JobID      string
	Kind       string
	Code       Code
	ReceivedAt time.Time
}

// Engine applies the design §2/§4/§5 rules. It holds no mutable state.
type Engine struct {
	Policy     *Policy
	Generation string
	// Enforce is WRK_RECORDS_API=enforce; activation needs it.
	Enforce bool
	Now     func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// WriteResult is the committed (or replayed) record and its summary.
type WriteResult struct {
	Created bool
	Record  Record
	Summary Summary
}

func kindPermission(kind string) string {
	return map[string]string{
		"activate": PermActivateWrite, "round_open": PermRoundOpen, "verdict": PermVerdictWrite,
		"void": PermVoidWrite, "joined": PermJoinedWrite, "completed": PermCompletedWrite,
		"grant": PermGrantWrite, "grant_cancel": PermGrantCancel, "end": PermEndWrite,
		"import_begin": PermImportBegin, "import_seal": PermImportSeal, "import": PermImportWrite,
	}[kind]
}

// Append handles one POST /records for an authenticated client. raw is the
// exact body; key is the Idempotency-Key header (already validated).
func (e *Engine) Append(ctx context.Context, b Backend, client, jobID, key string, raw []byte) (WriteResult, error) {
	res, kind, err := e.append(ctx, b, client, jobID, key, raw)
	if err != nil {
		code := InvalidRequest
		var we *Error
		if errors.As(err, &we) {
			code = we.Code
		} else {
			return WriteResult{}, err
		}
		_ = b.RecordAttempt(ctx, Attempt{AttemptID: NewUUID(), ClientID: client, JobID: jobID, Kind: kind, Code: code, ReceivedAt: e.now()})
	}
	return res, err
}

func (e *Engine) append(ctx context.Context, b Backend, client, jobID, key string, raw []byte) (WriteResult, string, error) {
	if !e.Policy.Known(client) {
		return WriteResult{}, "", Fail(ScopeDenied)
	}
	req, err := ParseRequest(jobID, raw)
	if err != nil {
		return WriteResult{}, "", err
	}
	kind := req.Kind
	p := req.Payload
	if kind == "claim" {
		role := pStr(p, "role")
		if reviewRole(role) {
			if !e.Policy.Has(client, PermReviewerClaimWrite) {
				return WriteResult{}, kind, Fail(ScopeDenied)
			}
		} else if !e.Policy.Has(client, PermClaimWrite) {
			return WriteResult{}, kind, Fail(ScopeDenied)
		}
	} else if !e.Policy.Has(client, kindPermission(kind)) {
		return WriteResult{}, kind, Fail(ScopeDenied)
	}
	locks := map[string]bool{jobID: true}
	for _, id := range []string{pStr(p, "tester_job_id"), pStr(p, "predecessor_job_id")} {
		if id != "" && ValidJobID(id) {
			locks[id] = true
		}
	}
	if ro := claimReviewOf(p); ro != nil && ValidJobID(ro.JobID) {
		locks[ro.JobID] = true
	}
	ids := make([]string, 0, len(locks))
	for id := range locks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out WriteResult
	err = b.Update(ctx, kind == "claim", ids, func(tx Tx) error {
		r, err := e.decide(tx, client, key, req)
		out = r
		return err
	})
	if err != nil {
		return WriteResult{}, kind, err
	}
	return out, kind, nil
}

// load builds a job's state including its whole trusted predecessor
// lineage. Predecessors are fixed by the first claim and ended, hence
// immutable, so reading them needs no lock. The chain has no length limit
// (design §2/§4 allow any number of non-branching successors); a visited
// set stops a malformed cycle.
func load(tx Tx, jobID string) (*state, *Journal, error) {
	j, err := tx.Journal(jobID)
	if err != nil {
		return nil, nil, err
	}
	pred, err := loadLineage(tx, jobID, firstClaimPredecessor(j))
	if err != nil {
		return nil, nil, err
	}
	return buildState(j, pred), j, nil
}

// loadLineage folds the predecessor chain starting at pred, root first, and
// returns pred's state (nil when there is no predecessor).
func loadLineage(tx Tx, jobID, pred string) (*state, error) {
	var chain []*Journal
	seen := map[string]bool{jobID: true}
	for cur := pred; cur != "" && !seen[cur]; {
		seen[cur] = true
		pj, err := tx.Journal(cur)
		if err != nil {
			return nil, err
		}
		chain = append(chain, pj)
		cur = firstClaimPredecessor(pj)
	}
	var st *state
	for k := len(chain) - 1; k >= 0; k-- {
		st = buildState(chain[k], st)
	}
	return st, nil
}

func firstClaimPredecessor(j *Journal) string {
	pid := ""
	for _, r := range j.Records {
		if r.Kind == "claim" {
			pid = pStr(r.Payload, "predecessor_job_id")
		}
	}
	return pid
}

// SummaryAt computes the summary of a job at snapshot seq.
func (e *Engine) SummaryAt(tx Tx, jobID string, seq int64) (Summary, error) {
	j, err := tx.Journal(jobID)
	if err != nil {
		return Summary{}, err
	}
	pj := j.Prefix(seq)
	pred, err := loadLineage(tx, jobID, firstClaimPredecessor(pj))
	if err != nil {
		return Summary{}, err
	}
	return buildState(pj, pred).summary(e.Generation, e.now()), nil
}

// ReceiptRequired is the §2 predicate: an activate record, or any accepted
// canonical review-role claim with review_of in the job's claim history.
func ReceiptRequired(j *Journal) bool {
	return buildState(j, nil).receiptReq
}

func (e *Engine) decide(tx Tx, client, key string, req *Request) (WriteResult, error) {
	s, j, err := load(tx, req.JobID)
	if err != nil {
		return WriteResult{}, err
	}
	exists := len(j.Records) > 0
	central := e.Policy.Central(client)
	host := e.Policy.Host(client)
	p := req.Payload

	// Permission (H/V) is rechecked on replay; mutable target activeness is not.
	if req.Kind != "claim" {
		if !exists {
			return WriteResult{}, Fail(NotFound)
		}
		if err := e.hostRule(tx, client, central, host, s, req); err != nil {
			return WriteResult{}, err
		}
	}
	// Successful-key replay precedes CAS, expiry, terminal state and rules.
	for _, r := range j.Records {
		if r.ActorClientID == client && r.IdempotencyKey == key {
			if r.RequestSHA256 != req.SHA256 {
				return WriteResult{}, Fail(IdempotencyConflict)
			}
			sum, err := e.SummaryAt(tx, req.JobID, r.Seq)
			if err != nil {
				return WriteResult{}, err
			}
			return WriteResult{Created: false, Record: r, Summary: sum}, nil
		}
	}
	if req.ExpectedSeq != s.tail || req.ClaimSeq != s.claimSeq {
		return WriteResult{}, Fail(SequenceConflict)
	}
	// A stored seal whose numbering_floor is below its baseline_used breaks
	// the §5 invariant: the job fails closed for every new mutation except
	// end, no round is numbered from it and nothing recomputes the floor.
	if s.brokenSeal() && req.Kind != "end" {
		return WriteResult{}, Fail(BrokenSealCode)
	}
	now := e.now()
	rec := Record{JobID: req.JobID, Seq: s.tail + 1, StoreGeneration: e.Generation, ClaimSeq: s.claimSeq, ActorClientID: client, IdempotencyKey: key, RequestSHA256: req.SHA256, ReceivedAt: FormatTime(now), PreviousHash: s.tailHash, Kind: req.Kind}
	var content []byte
	switch req.Kind {
	case "claim":
		rec.Payload, err = e.ruleClaim(tx, client, central, host, s, j, req, now)
		rec.ClaimSeq = rec.Seq
	case "activate":
		rec.Payload, err = e.ruleActivate(tx, s, p)
	case "round_open":
		rec.Payload, err = e.ruleOpen(tx, s, p, now)
	case "verdict":
		rec.Payload, err = e.ruleVerdict(tx, s, req)
		content = req.Content
	case "void":
		rec.Payload, err = e.ruleVoid(s, p, now)
	case "joined":
		rec.Payload, err = e.ruleJoined(tx, s, req, now)
		content = req.Content
	case "completed":
		rec.Payload, err = e.ruleCompleted(s, req)
		content = req.Content
	case "grant":
		rec.Payload, err = e.ruleGrant(tx, s, p, now)
	case "grant_cancel":
		rec.Payload, err = e.ruleGrantCancel(s, p)
	case "end":
		rec.Payload, err = e.ruleEnd(s, p, central)
	case "import_begin":
		rec.Payload, err = e.ruleImportBegin(s, p, rec.Seq)
	case "import":
		rec.Payload, err = e.ruleImport(s, req)
		rec.Imported = true
		rec.Source = &Source{SourceLocator: pStr(p, "source_locator"), SourceSHA256: pStr(p, "source_sha256")}
		content = req.Content
	case "import_seal":
		rec.Payload, err = e.ruleImportSeal(s, j, p)
	default:
		err = Fail(InvalidRequest)
	}
	if err != nil {
		return WriteResult{}, err
	}
	rec.RecordID = NewUUID()
	if rec.RecordHash, err = rec.ComputeHash(); err != nil {
		return WriteResult{}, Fail(InvalidRequest)
	}
	if err := tx.Append(rec, content); err != nil {
		return WriteResult{}, err
	}
	next := &Journal{JobID: j.JobID, Records: append(append([]Record{}, j.Records...), rec), Imports: j.Imports}
	if rec.Kind == "import" {
		next.Imports = map[int64][]byte{}
		for k, v := range j.Imports {
			next.Imports[k] = v
		}
		next.Imports[rec.Seq] = content
	}
	pred, err := loadLineage(tx, req.JobID, firstClaimPredecessor(next))
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Created: true, Record: rec, Summary: buildState(next, pred).summary(e.Generation, now)}, nil
}

// hostRule applies the §3 H and V columns for non-claim writes by host ids.
// Central A permissions (end, grants, imports, activate) need no host match.
func (e *Engine) hostRule(tx Tx, client string, central bool, host string, s *state, req *Request) error {
	if central {
		return nil
	}
	if host == "" {
		return Fail(ScopeDenied)
	}
	if s.host == host {
		return nil
	}
	if req.Kind == "verdict" {
		// V: a server-accepted review claim on this host assigned to this
		// exact job and round, same task.
		tid := pStr(req.Payload, "tester_job_id")
		if tid != "" {
			rs, _, err := load(tx, tid)
			if err != nil {
				return err
			}
			rn, _ := pInt(req.Payload, "round_no")
			if rs.claim != nil && reviewRole(rs.role) && rs.host == host && rs.reviewOf != nil && rs.reviewOf.JobID == req.JobID && rs.reviewOf.RoundNo == rn && rs.taskID == s.taskID {
				return nil
			}
		}
	}
	return Fail(HostMismatch)
}

// stateGate is the first business check of host/central journal writes.
func stateGate(s *state) error {
	switch s.lifecycle() {
	case "ended":
		return Fail(JobEnded)
	case "joined":
		return Fail(ReviewSealed)
	case "importing":
		return Fail(MigrationFrozen)
	case "sealed":
		return Fail(ImportSealed)
	}
	return nil
}

func reviewJob(s *state) bool { return reviewRole(s.role) && s.reviewOf != nil }

func (e *Engine) ruleClaim(tx Tx, client string, central bool, host string, s *state, j *Journal, req *Request, now time.Time) (map[string]any, error) {
	p := req.Payload
	if pStr(p, "job_id") != req.JobID {
		return nil, Fail(InvalidRequest)
	}
	out := map[string]any{}
	for k, v := range p {
		out[k] = v
	}
	out["policy_sha256"] = e.Policy.SHA256()
	out["created_at"] = FormatTime(now)
	role := pStr(p, "role")
	ro := claimReviewOf(p)
	if reviewRole(role) {
		return out, e.reviewClaim(tx, central, host, s, req, ro)
	}
	if ro != nil {
		return nil, Fail(ClaimConflict)
	}
	return out, e.subjectClaim(tx, s, j, req)
}

// reviewClaim applies the §2/R4 scoped (host) and central review-assignment
// rules. The error order follows the R4 acceptance text.
func (e *Engine) reviewClaim(tx Tx, central bool, host string, s *state, req *Request, ro *ReviewOf) error {
	p := req.Payload
	if ro == nil || ro.JobID == req.JobID {
		return Fail(ClaimConflict)
	}
	t, tj, err := load(tx, ro.JobID)
	if err != nil {
		return err
	}
	if len(tj.Records) == 0 || t.claim == nil {
		return Fail(NotFound)
	}
	if !central && (t.host != host || pStr(p, "host_client_id") != host) {
		return Fail(HostMismatch)
	}
	if err := stateGate(t); err != nil {
		return err
	}
	// Assignments are first bindings only: no reuse, correction or successor.
	if s.tail != 0 || pStr(p, "predecessor_job_id") != "" || pIntPtr(p, "supersedes") != nil {
		return Fail(ClaimConflict)
	}
	if reviewJob(t) || !(t.role == "builder" || t.role == "worker") {
		return Fail(ClaimConflict)
	}
	// Hosts mint only for an active subject; a central P1 assignment may
	// target a shadow subject.
	if t.lifecycle() != "active" && !(central && CentralShadowAssignments && t.lifecycle() == "shadow") {
		return Fail(ClaimConflict)
	}
	task, _ := pInt(p, "task_id")
	if task != t.taskID || pStr(p, "owner_lane") != t.ownerLane || pStr(p, "parent_lane") != t.ownerLane {
		return Fail(ClaimConflict)
	}
	if !central && pStr(p, "tier") != t.tier {
		return Fail(ClaimConflict)
	}
	// review_of.round_no: the target's next round, or a live open round
	// whose tester assignment is not yet frozen.
	if ro.RoundNo != t.nextRoundNo() {
		rd := t.rounds[ro.RoundNo]
		if rd == nil || !rd.live || rd.void != nil || rd.verdict != nil || rd.tester != "" {
			return Fail(ClaimConflict)
		}
	}
	return nil
}

// subjectClaim applies central builder/worker claim, correction and
// successor rules, the lane graph and the protected task binding.
func (e *Engine) subjectClaim(tx Tx, s *state, j *Journal, req *Request) error {
	p := req.Payload
	supersedes := pIntPtr(p, "supersedes")
	pred := pStr(p, "predecessor_job_id")
	task, _ := pInt(p, "task_id")
	role := pStr(p, "role")
	release, releaseTask := "", int64(0)
	if s.tail == 0 {
		if supersedes != nil {
			return Fail(ClaimConflict)
		}
		if pred != "" {
			ps, pj, err := load(tx, pred)
			if err != nil {
				return err
			}
			if len(pj.Records) == 0 || ps.claim == nil || ps.end == nil || reviewJob(ps) || ps.taskID != task || pred == req.JobID {
				return Fail(ClaimConflict)
			}
			claims, err := tx.CanonicalClaims()
			if err != nil {
				return err
			}
			for _, c := range claims {
				if c.JobID != req.JobID && pStr(c.Payload, "predecessor_job_id") == pred {
					return Fail(ClaimConflict)
				}
			}
			release, releaseTask = pred, task
		}
	} else {
		// A correction: same job, before activation and before any
		// authoritative non-claim record, superseding the current claim.
		if reviewJob(s) || supersedes == nil || *supersedes != s.claimSeq {
			return Fail(ClaimConflict)
		}
		for _, r := range j.Records {
			if r.Kind != "claim" {
				return Fail(ClaimConflict)
			}
		}
		if pred != s.predecessor {
			return Fail(ClaimConflict)
		}
		// A successor's lineage never changes task, corrections included.
		if pred != "" {
			ps, _, err := load(tx, pred)
			if err != nil {
				return err
			}
			if ps.taskID != task {
				return Fail(ClaimConflict)
			}
		}
		release, releaseTask = req.JobID, s.taskID
	}
	claims, err := tx.CanonicalClaims()
	if err != nil {
		return err
	}
	if s.tail != 0 && (pStr(p, "owner_lane") != s.ownerLane || pStr(p, "parent_lane") != s.parentLane) {
		for _, c := range claims {
			if c.JobID == req.JobID {
				continue
			}
			cp := c.Payload
			if ro := claimReviewOf(cp); ro != nil && ro.JobID == req.JobID {
				return Fail(ClaimConflict)
			}
			if pStr(cp, "parent_lane") == s.ownerLane || (pStr(cp, "role") == "worker" && pStr(cp, "owner_lane") == s.ownerLane) {
				return Fail(ClaimConflict)
			}
		}
	}
	if err := e.checkLane(claims, req.JobID, role, pStr(p, "owner_lane"), pStr(p, "parent_lane")); err != nil {
		return err
	}
	// The job's own successor lineage holds the task in turn; any other
	// canonical subject on the same task is a competing claimant.
	lineage, err := ancestors(tx, req.JobID, pred)
	if err != nil {
		return err
	}
	for _, c := range claims {
		cp := c.Payload
		ct, _ := pInt(cp, "task_id")
		if c.JobID == req.JobID || lineage[c.JobID] || ct != task {
			continue
		}
		if r := pStr(cp, "role"); r == "builder" || r == "worker" {
			return Fail(ClaimConflict)
		}
	}
	return tx.BindTask(task, req.JobID, release, releaseTask)
}

// ancestors returns the whole predecessor chain starting at pred (the
// job's trusted lineage), without a length limit.
func ancestors(tx Tx, jobID, pred string) (map[string]bool, error) {
	out := map[string]bool{}
	for cur := pred; cur != "" && cur != jobID && !out[cur]; {
		out[cur] = true
		j, err := tx.Journal(cur)
		if err != nil {
			return nil, err
		}
		cur = firstClaimPredecessor(j)
	}
	return out, nil
}

// checkLane applies the §5 lane graph algorithm against the policy roots and
// every other job's canonical builder edge.
func (e *Engine) checkLane(claims []Record, jobID, role, owner, parent string) error {
	builderParent := map[string]string{}
	for _, c := range claims {
		if c.JobID == jobID || pStr(c.Payload, "role") != "builder" {
			continue
		}
		builderParent[pStr(c.Payload, "owner_lane")] = pStr(c.Payload, "parent_lane")
	}
	parentOf := func(lane string) (string, bool, bool) {
		if pp, ok := e.Policy.LaneParent(lane); ok {
			if pp == nil {
				return "", true, true
			}
			return *pp, false, true
		}
		if bp, ok := builderParent[lane]; ok {
			return bp, false, true
		}
		return "", false, false
	}
	if role == "worker" {
		bp, ok := builderParent[owner]
		if !ok || bp != parent {
			return Fail(ClaimConflict)
		}
		return nil
	}
	if _, ok := e.Policy.LaneParent(owner); ok {
		return Fail(ClaimConflict)
	}
	if bp, ok := builderParent[owner]; ok {
		if bp != parent {
			return Fail(ClaimConflict)
		}
		return nil
	}
	if !e.Policy.ClaimLaneCreation || owner == parent {
		return Fail(ClaimConflict)
	}
	cur := parent
	for depth := 0; depth < 1024; depth++ {
		if cur == owner {
			return Fail(ClaimConflict)
		}
		next, root, ok := parentOf(cur)
		if !ok {
			return Fail(ClaimConflict)
		}
		if root {
			return nil
		}
		cur = next
	}
	return Fail(ClaimConflict)
}

func (e *Engine) ruleActivate(tx Tx, s *state, p map[string]any) (map[string]any, error) {
	if !e.Enforce {
		return nil, Fail(Unavailable)
	}
	if reviewJob(s) {
		return nil, Fail(ScopeDenied)
	}
	switch s.lifecycle() {
	case "ended":
		return nil, Fail(JobEnded)
	case "joined":
		return nil, Fail(ReviewSealed)
	case "active":
		return nil, Fail(ClaimConflict)
	case "importing":
		return nil, Fail(ImportUnsealed)
	}
	seal := pIntPtr(p, "seal_seq")
	var baseline int64
	if s.seal != nil {
		if seal == nil || *seal != s.seal.Seq {
			return nil, Fail(ImportUnsealed)
		}
		baseline, _ = pInt(s.seal.Payload, "baseline_used")
	} else {
		if seal != nil || s.workEvidence {
			return nil, Fail(ImportUnsealed)
		}
		if s.predBudgetInvalid {
			return nil, Fail(InvalidRequest)
		}
		baseline = s.inheritedUsed
	}
	return map[string]any{"baseline_used": baseline, "mode": "enforced", "reason_code": pStr(p, "reason_code"), "seal_seq": intOrNil(seal)}, nil
}

// grantCheck validates a grant reference for consumption: it must name an
// unconsumed, uncancelled, unexpired grant of typ bound to this job's
// current claim and, once active, issued after activation.
func (s *state) grantCheck(seq int64, typ string, now time.Time) (*grant, error) {
	g := s.grants[seq]
	if g == nil || g.typ != typ {
		return nil, Fail(GrantInvalid)
	}
	if g.consumedBy != 0 {
		return nil, Fail(GrantConsumed)
	}
	if g.cancelled || g.rec.ClaimSeq != s.claimSeq || (s.activate != nil && g.rec.Seq < s.activate.Seq) {
		return nil, Fail(GrantInvalid)
	}
	if !now.Before(g.expires) {
		return nil, Fail(GrantExpired)
	}
	return g, nil
}

// liveAssignment reports a reviewer claim live for (job, round): canonical,
// review-role, review_of exact, same task, not ended.
func liveAssignment(tx Tx, target *state, reviewer string, roundNo int64) (bool, error) {
	rs, rj, err := load(tx, reviewer)
	if err != nil {
		return false, err
	}
	if len(rj.Records) == 0 || !reviewJob(rs) || rs.end != nil {
		return false, nil
	}
	return rs.reviewOf.JobID == target.jobID && rs.reviewOf.RoundNo == roundNo && rs.taskID == target.taskID, nil
}

func (e *Engine) ruleOpen(tx Tx, s *state, p map[string]any, now time.Time) (map[string]any, error) {
	if reviewJob(s) {
		return nil, Fail(ScopeDenied)
	}
	if err := stateGate(s); err != nil {
		return nil, err
	}
	head := pStr(p, "head")
	tester := pStr(p, "tester_job_id")
	next := s.nextRoundNo()
	// A round numbered at the wire bound would leave next_round_no outside
	// it (design §2 L41, §5 L4028): overflow, never committed.
	if next >= MaxSafeInteger {
		return nil, Fail(InvalidRequest)
	}
	ext, rep := pIntPtr(p, "extension_grant_seq"), pIntPtr(p, "repeat_grant_seq")
	if ext != nil {
		g, err := s.grantCheck(*ext, "extension", now)
		if err != nil {
			return nil, err
		}
		if deref(g.roundNo) != next || g.head == nil || *g.head != head {
			return nil, Fail(GrantInvalid)
		}
	}
	if rep != nil {
		g, err := s.grantCheck(*rep, "head_repeat", now)
		if err != nil {
			return nil, err
		}
		if deref(g.roundNo) != next || g.head == nil || *g.head != head || g.tester == nil || tester == "" || *g.tester != tester {
			return nil, Fail(GrantInvalid)
		}
		if !s.headReserved(head) {
			return nil, Fail(GrantInvalid)
		}
	}
	if tester != "" {
		ok, err := liveAssignment(tx, s, tester, next)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, Fail(RoundNotOpen)
		}
	}
	if rep == nil && s.headReserved(head) {
		return nil, Fail(DuplicateHead)
	}
	if used := s.used(); used < Cap {
		if ext != nil {
			return nil, Fail(GrantBeforeCap)
		}
	} else if ext == nil {
		return nil, Fail(RoundCap)
	}
	return map[string]any{"round_no": next, "head": head, "tester_job_id": strOrNil(pStrPtr(p, "tester_job_id")), "extension_grant_seq": intOrNil(ext), "repeat_grant_seq": intOrNil(rep)}, nil
}

// CentralShadowAssignments reads "P1" in the §2/§6 central review
// assignment rule as the subject's phase: a central role may assign a
// reviewer to a shadow subject in either server mode (operator answer
// dr-1925-1 = A).
const CentralShadowAssignments = true

// verdictCheck is one named verdict acceptance check.
type verdictCheck struct {
	name string
	run  func(v *verdictCtx) error
}

type verdictCtx struct {
	tx      Tx
	s       *state
	req     *Request
	rd      *round
	no      int64
	path    string
	verdict string
	head    string
}

// BrokenSealCode is the refusal for a job sealed with numbering_floor below
// baseline_used: an integrity error on read that fails closed and is never
// clamped (director-1 esc-2 item 20).
const BrokenSealCode = ImportUnsealed

// VerdictCheckOrder is the verdict acceptance order, as ruled by
// director-1 on b1925-opus esc-2 (A): existence and voided, tester
// assignment, digest, grammar, path validity, guard, path duplicate,
// content duplicate, then header-head and open-head equality, then
// round_has_verdict. It reconciles the §4 order list with the §5
// transition row 6 and §8 row 2 outcomes.
var VerdictCheckOrder = []string{"round_open", "round_voided", "assignment", "digest", "grammar", "path", "guard", "duplicate_path", "duplicate_content", "head", "round_has_verdict"}

var verdictChecks = map[string]func(v *verdictCtx) error{
	"round_open": func(v *verdictCtx) error {
		if v.rd == nil || !v.rd.live {
			return Fail(RoundNotOpen)
		}
		return nil
	},
	"round_voided": func(v *verdictCtx) error {
		if v.rd.void != nil {
			return Fail(RoundVoided)
		}
		return nil
	},
	"round_has_verdict": func(v *verdictCtx) error {
		if v.rd.verdict != nil {
			return Fail(RoundHasVerdict)
		}
		return nil
	},
	"assignment": func(v *verdictCtx) error {
		tester := pStr(v.req.Payload, "tester_job_id")
		if v.rd.tester != "" && tester != v.rd.tester {
			return Fail(RoundNotOpen)
		}
		if tester != "" {
			ok, err := liveAssignment(v.tx, v.s, tester, v.no)
			if err != nil {
				return err
			}
			if !ok {
				return Fail(RoundNotOpen)
			}
		}
		return nil
	},
	"digest": func(v *verdictCtx) error {
		if ContentSHA256(v.req.Content) != pStr(v.req.Payload, "content_sha256") {
			return Fail(InvalidRequest)
		}
		return nil
	},
	"grammar": func(v *verdictCtx) error {
		verdict, head, ok := ParseVerdict(v.req.Content)
		if !ok {
			return Fail(InvalidVerdict)
		}
		if verdict != pStr(v.req.Payload, "verdict") {
			return Fail(InvalidRequest)
		}
		v.verdict, v.head = verdict, head
		return nil
	},
	"path": func(v *verdictCtx) error {
		path, ok := NormalizePath(pStr(v.req.Payload, "path"))
		if !ok {
			return Fail(InvalidPath)
		}
		v.path = path
		return nil
	},
	"guard": func(v *verdictCtx) error {
		if guard.Reject(string(v.req.Content)) != nil {
			return Fail(InvalidRequest)
		}
		return nil
	},
	"duplicate_path": func(v *verdictCtx) error {
		if v.s.paths[v.path] {
			return Fail(DuplicatePath)
		}
		return nil
	},
	"duplicate_content": func(v *verdictCtx) error {
		if v.s.contents[pStr(v.req.Payload, "content_sha256")] {
			return Fail(DuplicateContent)
		}
		return nil
	},
	"head": func(v *verdictCtx) error {
		asserted := pStr(v.req.Payload, "head")
		if v.head != asserted || asserted != v.rd.head {
			return Fail(HeadMismatch)
		}
		return nil
	},
}

func (e *Engine) ruleVerdict(tx Tx, s *state, req *Request) (map[string]any, error) {
	p := req.Payload
	if err := stateGate(s); err != nil {
		return nil, err
	}
	no, _ := pInt(p, "round_no")
	v := &verdictCtx{tx: tx, s: s, req: req, rd: s.rounds[no], no: no}
	for _, name := range VerdictCheckOrder {
		if err := verdictChecks[name](v); err != nil {
			return nil, err
		}
	}
	return map[string]any{"round_no": no, "head": v.head, "verdict": v.verdict, "path": v.path, "content_sha256": pStr(p, "content_sha256"), "size_bytes": int64(len(req.Content)), "tester_job_id": strOrNil(pStrPtr(p, "tester_job_id"))}, nil
}

func (e *Engine) ruleVoid(s *state, p map[string]any, now time.Time) (map[string]any, error) {
	if reviewJob(s) {
		return nil, Fail(ScopeDenied)
	}
	if err := stateGate(s); err != nil {
		return nil, err
	}
	no, _ := pInt(p, "round_no")
	gseq, _ := pInt(p, "grant_seq")
	g, err := s.grantCheck(gseq, "void", now)
	if err != nil {
		return nil, err
	}
	if deref(g.roundNo) != no {
		return nil, Fail(GrantInvalid)
	}
	rd := s.rounds[no]
	if rd == nil || !rd.live {
		return nil, Fail(RoundNotOpen)
	}
	if rd.void != nil {
		return nil, Fail(RoundVoided)
	}
	if rd.verdict != nil {
		return nil, Fail(RoundHasVerdict)
	}
	return map[string]any{"round_no": no, "grant_seq": gseq, "reason_code": pStr(p, "reason_code")}, nil
}

func (e *Engine) reportChecks(req *Request) error {
	if ContentSHA256(req.Content) != pStr(req.Payload, "report_sha256") {
		return Fail(InvalidRequest)
	}
	if guard.Reject(string(req.Content)) != nil {
		return Fail(InvalidRequest)
	}
	return nil
}

func (e *Engine) ruleJoined(tx Tx, s *state, req *Request, now time.Time) (map[string]any, error) {
	p := req.Payload
	if reviewJob(s) {
		return nil, Fail(ScopeDenied)
	}
	if err := stateGate(s); err != nil {
		return nil, err
	}
	head := pStr(p, "head")
	waiver := pIntPtr(p, "waiver_grant_seq")
	rounds := s.nonvoidRounds()
	if waiver != nil {
		g, err := s.grantCheck(*waiver, "join_waiver", now)
		if err != nil {
			return nil, err
		}
		if g.head == nil || *g.head != head || len(rounds) != 0 {
			return nil, Fail(GrantInvalid)
		}
	}
	if !ValidPRURL(pStr(p, "pr")) {
		return nil, Fail(InvalidRequest)
	}
	if err := e.reportChecks(req); err != nil {
		return nil, err
	}
	verdictSeqs := []int64{}
	if waiver == nil {
		if len(rounds) == 0 {
			return nil, Fail(ZeroRounds)
		}
		sort.Slice(rounds, func(i, k int) bool { return rounds[i].no < rounds[k].no })
		for _, rd := range rounds {
			if rd.verdict == nil {
				return nil, Fail(ReviewIncomplete)
			}
			verdictSeqs = append(verdictSeqs, rd.verdict.Seq)
		}
		var pass *round
		for _, rd := range rounds {
			if rd.head == head {
				pass = rd
			}
		}
		if pass == nil {
			return nil, Fail(ReviewIncomplete)
		}
		if pStr(pass.verdict.Payload, "verdict") != "PASS" {
			return nil, Fail(BlockerUnresolved)
		}
		for _, rd := range rounds {
			if rd.no > pass.no && pStr(rd.verdict.Payload, "verdict") == "BLOCKER" {
				return nil, Fail(BlockerUnresolved)
			}
		}
		if s.tier == "T2" || s.tier == "T3" {
			tid := pStr(pass.verdict.Payload, "tester_job_id")
			if tid == "" {
				return nil, Fail(ReviewIncomplete)
			}
			rs, rj, err := load(tx, tid)
			if err != nil {
				return nil, err
			}
			if len(rj.Records) == 0 || !reviewJob(rs) || rs.reviewOf.JobID != s.jobID || rs.reviewOf.RoundNo != pass.no || rs.taskID != s.taskID {
				return nil, Fail(ReviewIncomplete)
			}
		}
		sort.Slice(verdictSeqs, func(i, k int) bool { return verdictSeqs[i] < verdictSeqs[k] })
		for _, seq := range verdictSeqs {
			ok, err := tx.ContentExists(s.jobID, seq)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, Fail(ReviewIncomplete)
			}
		}
	}
	return map[string]any{"head": head, "pr": pStr(p, "pr"), "report_sha256": pStr(p, "report_sha256"), "report_size_bytes": int64(len(req.Content)), "verdict_seqs": verdictSeqs, "waiver_grant_seq": intOrNil(waiver)}, nil
}

func (e *Engine) ruleCompleted(s *state, req *Request) (map[string]any, error) {
	if err := stateGate(s); err != nil {
		return nil, err
	}
	if err := e.reportChecks(req); err != nil {
		return nil, err
	}
	warnings := []string{}
	if !reviewJob(s) {
		if len(s.pending()) > 0 {
			warnings = append(warnings, "rounds_pending")
		}
		if len(s.nonvoidRounds()) == 0 {
			warnings = append(warnings, "zero_rounds")
		}
	}
	return map[string]any{"report_sha256": pStr(req.Payload, "report_sha256"), "report_size_bytes": int64(len(req.Content)), "warnings": warnings}, nil
}

func (e *Engine) ruleGrant(tx Tx, s *state, p map[string]any, now time.Time) (map[string]any, error) {
	if reviewJob(s) {
		return nil, Fail(ScopeDenied)
	}
	if err := stateGate(s); err != nil {
		return nil, err
	}
	typ := pStr(p, "grant_type")
	roundNo, head, earlier, tester := pIntPtr(p, "round_no"), pStrPtr(p, "head"), pIntPtr(p, "earlier_round_no"), pStrPtr(p, "tester_job_id")
	extra, _ := pInt(p, "extra")
	expires, _ := parseDateTime(pStr(p, "expires_at"))
	if !expires.After(now) || expires.After(now.Add(7*24*time.Hour)) {
		return nil, Fail(GrantInvalid)
	}
	next := s.nextRoundNo()
	bad := false
	switch typ {
	case "extension":
		bad = deref(roundNo) != next || head == nil || extra != 1 || earlier != nil || tester != nil
	case "void":
		bad = roundNo == nil || s.rounds[*roundNo] == nil || head != nil || extra != 0 || earlier != nil || tester != nil
	case "head_repeat":
		bad = deref(roundNo) != next || head == nil || extra != 0 || earlier == nil || tester == nil || *earlier >= next
		if !bad {
			onHead := false
			if rd := s.rounds[*earlier]; rd != nil && rd.head == *head {
				onHead = true
			}
			if s.legacyRounds[*earlier] == *head || s.inheritedRounds[*earlier] == *head {
				onHead = true
			}
			bad = !onHead
		}
		if !bad {
			rs, rj, err := load(tx, *tester)
			if err != nil {
				return nil, err
			}
			bad = len(rj.Records) == 0 || !reviewJob(rs) || rs.reviewOf.JobID != s.jobID || rs.reviewOf.RoundNo != next
		}
	case "join_waiver":
		bad = roundNo != nil || head == nil || extra != 0 || earlier != nil || tester != nil
	default:
		bad = true
	}
	if bad {
		return nil, Fail(GrantInvalid)
	}
	out := map[string]any{}
	for k, v := range p {
		out[k] = v
	}
	return out, nil
}

func (e *Engine) ruleGrantCancel(s *state, p map[string]any) (map[string]any, error) {
	if err := stateGate(s); err != nil {
		return nil, err
	}
	gseq, _ := pInt(p, "grant_seq")
	g := s.grants[gseq]
	if g == nil || g.cancelled {
		return nil, Fail(GrantInvalid)
	}
	if g.consumedBy != 0 {
		return nil, Fail(GrantConsumed)
	}
	return map[string]any{"grant_seq": gseq, "reason_code": pStr(p, "reason_code")}, nil
}

func (e *Engine) ruleEnd(s *state, p map[string]any, central bool) (map[string]any, error) {
	if s.end != nil {
		return nil, Fail(JobEnded)
	}
	if s.lifecycle() == "importing" && !central {
		return nil, Fail(MigrationFrozen)
	}
	return map[string]any{"end_type": pStr(p, "end_type"), "reason_code": pStr(p, "reason_code")}, nil
}

func importGate(s *state, want string) error {
	if reviewJob(s) {
		return Fail(ScopeDenied)
	}
	st := s.lifecycle()
	switch st {
	case "ended":
		return Fail(JobEnded)
	case "joined":
		return Fail(ReviewSealed)
	case "active", "sealed":
		return Fail(ImportSealed)
	}
	if st != want {
		if want == "shadow" {
			return Fail(MigrationFrozen)
		}
		return Fail(ImportUnsealed)
	}
	return nil
}

func (e *Engine) ruleImportBegin(s *state, p map[string]any, seq int64) (map[string]any, error) {
	if err := importGate(s, "shadow"); err != nil {
		return nil, err
	}
	return map[string]any{"reason_code": pStr(p, "reason_code"), "shadow_cutoff_seq": seq - 1}, nil
}

func (e *Engine) ruleImport(s *state, req *Request) (map[string]any, error) {
	if err := importGate(s, "importing"); err != nil {
		return nil, err
	}
	p := req.Payload
	if ContentSHA256(req.Content) != pStr(p, "source_sha256") {
		return nil, Fail(InvalidRequest)
	}
	if guard.Reject(string(req.Content)) != nil {
		return nil, Fail(InvalidRequest)
	}
	return map[string]any{"source_locator": pStr(p, "source_locator"), "source_sha256": pStr(p, "source_sha256"), "original_actor_client_id": strOrNil(pStrPtr(p, "original_actor_client_id")), "original_at": strOrNil(pStrPtr(p, "original_at")), "legacy_kind": pStr(p, "legacy_kind")}, nil
}

// ruleImportSeal checks the manifest and computes the conservative baseline
// and numbering floor of design §5 (L, I, work evidence).
func (e *Engine) ruleImportSeal(s *state, j *Journal, p map[string]any) (map[string]any, error) {
	if err := importGate(s, "importing"); err != nil {
		return nil, err
	}
	beginSeq, _ := pInt(p, "begin_seq")
	count, _ := pInt(p, "expected_import_count")
	if s.begin == nil || beginSeq != s.begin.Seq {
		return nil, Fail(ImportUnsealed)
	}
	manifest := []any{}
	for _, r := range j.Records {
		if r.Kind == "import" && r.Seq > s.begin.Seq {
			manifest = append(manifest, map[string]any{"seq": r.Seq, "source_locator": r.Source.SourceLocator, "source_sha256": r.Source.SourceSHA256})
		}
	}
	if int64(len(manifest)) != count {
		return nil, Fail(ImportUnsealed)
	}
	if ContentSHA256(MustCanonical(manifest)) != pStr(p, "manifest_sha256") {
		return nil, Fail(ImportUnsealed)
	}
	baseline, floor, ok := s.conservative()
	if !ok {
		return nil, Fail(InvalidRequest)
	}
	return map[string]any{"begin_seq": beginSeq, "expected_import_count": count, "manifest_sha256": pStr(p, "manifest_sha256"), "reason_code": pStr(p, "reason_code"), "shadow_cutoff_seq": pIntOr(s.begin.Payload, "shadow_cutoff_seq"), "baseline_used": baseline, "numbering_floor": floor}, nil
}

func pIntOr(p map[string]any, k string) int64 {
	n, _ := pInt(p, k)
	return n
}

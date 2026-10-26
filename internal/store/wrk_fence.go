package store

import (
	"context"
	"errors"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// Legacy fences (hk 1873 design §4 last part, §5 fence table, §6a S1 rows).
// One check, run inside each legacy mutation's own transaction after its
// row lock, decides every protected task and receipt_required job; HTTP,
// assistant, MCP and operator UI callers all reach it through the store.
// Unrelated tasks and jobs never match and keep today's behaviour.

var (
	// ErrWrkReceiptRequired refuses a protected legacy mutation that lacks
	// a matching canonical receipt.
	ErrWrkReceiptRequired = errors.New("wrk_receipt_required")
	// ErrWrkScopeDenied refuses a protected legacy mutation by a caller
	// without the O3 authority for it (including callers with no
	// bearer-derived principal, such as the operator UI).
	ErrWrkScopeDenied = errors.New("scope_denied")
)

type wrkPrincipalKey struct{}

// WithWrkPrincipal attaches the bearer-authenticated O3 principal to ctx.
// Only the authenticated HTTP layer sets it; a free "by" string never does.
func WithWrkPrincipal(ctx context.Context, p wrk.Principal) context.Context {
	return context.WithValue(ctx, wrkPrincipalKey{}, p)
}

func wrkPrincipal(ctx context.Context) (wrk.Principal, bool) {
	p, ok := ctx.Value(wrkPrincipalKey{}).(wrk.Principal)
	return p, ok && p.ClientID != "" && p.Policy != nil
}

// WrkReceipt is the design §5 legacy receipt tuple carried by a legacy
// relay or task transition request as field wrk_receipt.
type WrkReceipt struct {
	JobID           string `json:"job_id"`
	Seq             int64  `json:"seq"`
	RecordHash      string `json:"record_hash"`
	StoreGeneration string `json:"store_generation"`
}

type wrkQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// wrkJobProtected reports an O3 job a legacy write may not name as a task's
// job: one whose records make it receipt_required (or, without
// WrkProtectAtActivate, any journal job).
func wrkJobProtected(ctx context.Context, q wrkQuerier, jobID string) (bool, error) {
	if jobID == "" {
		return false, nil
	}
	if WrkProtectAtActivate {
		return wrkReceiptRequiredJob(ctx, q, jobID)
	}
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wrk_records WHERE job_id=$1)`, jobID).Scan(&ok)
	return ok, err
}

// wrkBinding is a protected task's current canonical subject.
type wrkBinding struct {
	JobID    string
	ClaimSeq int64
}

// WrkProtectAtActivate starts task protection at the bound subject's
// activate record, not at its shadow claim (operator answer dr-1925-1 = A).
// With false, any canonical builder/worker claim protects its task.
const WrkProtectAtActivate = true

// wrkTaskBinding returns the canonical builder/worker subject bound to
// taskID, or nil for an unprotected task. With a successor chain the live
// (unended) subject is current; with none live, the latest claimed one.
func wrkTaskBinding(ctx context.Context, q wrkQuerier, taskID int64) (*wrkBinding, error) {
	rows, err := q.Query(ctx, `WITH jobs AS (SELECT DISTINCT job_id FROM wrk_records WHERE kind='claim' AND (payload->>'task_id')::bigint=$1),
		latest AS (SELECT DISTINCT ON (r.job_id) r.job_id, r.seq, r.payload, r.received_at FROM wrk_records r JOIN jobs USING (job_id) WHERE r.kind='claim' ORDER BY r.job_id, r.seq DESC)
		SELECT l.job_id, l.seq, EXISTS(SELECT 1 FROM wrk_records e WHERE e.job_id=l.job_id AND e.kind='end') FROM latest l
		WHERE (l.payload->>'task_id')::bigint=$1 AND l.payload->>'role' IN ('builder','worker')
		AND (NOT $2 OR EXISTS(SELECT 1 FROM wrk_records a WHERE a.job_id=l.job_id AND a.kind='activate'))
		ORDER BY 3 ASC, l.received_at DESC, l.job_id DESC`, taskID, WrkProtectAtActivate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var b wrkBinding
	var ended bool
	if err := rows.Scan(&b.JobID, &b.ClaimSeq, &ended); err != nil {
		return nil, err
	}
	return &b, nil
}

// WrkTaskProtected reports whether a task is bound by a canonical claim.
// Callers that write a lane event before a task transition use it as a
// pre-check; the in-transaction fence remains the authority.
func (s *Store) WrkTaskProtected(ctx context.Context, taskID int64) (bool, error) {
	if !s.wrk {
		return false, nil
	}
	b, err := wrkTaskBinding(ctx, s.pool, taskID)
	return b != nil, err
}

// wrkFenceBinding refuses any legacy mutation of a protected task's job
// binding, claimant, parent, lane or project, for central and host callers
// alike, and any legacy write that names an O3 job as a task's job.
func (s *Store) wrkFenceBinding(ctx context.Context, q wrkQuerier, taskID int64, jobIDs ...string) error {
	if !s.wrk {
		return nil
	}
	if taskID > 0 {
		b, err := wrkTaskBinding(ctx, q, taskID)
		if err != nil {
			return err
		}
		if b != nil {
			return ErrWrkReceiptRequired
		}
	}
	for _, job := range jobIDs {
		ok, err := wrkJobProtected(ctx, q, job)
		if err != nil {
			return err
		}
		if ok {
			return ErrWrkReceiptRequired
		}
	}
	return nil
}

// wrkFenceState decides a state-only legacy transition of taskID to to.
// Protected tasks need a central principal with task.state and a receipt
// naming the current bound subject: its current claim for a nonterminal
// state, its canonical joined for join/merged, its canonical end for
// dropped. A receipt of another job sharing the task never qualifies.
func (s *Store) wrkFenceState(ctx context.Context, q wrkQuerier, taskID int64, to string, receipt *WrkReceipt) error {
	if !s.wrk {
		return nil
	}
	b, err := wrkTaskBinding(ctx, q, taskID)
	if err != nil || b == nil {
		return err
	}
	p, ok := wrkPrincipal(ctx)
	if !ok {
		return ErrWrkScopeDenied
	}
	if receipt == nil {
		return ErrWrkReceiptRequired
	}
	if !p.Has(wrk.PermTaskState) {
		// A host's terminal-state change is a receipt failure (design §4
		// L137, director-1 esc-2 #12); other host state changes are denied.
		if isTerminalTaskState(to) {
			return ErrWrkReceiptRequired
		}
		return ErrWrkScopeDenied
	}
	if receipt.JobID != b.JobID {
		return ErrWrkReceiptRequired
	}
	rec, found, err := wrkRecordAt(ctx, q, receipt.JobID, receipt.Seq)
	if err != nil {
		return err
	}
	if !found || rec.RecordHash != receipt.RecordHash || rec.StoreGeneration != receipt.StoreGeneration {
		return ErrWrkReceiptRequired
	}
	if ok, err := wrkReceiptPhaseOK(ctx, q, rec); err != nil || !ok {
		return firstErr(err, ErrWrkReceiptRequired)
	}
	switch to {
	case "join", "merged":
		if rec.Kind != "joined" {
			return ErrWrkReceiptRequired
		}
	case "dropped":
		if rec.Kind != "end" {
			return ErrWrkReceiptRequired
		}
	default:
		if rec.Kind != "claim" || rec.Seq != b.ClaimSeq {
			return ErrWrkReceiptRequired
		}
	}
	return nil
}

func wrkRecordAt(ctx context.Context, q wrkQuerier, jobID string, seq int64) (wrk.Record, bool, error) {
	rec, err := scanWrkRecord(q.QueryRow(ctx, `SELECT `+wrkRecordColumns+` FROM wrk_records WHERE job_id=$1 AND seq=$2`, jobID, seq))
	if errors.Is(err, pgx.ErrNoRows) {
		return wrk.Record{}, false, nil
	}
	return rec, err == nil, err
}

// wrkReceiptRequiredJob is the §2 predicate from canonical records: an
// activate record, or any review-role claim with review_of.
func wrkReceiptRequiredJob(ctx context.Context, q wrkQuerier, jobID string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wrk_records WHERE job_id=$1 AND (kind='activate' OR (kind='claim' AND payload->>'role' IN ('tester','reviewer','counsel') AND jsonb_typeof(payload->'review_of')='object')))`, jobID).Scan(&ok)
	return ok, err
}

// wrkTerminalRelayKinds maps fenced legacy relay kinds to the canonical
// record kind their receipt must name: completed, joined and the end-type
// kinds the legacy relay carries (director-1 esc-2 #11). job.lost stays a
// notification and is not fenced.
var wrkTerminalRelayKinds = map[string]string{"job.completed": "completed", "job.joined": "joined", "job.revoked": "end"}

// wrkFenceRelay decides a legacy terminal relay publication. It returns the
// existing canonical projection when the receipt names a completed/joined
// record (legacy retries deduplicate to it), or nil to insert normally.
func (s *Store) wrkFenceRelay(ctx context.Context, q wrkQuerier, x RelayEvent) (*RelayEvent, error) {
	want, terminal := wrkTerminalRelayKinds[x.Kind]
	if !terminal {
		return nil, nil
	}
	required, err := wrkReceiptRequiredJob(ctx, q, x.JobID)
	if err != nil || !required {
		return nil, err
	}
	if x.WrkReceipt == nil {
		return nil, ErrWrkReceiptRequired
	}
	p, ok := wrkPrincipal(ctx)
	if !ok || !p.Has(wrk.PermRelayPublish) {
		return nil, ErrWrkScopeDenied
	}
	var host string
	if err := q.QueryRow(ctx, `SELECT payload->>'host_client_id' FROM wrk_records WHERE job_id=$1 AND kind='claim' ORDER BY seq DESC LIMIT 1`, x.JobID).Scan(&host); err != nil {
		return nil, err
	}
	if p.Host() == "" || p.Host() != host {
		return nil, ErrWrkScopeDenied
	}
	r := x.WrkReceipt
	rec, found, err := wrkRecordAt(ctx, q, r.JobID, r.Seq)
	if err != nil {
		return nil, err
	}
	if !found || r.JobID != x.JobID || rec.Kind != want || rec.RecordHash != r.RecordHash || rec.StoreGeneration != r.StoreGeneration {
		return nil, ErrWrkReceiptRequired
	}
	if ok, err := wrkReceiptPhaseOK(ctx, q, rec); err != nil || !ok {
		return nil, firstErr(err, ErrWrkReceiptRequired)
	}
	if head, _ := rec.Payload["head"].(string); rec.Kind == "joined" && head != x.Head {
		return nil, ErrWrkReceiptRequired
	}
	if digest, _ := rec.Payload["report_sha256"].(string); (rec.Kind == "completed" || rec.Kind == "joined") && digest != x.ReportSHA256 {
		return nil, ErrWrkReceiptRequired
	}
	if rec.Kind == "end" {
		return nil, nil
	}
	var out RelayEvent
	err = scanRelayEvent(q.QueryRow(ctx, `SELECT `+relayEventColumns+` FROM relay_events WHERE kind=$1 AND job_id=$2 AND reason=$3`, x.Kind, x.JobID, WrkProjectionReason(r.Seq)), &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWrkReceiptRequired
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func firstErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

// wrkReceiptPhaseOK applies design L3962 to a completed or joined receipt:
// for a builder/worker subject, preactivation writes are observations only,
// so the record must follow the subject's activate record. A review-role
// job's own completed is receipt-required from its claim and always
// eligible. Other kinds pass through.
func wrkReceiptPhaseOK(ctx context.Context, q wrkQuerier, rec wrk.Record) (bool, error) {
	if rec.Kind != "completed" && rec.Kind != "joined" {
		return true, nil
	}
	var role string
	var activateSeq *int64
	err := q.QueryRow(ctx, `SELECT (SELECT payload->>'role' FROM wrk_records WHERE job_id=$1 AND kind='claim' ORDER BY seq DESC LIMIT 1),
		(SELECT seq FROM wrk_records WHERE job_id=$1 AND kind='activate' LIMIT 1)`, rec.JobID).Scan(&role, &activateSeq)
	if err != nil {
		return false, err
	}
	if role == "tester" || role == "reviewer" || role == "counsel" {
		return true, nil
	}
	return activateSeq != nil && rec.Seq > *activateSeq, nil
}

var wrkProjectionReasonRE = regexp.MustCompile(`^o3 receipt seq ([1-9][0-9]{0,15})$`)

// hydrateWrkReceipts fills wrk_receipt and report_sha256 on canonical relay
// projections (design L137, L4142; director-1 projection ruling). A row is
// hydrated only when its canonical record exists, is the same kind, and
// matches it exactly on job, head, PR, owner lane and phase; anything else
// leaves the row without a receipt (consumers fail closed) and never drops
// it. Errors are swallowed for the same reason.
func (s *Store) hydrateWrkReceipts(ctx context.Context, xs []RelayEvent) {
	for i := range xs {
		x := &xs[i]
		x.WrkReceipt, x.ReportSHA256 = nil, ""
		if !s.wrk {
			continue
		}
		want, ok := map[string]string{"job.completed": "completed", "job.joined": "joined"}[x.Kind]
		m := wrkProjectionReasonRE.FindStringSubmatch(x.Reason)
		if !ok || m == nil || x.Epoch != 0 || x.ReportPath != "" {
			continue
		}
		seq, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		rec, found, err := wrkRecordAt(ctx, s.pool, x.JobID, seq)
		if err != nil || !found || rec.Kind != want {
			continue
		}
		head, _ := rec.Payload["head"].(string)
		pr, _ := rec.Payload["pr"].(string)
		digest, _ := rec.Payload["report_sha256"].(string)
		var owner string
		if err := s.pool.QueryRow(ctx, `SELECT payload->>'owner_lane' FROM wrk_records WHERE job_id=$1 AND kind='claim' ORDER BY seq DESC LIMIT 1`, x.JobID).Scan(&owner); err != nil {
			continue
		}
		if head != x.Head || pr != x.PR || owner != x.OwnerLane || x.EventID != "wrk:"+x.JobID+":"+m[1] {
			continue
		}
		if ok, err := wrkReceiptPhaseOK(ctx, s.pool, rec); err != nil || !ok {
			continue
		}
		x.WrkReceipt = &WrkReceipt{JobID: rec.JobID, Seq: rec.Seq, RecordHash: rec.RecordHash, StoreGeneration: rec.StoreGeneration}
		x.ReportSHA256 = digest
	}
}

func (s *Store) hydrateWrkReceipt(ctx context.Context, x RelayEvent) RelayEvent {
	xs := []RelayEvent{x}
	s.hydrateWrkReceipts(ctx, xs)
	return xs[0]
}

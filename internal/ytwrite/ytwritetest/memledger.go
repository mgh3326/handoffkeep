// Package ytwritetest is the test support for internal/ytwrite: a fake YouTrack
// that fails the ways the real one can, an in-memory Ledger, a crash injector,
// and the contract suites that every Ledger and the protocol must pass. The
// Postgres Ledger in internal/store runs the same suites, so the in-memory
// Ledger is only a second implementation of one contract, not a second
// definition of it.
package ytwritetest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// MemLedger is an in-memory ytwrite.Ledger.
type MemLedger struct {
	// Hold, when set, is the ledger's admission: Claim answers ClaimHeld for
	// a prepared op it returns true for, and Due does not list that op. It
	// runs under the ledger's lock (it must not call the ledger); set it
	// before the ledger is shared.
	Hold func(op ytwrite.Op) bool

	mu     sync.Mutex
	nextID int64
	ops    map[int64]*ytwrite.Op
	byKey  map[[3]string]int64
	maps   map[[2]string]ytwrite.MapRow
	mapIDs map[[2]string]string // (entity kind, yt id) -> hk key
	adopts map[int64]ytwrite.Adoption
	// links is the issue_links record (memledger_links.go).
	links memLinks
}

// NewMemLedger returns an empty ledger.
func NewMemLedger() *MemLedger {
	return &MemLedger{
		ops:    map[int64]*ytwrite.Op{},
		byKey:  map[[3]string]int64{},
		maps:   map[[2]string]ytwrite.MapRow{},
		mapIDs: map[[2]string]string{},
		adopts: map[int64]ytwrite.Adoption{},
	}
}

func (l *MemLedger) Prepare(_ context.Context, now time.Time, n ytwrite.NewOp) (ytwrite.Op, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := [3]string{n.Principal, n.Kind, n.Key}
	if id, ok := l.byKey[k]; ok {
		op := l.ops[id]
		if op.PayloadSHA != n.PayloadSHA {
			return ytwrite.Op{}, false, ytwrite.ErrKeyConflict
		}
		return *op, false, nil
	}
	l.nextID++
	op := &ytwrite.Op{
		ID: l.nextID, Principal: n.Principal, Kind: n.Kind, Key: n.Key,
		PayloadSHA: n.PayloadSHA, Payload: append([]byte(nil), n.Payload...),
		EntityKind: n.EntityKind, EntityID: n.EntityID, YTTargetID: n.YTTargetID, Tenant: n.Tenant,
		Actor: n.Actor, ParentID: n.ParentID, ChildSeq: n.ChildSeq,
		State: ytwrite.StatePrepared, CreatedAt: now, NextAttemptAt: now,
	}
	l.ops[op.ID] = op
	l.byKey[k] = op.ID
	return *op, true, nil
}

func (l *MemLedger) Get(_ context.Context, principal string, id int64) (ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, ok := l.ops[id]
	if !ok || op.Principal != principal {
		return ytwrite.Op{}, ytwrite.ErrNotFound
	}
	return *op, nil
}

func (l *MemLedger) ByID(_ context.Context, id int64) (ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, ok := l.ops[id]
	if !ok {
		return ytwrite.Op{}, ytwrite.ErrNotFound
	}
	return *op, nil
}

func (l *MemLedger) get(id int64) (*ytwrite.Op, error) {
	op, ok := l.ops[id]
	if !ok {
		return nil, ytwrite.ErrNotFound
	}
	return op, nil
}

func (l *MemLedger) Claim(_ context.Context, now time.Time, id int64) (ytwrite.Op, ytwrite.ClaimOutcome, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return ytwrite.Op{}, "", err
	}
	if op.State != ytwrite.StatePrepared {
		return *op, ytwrite.ClaimNotPrepared, nil
	}
	if op.NextAttemptAt.After(now) {
		return *op, ytwrite.ClaimNotDue, nil
	}
	if l.Hold != nil && l.Hold(*op) {
		return *op, ytwrite.ClaimHeld, nil
	}
	for _, e := range l.ops {
		if e.ID < op.ID && e.EntityKind == op.EntityKind && e.EntityID == op.EntityID && !e.State.Resolved() {
			return *op, ytwrite.ClaimBlocked, nil
		}
	}
	op.State = ytwrite.StateDispatched
	op.Attempts++
	op.DispatchedAt = now
	return *op, ytwrite.ClaimClaimed, nil
}

func (l *MemLedger) Apply(_ context.Context, now time.Time, id int64, ytID string, maps []ytwrite.MapRow) (ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	switch op.State {
	case ytwrite.StateApplied:
		if op.YTResultID == ytID {
			return *op, nil
		}
		return ytwrite.Op{}, ytwrite.ErrConflictingResult
	case ytwrite.StateDispatched, ytwrite.StateRemoteUnknown:
	default:
		return ytwrite.Op{}, fmt.Errorf("%w: apply from %s", ytwrite.ErrState, op.State)
	}
	// Check every map row before writing any, so a conflict commits nothing.
	for _, m := range maps {
		if have, ok := l.maps[[2]string{m.EntityKind, m.HKKey}]; ok && have.YTID != m.YTID {
			return ytwrite.Op{}, ytwrite.ErrConflictingResult
		}
		if hk, ok := l.mapIDs[[2]string{m.EntityKind, m.YTID}]; ok && hk != m.HKKey {
			return ytwrite.Op{}, ytwrite.ErrConflictingResult
		}
	}
	for _, m := range maps {
		m.OpID = id
		l.maps[[2]string{m.EntityKind, m.HKKey}] = m
		l.mapIDs[[2]string{m.EntityKind, m.YTID}] = m.HKKey
	}
	op.State, op.YTResultID, op.Reason, op.LastError, op.ResolvedAt = ytwrite.StateApplied, ytID, "", "", now
	return *op, nil
}

func (l *MemLedger) Reject(_ context.Context, now time.Time, id int64, reason, detail string) (ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	if op.State.Resolved() {
		return ytwrite.Op{}, fmt.Errorf("%w: reject from %s", ytwrite.ErrState, op.State)
	}
	op.State, op.Reason, op.LastError, op.ResolvedAt = ytwrite.StateRejected, reason, detail, now
	return *op, nil
}

func (l *MemLedger) Defer(_ context.Context, _ time.Time, id int64, next time.Time, reason, detail string) (ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	if op.State != ytwrite.StateDispatched {
		return ytwrite.Op{}, fmt.Errorf("%w: defer from %s", ytwrite.ErrState, op.State)
	}
	op.State, op.NextAttemptAt, op.Reason, op.LastError = ytwrite.StatePrepared, next, reason, detail
	return *op, nil
}

func (l *MemLedger) MarkUnknown(_ context.Context, _ time.Time, id int64, next time.Time, reason, detail string) (ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	switch op.State {
	case ytwrite.StateRemoteUnknown:
		return *op, nil
	case ytwrite.StateDispatched:
	default:
		return ytwrite.Op{}, fmt.Errorf("%w: mark unknown from %s", ytwrite.ErrState, op.State)
	}
	op.State, op.NextAttemptAt, op.Reason, op.LastError = ytwrite.StateRemoteUnknown, next, reason, detail
	return *op, nil
}

func (l *MemLedger) RecoverStale(_ context.Context, id int64, attempts int, cutoff, next time.Time, detail string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return false, err
	}
	if op.State != ytwrite.StateDispatched || op.Attempts != attempts || op.DispatchedAt.After(cutoff) {
		return false, nil
	}
	op.State, op.NextAttemptAt, op.Reason, op.LastError = ytwrite.StateRemoteUnknown, next, ytwrite.ReasonDispatcherLost, detail
	return true, nil
}

func (l *MemLedger) TouchUnknown(_ context.Context, _ time.Time, id int64, next time.Time, reason, detail string) (ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	if op.State != ytwrite.StateRemoteUnknown {
		return ytwrite.Op{}, fmt.Errorf("%w: touch unknown from %s", ytwrite.ErrState, op.State)
	}
	op.Reconciles++
	op.NextAttemptAt, op.Reason, op.LastError = next, reason, detail
	return *op, nil
}

func (l *MemLedger) list(keep func(*ytwrite.Op) bool, limit int) []ytwrite.Op {
	var out []ytwrite.Op
	for _, op := range l.ops {
		if keep(op) {
			out = append(out, *op)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (l *MemLedger) Due(_ context.Context, now time.Time, limit int) ([]ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.list(func(o *ytwrite.Op) bool {
		if o.NextAttemptAt.After(now) {
			return false
		}
		switch o.State {
		case ytwrite.StateRemoteUnknown:
			return true
		case ytwrite.StatePrepared:
			if l.Hold != nil && l.Hold(*o) {
				return false
			}
			for _, e := range l.ops {
				if e.ID < o.ID && e.EntityKind == o.EntityKind && e.EntityID == o.EntityID && !e.State.Resolved() {
					return false
				}
			}
			return true
		}
		return false
	}, limit), nil
}

func (l *MemLedger) Stale(_ context.Context, cutoff time.Time, limit int) ([]ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.list(func(o *ytwrite.Op) bool {
		return o.State == ytwrite.StateDispatched && !o.DispatchedAt.After(cutoff)
	}, limit), nil
}

func (l *MemLedger) ListState(_ context.Context, state ytwrite.State, limit int) ([]ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.list(func(o *ytwrite.Op) bool { return o.State == state }, limit), nil
}

func (l *MemLedger) MapGet(_ context.Context, entityKind, hkKey string) (ytwrite.MapRow, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.maps[[2]string{entityKind, hkKey}]
	return m, ok, nil
}

func (l *MemLedger) RecordAdoption(_ context.Context, _ time.Time, id int64, attempts int, ytID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return err
	}
	a := ytwrite.Adoption{Attempts: attempts, YTID: ytID}
	if have, ok := l.adopts[id]; ok {
		if have != a {
			return ytwrite.ErrConflictingResult
		}
		return nil
	}
	if op.State != ytwrite.StateDispatched || op.Attempts != attempts {
		return fmt.Errorf("%w: record adoption of attempt %d from %s attempt %d", ytwrite.ErrState, attempts, op.State, op.Attempts)
	}
	l.adopts[id] = a
	return nil
}

func (l *MemLedger) Adoption(_ context.Context, id int64) (ytwrite.Adoption, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.adopts[id]
	return a, ok, nil
}

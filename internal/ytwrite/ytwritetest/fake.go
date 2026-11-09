package ytwritetest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// FaultKind is one way the fake YouTrack fails a CreateComment call.
type FaultKind int

const (
	// CommitDropReply commits the comment, then loses the reply (the caller
	// sees a connection error). The write exists; the caller cannot know.
	CommitDropReply FaultKind = iota + 1
	// DelayCommit loses the reply now and commits the comment only when
	// ReleaseDelayed is called: a commit that lands after reconciliation has
	// already searched and found nothing.
	DelayCommit
	// NoID commits the comment and answers 2xx with an empty id.
	NoID
	// RateLimit answers 429 with Retry-After and does not commit.
	RateLimit
	// ServerError answers 5xx. With Commit the comment was committed first.
	ServerError
	// Refuse answers a 4xx (Status) and does not commit.
	Refuse
)

// Fault is one queued failure; the fake takes one per CreateComment call, in
// order, and behaves normally once the queue is empty.
type Fault struct {
	Kind       FaultKind
	Status     int
	RetryAfter time.Duration
	Commit     bool
}

// FakeYouTrack is an in-memory YouTrack comment store with the failure modes
// the protocol must survive. It implements ytwrite.Remote.
type FakeYouTrack struct {
	mu         sync.Mutex
	faults     []Fault
	issues     map[string][]*ytwrite.RemoteComment
	nextID     int
	calls      int
	commits    int
	delayed    []delayedComment
	pageCap    int
	listFail   int
	shiftPages bool
	latency    time.Duration

	// BeforeCreate runs at the start of every CreateComment, with the caller's
	// context, before anything else. Tests use it to cancel a caller mid-call.
	BeforeCreate func(ctx context.Context)
}

type delayedComment struct{ issue, text string }

// NewFake returns an empty fake with no faults queued.
func NewFake() *FakeYouTrack { return &FakeYouTrack{issues: map[string][]*ytwrite.RemoteComment{}} }

// Queue adds faults for the next CreateComment calls.
func (f *FakeYouTrack) Queue(faults ...Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, faults...)
}

// SetPageCap makes the server return at most n comments per page whatever the
// client asks for (0 = no cap).
func (f *FakeYouTrack) SetPageCap(n int) { f.mu.Lock(); f.pageCap = n; f.mu.Unlock() }

// ShiftPages inserts a comment after each first page, simulating offset shifts.
func (f *FakeYouTrack) ShiftPages() { f.mu.Lock(); defer f.mu.Unlock(); f.shiftPages = true }

// FailLists makes the next n ListComments calls fail.
func (f *FakeYouTrack) FailLists(n int) { f.mu.Lock(); f.listFail = n; f.mu.Unlock() }

// SetLatency makes every CreateComment take d, to widen race windows.
func (f *FakeYouTrack) SetLatency(d time.Duration) { f.mu.Lock(); f.latency = d; f.mu.Unlock() }

// Calls is the number of CreateComment calls received, Commits the number of
// comments that actually exist because of them.
func (f *FakeYouTrack) Calls() int   { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }
func (f *FakeYouTrack) Commits() int { f.mu.Lock(); defer f.mu.Unlock(); return f.commits }

// Comments returns a copy of the issue's comments, oldest first.
func (f *FakeYouTrack) Comments(issue string) []ytwrite.RemoteComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ytwrite.RemoteComment, 0, len(f.issues[issue]))
	for _, c := range f.issues[issue] {
		out = append(out, *c)
	}
	return out
}

// Inject adds a comment hk did not write and returns its id.
func (f *FakeYouTrack) Inject(issue, text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addLocked(issue, text)
}

// Duplicate adds a second comment with the same text as comment id.
func (f *FakeYouTrack) Duplicate(issue, id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.issues[issue] {
		if c.ID == id {
			return f.addLocked(issue, c.Text)
		}
	}
	panic("ytwritetest: duplicate of unknown comment " + id)
}

// Delete marks comment id deleted.
func (f *FakeYouTrack) Delete(issue, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.issues[issue] {
		if c.ID == id {
			c.Deleted = true
			return
		}
	}
	panic("ytwritetest: delete of unknown comment " + id)
}

// ReleaseDelayed commits every comment a DelayCommit fault held back.
func (f *FakeYouTrack) ReleaseDelayed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.delayed {
		f.addLocked(d.issue, d.text)
		f.commits++
	}
	f.delayed = nil
}

func (f *FakeYouTrack) addLocked(issue, text string) string {
	f.nextID++
	id := fmt.Sprintf("c-%d", f.nextID)
	f.issues[issue] = append(f.issues[issue], &ytwrite.RemoteComment{ID: id, Text: text})
	return id
}

var errConnReset = errors.New("read tcp: connection reset by peer")

func (f *FakeYouTrack) CreateComment(ctx context.Context, issueID, text string) (string, error) {
	if f.BeforeCreate != nil {
		f.BeforeCreate(ctx)
	}
	f.mu.Lock()
	latency := f.latency
	f.mu.Unlock()
	if latency > 0 {
		time.Sleep(latency)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var fault Fault
	if len(f.faults) > 0 {
		fault, f.faults = f.faults[0], f.faults[1:]
	}
	commit := func() string { f.commits++; return f.addLocked(issueID, text) }
	switch fault.Kind {
	case CommitDropReply:
		commit()
		return "", errConnReset
	case DelayCommit:
		f.delayed = append(f.delayed, delayedComment{issueID, text})
		return "", context.DeadlineExceeded
	case NoID:
		commit()
		return "", nil
	case RateLimit:
		return "", &ytwrite.RemoteError{Status: http.StatusTooManyRequests, RetryAfter: fault.RetryAfter, Message: "rate limited"}
	case ServerError:
		if fault.Commit {
			commit()
		}
		st := fault.Status
		if st == 0 {
			st = http.StatusBadGateway
		}
		return "", &ytwrite.RemoteError{Status: st, Message: "server error"}
	case Refuse:
		return "", &ytwrite.RemoteError{Status: fault.Status, Message: "refused"}
	}
	return commit(), nil
}

func (f *FakeYouTrack) ListComments(ctx context.Context, issueID string, top int) ([]ytwrite.RemoteComment, error) {
	return ytwrite.EnumerateStableComments(ctx, top, func(ctx context.Context, skip, top int) ([]ytwrite.RemoteComment, error) {
		return f.listPage(ctx, issueID, skip, top)
	})
}

func (f *FakeYouTrack) listPage(ctx context.Context, issueID string, skip, top int) ([]ytwrite.RemoteComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.listFail > 0 {
		f.listFail--
		return nil, errConnReset
	}
	all := f.issues[issueID]
	if f.pageCap > 0 && top > f.pageCap {
		top = f.pageCap
	}
	if skip >= len(all) {
		return nil, nil
	}
	end := min(len(all), skip+top)
	out := make([]ytwrite.RemoteComment, 0, end-skip)
	for _, c := range all[skip:end] {
		out = append(out, *c)
	}
	if f.shiftPages && skip == 0 && end < len(all) {
		f.nextID++
		c := &ytwrite.RemoteComment{ID: fmt.Sprintf("shift-%d", f.nextID), Text: "inserted"}
		f.issues[issueID] = append([]*ytwrite.RemoteComment{c}, f.issues[issueID]...)
	}
	return out, nil
}

// Clock is a manually advanced clock shared by a Protocol and its test.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at a fixed instant.
func NewClock() *Clock { return &Clock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)} }

func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// ErrCrash is what a crashed process's ledger calls return.
var ErrCrash = errors.New("ytwritetest: process crashed")

// CrashLedger wraps a Ledger and kills "the process" at one method: the first
// call to Method either never reaches the ledger (After false: the transaction
// did not commit) or reaches it and then reports ErrCrash (After true: it
// committed and the caller never learned). Every later call also fails, as in a
// dead process. A restart builds a new Protocol over the wrapped ledger.
type CrashLedger struct {
	ytwrite.Ledger
	Method string
	After  bool
	mu     sync.Mutex
	dead   bool
}

// Dead reports whether the crash has happened.
func (c *CrashLedger) Dead() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.dead }

func (c *CrashLedger) enter(m string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return ErrCrash
	}
	if m == c.Method && !c.After {
		c.dead = true
		return ErrCrash
	}
	return nil
}

func (c *CrashLedger) leave(m string, err error) error {
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if m == c.Method && c.After {
		c.dead = true
		return ErrCrash
	}
	return nil
}

func (c *CrashLedger) Prepare(ctx context.Context, now time.Time, n ytwrite.NewOp) (ytwrite.Op, bool, error) {
	if err := c.enter("Prepare"); err != nil {
		return ytwrite.Op{}, false, err
	}
	op, created, err := c.Ledger.Prepare(ctx, now, n)
	if err := c.leave("Prepare", err); err != nil {
		return ytwrite.Op{}, false, err
	}
	return op, created, nil
}

func (c *CrashLedger) Get(ctx context.Context, principal string, id int64) (ytwrite.Op, error) {
	if err := c.enter("Get"); err != nil {
		return ytwrite.Op{}, err
	}
	return c.Ledger.Get(ctx, principal, id)
}

func (c *CrashLedger) ByID(ctx context.Context, id int64) (ytwrite.Op, error) {
	if err := c.enter("ByID"); err != nil {
		return ytwrite.Op{}, err
	}
	return c.Ledger.ByID(ctx, id)
}

func (c *CrashLedger) Claim(ctx context.Context, now time.Time, id int64) (ytwrite.Op, ytwrite.ClaimOutcome, error) {
	if err := c.enter("Claim"); err != nil {
		return ytwrite.Op{}, "", err
	}
	op, out, err := c.Ledger.Claim(ctx, now, id)
	if err := c.leave("Claim", err); err != nil {
		return ytwrite.Op{}, "", err
	}
	return op, out, nil
}

func (c *CrashLedger) Apply(ctx context.Context, now time.Time, id int64, ytID string, maps []ytwrite.MapRow) (ytwrite.Op, error) {
	if err := c.enter("Apply"); err != nil {
		return ytwrite.Op{}, err
	}
	op, err := c.Ledger.Apply(ctx, now, id, ytID, maps)
	if err := c.leave("Apply", err); err != nil {
		return ytwrite.Op{}, err
	}
	return op, nil
}

func (c *CrashLedger) Reject(ctx context.Context, now time.Time, id int64, reason, detail string) (ytwrite.Op, error) {
	if err := c.enter("Reject"); err != nil {
		return ytwrite.Op{}, err
	}
	op, err := c.Ledger.Reject(ctx, now, id, reason, detail)
	if err := c.leave("Reject", err); err != nil {
		return ytwrite.Op{}, err
	}
	return op, nil
}

func (c *CrashLedger) Defer(ctx context.Context, now time.Time, id int64, next time.Time, reason, detail string) (ytwrite.Op, error) {
	if err := c.enter("Defer"); err != nil {
		return ytwrite.Op{}, err
	}
	op, err := c.Ledger.Defer(ctx, now, id, next, reason, detail)
	if err := c.leave("Defer", err); err != nil {
		return ytwrite.Op{}, err
	}
	return op, nil
}

func (c *CrashLedger) MarkUnknown(ctx context.Context, now time.Time, id int64, next time.Time, reason, detail string) (ytwrite.Op, error) {
	if err := c.enter("MarkUnknown"); err != nil {
		return ytwrite.Op{}, err
	}
	op, err := c.Ledger.MarkUnknown(ctx, now, id, next, reason, detail)
	if err := c.leave("MarkUnknown", err); err != nil {
		return ytwrite.Op{}, err
	}
	return op, nil
}

func (c *CrashLedger) TouchUnknown(ctx context.Context, now time.Time, id int64, next time.Time, reason, detail string) (ytwrite.Op, error) {
	if err := c.enter("TouchUnknown"); err != nil {
		return ytwrite.Op{}, err
	}
	op, err := c.Ledger.TouchUnknown(ctx, now, id, next, reason, detail)
	if err := c.leave("TouchUnknown", err); err != nil {
		return ytwrite.Op{}, err
	}
	return op, nil
}

func (c *CrashLedger) Due(ctx context.Context, now time.Time, limit int) ([]ytwrite.Op, error) {
	if err := c.enter("Due"); err != nil {
		return nil, err
	}
	return c.Ledger.Due(ctx, now, limit)
}

func (c *CrashLedger) Stale(ctx context.Context, cutoff time.Time, limit int) ([]ytwrite.Op, error) {
	if err := c.enter("Stale"); err != nil {
		return nil, err
	}
	return c.Ledger.Stale(ctx, cutoff, limit)
}

func (c *CrashLedger) ListState(ctx context.Context, state ytwrite.State, limit int) ([]ytwrite.Op, error) {
	if err := c.enter("ListState"); err != nil {
		return nil, err
	}
	return c.Ledger.ListState(ctx, state, limit)
}

func (c *CrashLedger) MapGet(ctx context.Context, entityKind, hkKey string) (ytwrite.MapRow, bool, error) {
	if err := c.enter("MapGet"); err != nil {
		return ytwrite.MapRow{}, false, err
	}
	return c.Ledger.MapGet(ctx, entityKind, hkKey)
}

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

// Fault is one queued failure; the fake takes one per create call
// (CreateComment or CreateIssue), in order, and behaves normally once the
// queue is empty.
type Fault struct {
	Kind       FaultKind
	Status     int
	RetryAfter time.Duration
	Commit     bool
}

// FakeYouTrack is an in-memory YouTrack comment and issue store with the
// failure modes the protocol must survive. It implements ytwrite.Remote and
// ytwrite.IssueRemote.
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
	// projects holds the issues by YouTrack project id; findFaults are the
	// errors the next FindTaskIssues calls return, in order.
	projects   map[string][]*fakeIssue
	findFaults []error
	finds      int
	// fieldsMissing names the projects whose issue fields are not set up;
	// projectText those whose hk project field is text.
	fieldsMissing map[string]bool
	projectText   map[string]bool
	created       []ytwrite.IssueCreate
	// bundles are the values the named enum or state fields accept (a
	// field not named takes any value); updates every IssueFieldsUpdate
	// received; readFaults the errors the next ReadIssueFields calls
	// return (fake_fields.go).
	bundles    map[string]map[string]bool
	updates    []ytwrite.IssueFieldsUpdate
	reads      int
	readFaults []error

	// BeforeCreate runs at the start of every CreateComment, with the caller's
	// context, before anything else. Tests use it to cancel a caller mid-call.
	BeforeCreate func(ctx context.Context)
}

type delayedComment struct{ issue, text string }

type fakeIssue struct {
	ytwrite.RemoteIssue
	hkID int64
	// fields are the issue's field values, by name (no entry: empty).
	fields map[string]string
}

// NewFake returns an empty fake with no faults queued.
func NewFake() *FakeYouTrack {
	return &FakeYouTrack{issues: map[string][]*ytwrite.RemoteComment{}, projects: map[string][]*fakeIssue{}, fieldsMissing: map[string]bool{}, projectText: map[string]bool{}, bundles: map[string]map[string]bool{}}
}

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

// Calls is the number of create calls (CreateComment, CreateIssue) received,
// Commits the number of comments or issues that actually exist because of
// them.
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

// InjectAs adds a comment written by the YouTrack user authorID and returns
// its id.
func (f *FakeYouTrack) InjectAs(issue, text, authorID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.addLocked(issue, text)
	cs := f.issues[issue]
	cs[len(cs)-1].AuthorID = authorID
	return id
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

func (c *CrashLedger) RecoverStale(ctx context.Context, id int64, attempts int, cutoff, next time.Time, detail string) (bool, error) {
	if err := c.enter("RecoverStale"); err != nil {
		return false, err
	}
	changed, err := c.Ledger.RecoverStale(ctx, id, attempts, cutoff, next, detail)
	return changed, c.leave("RecoverStale", err)
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

// InjectIssue adds an issue hk did not create (or created earlier) to the
// project, carrying hk ID taskID, and returns its id.
func (f *FakeYouTrack) InjectIssue(projectID string, taskID int64, summary, description string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addIssueLocked(projectID, taskID, summary, description)
}

// Issues returns a copy of the project's issues, oldest first.
func (f *FakeYouTrack) Issues(projectID string) []ytwrite.RemoteIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ytwrite.RemoteIssue, 0, len(f.projects[projectID]))
	for _, i := range f.projects[projectID] {
		out = append(out, i.RemoteIssue)
	}
	return out
}

// QueueFind makes the next FindTaskIssues calls return errs, in order.
func (f *FakeYouTrack) QueueFind(errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findFaults = append(f.findFaults, errs...)
}

// Finds is the number of FindTaskIssues calls received.
func (f *FakeYouTrack) Finds() int { f.mu.Lock(); defer f.mu.Unlock(); return f.finds }

func (f *FakeYouTrack) addIssueLocked(projectID string, taskID int64, summary, description string) string {
	f.nextID++
	id := fmt.Sprintf("2-%d", f.nextID)
	f.projects[projectID] = append(f.projects[projectID], &fakeIssue{
		RemoteIssue: ytwrite.RemoteIssue{ID: id, Readable: fmt.Sprintf("HK-%d", f.nextID), ProjectID: projectID, Summary: summary, Description: description},
		hkID:        taskID,
	})
	return id
}

// FindTaskIssues returns every issue of the project carrying hk ID taskID.
// The short name is not checked: the fake keys issues by project id.
func (f *FakeYouTrack) FindTaskIssues(ctx context.Context, _, projectID string, taskID int64) ([]ytwrite.RemoteIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(f.findFaults) > 0 {
		err := f.findFaults[0]
		f.findFaults = f.findFaults[1:]
		return nil, err
	}
	out := []ytwrite.RemoteIssue{}
	for _, i := range f.projects[projectID] {
		if i.hkID == taskID {
			out = append(out, i.RemoteIssue)
		}
	}
	return out, nil
}

// CreateIssue creates one issue, failing as the next queued Fault says (as
// CreateComment does).
func (f *FakeYouTrack) CreateIssue(ctx context.Context, in ytwrite.IssueCreate) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.created = append(f.created, in)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var fault Fault
	if len(f.faults) > 0 {
		fault, f.faults = f.faults[0], f.faults[1:]
	}
	commit := func() string {
		f.commits++
		return f.addIssueLocked(in.ProjectID, in.TaskID, in.Summary, in.Description)
	}
	switch fault.Kind {
	case CommitDropReply:
		commit()
		return "", errConnReset
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
	case DelayCommit:
		panic("ytwritetest: DelayCommit is not supported for issues")
	}
	return commit(), nil
}

// SetIssueFieldsMissing makes CheckIssueFields refuse the project (missing
// true) or accept it again.
func (f *FakeYouTrack) SetIssueFieldsMissing(projectID string, missing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fieldsMissing[projectID] = missing
}

// SetProjectFieldText makes the project's hk project field a text field.
func (f *FakeYouTrack) SetProjectFieldText(projectID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projectText[projectID] = true
}

// Created returns every IssueCreate the fake received, in order.
func (f *FakeYouTrack) Created() []ytwrite.IssueCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ytwrite.IssueCreate(nil), f.created...)
}

// CheckIssueFields refuses a project marked by SetIssueFieldsMissing.
func (f *FakeYouTrack) CheckIssueFields(ctx context.Context, projectID, _ string) (ytwrite.IssueFields, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ytwrite.IssueFields{}, err
	}
	if f.fieldsMissing[projectID] {
		return ytwrite.IssueFields{}, fmt.Errorf("project %s has no hk ID field", projectID)
	}
	return ytwrite.IssueFields{ProjectText: f.projectText[projectID]}, nil
}

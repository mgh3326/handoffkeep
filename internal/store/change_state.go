package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// PR state in hk (hk 3176, hk 3198; hk:doc
// research/2026-11-06/hk3176-pr-state-contract §0-§3 and §6, which supersedes
// design v1.1 §0-1 for findings and threads). mergewake writes five sections
// of a pull request's state: state, checks, threads, findings and gate. Each
// section carries the time it was observed and where it came from, and a
// write older than the stored one changes nothing and answers "stale". The
// same write twice changes nothing. hk makes no GitHub call.

// ErrInvalidChangeWrite refuses a PR-state write or read. The wire code is
// its text, and the text after "invalid_context: " is store-authored (never
// echoed input), so the API returns it as the reason.
var ErrInvalidChangeWrite = errors.New("invalid_context")

func invalidChangeWrite(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidChangeWrite, fmt.Sprintf(format, a...))
}

// Section names, in the order a batch applies them.
const (
	SectionState    = "state"
	SectionChecks   = "checks"
	SectionThreads  = "threads"
	SectionFindings = "findings"
	SectionGate     = "gate"
)

// Caps.
const (
	MaxThreadExcerpt  = 2000    // characters
	MaxFindingBody    = 8 << 10 // bytes
	maxChangeTitle    = 512     // bytes
	MaxFindingSummary = 200     // characters
	maxChangeChecks   = 500     // per write
	maxChangeThreads  = 300     // per write
	maxChangeFinds    = 200     // per write
	maxGateReasons    = 32      // per write
	observedSkew      = 5 * time.Minute
	staleReason       = "stale"
	defaultFindingSrc = "review"
)

var (
	changeRepoRE     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	changeSources    = map[string]bool{"webhook": true, "poll": true, "fresh": true, "review": true}
	changeWordRE     = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	changeCodeRE     = regexp.MustCompile(`^[a-z0-9_.:-]{1,64}$`)
	changeLensRE     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
	changeRevisionRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	changeStates     = map[string]bool{ChangeOpen: true, ChangeMerged: true, ChangeClosed: true}
	gateStates       = map[string]bool{"ready": true, "blocked": true, "pending": true}
	severities       = map[string]bool{"critical": true, "high": true, "medium": true, "low": true}
	threadAuthors    = map[string]bool{"mergewake": true, "bot": true, "human": true}
)

// ChangeRef names a pull request: the repo as owner/name and the number.
type ChangeRef struct {
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
}

// ChangeStateWrite is the scalar state of a PR. An omitted optional field
// keeps the stored value; merged is terminal.
type ChangeStateWrite struct {
	HeadSHA    string     `json:"head_sha"`
	BaseSHA    string     `json:"base_sha,omitempty"`
	BaseRef    string     `json:"base_ref,omitempty"`
	State      string     `json:"state"`
	Draft      *bool      `json:"draft,omitempty"`
	Title      string     `json:"title,omitempty"`
	Author     string     `json:"author,omitempty"`
	MergeSHA   string     `json:"merge_sha,omitempty"`
	MergedAt   *time.Time `json:"merged_at,omitempty"`
	ObservedAt time.Time  `json:"observed_at"`
	Source     string     `json:"source"`
	DeliveryID string     `json:"delivery_id,omitempty"`
}

// ChangeCheckIn is one check of a head.
type ChangeCheckIn struct {
	Name        string     `json:"name"`
	State       string     `json:"state"`
	Conclusion  string     `json:"conclusion,omitempty"`
	URL         string     `json:"url,omitempty"`
	Required    *bool      `json:"required,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// ChangeChecksWrite is the checks of one head. Complete says it is the full
// set for that head: a stored check it does not name is marked gone.
type ChangeChecksWrite struct {
	HeadSHA    string          `json:"head_sha"`
	Checks     []ChangeCheckIn `json:"checks"`
	ObservedAt time.Time       `json:"observed_at"`
	Source     string          `json:"source"`
	Complete   bool            `json:"complete,omitempty"`
}

// ChangeThreadIn is one GitHub review thread.
type ChangeThreadIn struct {
	ThreadID      string     `json:"thread_id"`
	URL           string     `json:"url,omitempty"`
	Path          string     `json:"path,omitempty"`
	Line          *int       `json:"line,omitempty"`
	Author        string     `json:"author,omitempty"`
	AuthorKind    string     `json:"author_kind"`
	Severity      string     `json:"severity,omitempty"`
	Resolved      bool       `json:"resolved"`
	Outdated      bool       `json:"outdated,omitempty"`
	Replied       bool       `json:"replied,omitempty"`
	CommentCount  int        `json:"comment_count,omitempty"`
	LastCommentAt *time.Time `json:"last_comment_at,omitempty"`
	Excerpt       string     `json:"excerpt,omitempty"`
}

// ChangeThreadsWrite is the thread set of a PR. Complete marks a stored
// thread it does not name gone.
type ChangeThreadsWrite struct {
	Threads    []ChangeThreadIn `json:"threads"`
	ObservedAt time.Time        `json:"observed_at"`
	Source     string           `json:"source"`
	Complete   bool             `json:"complete,omitempty"`
}

// ChangeFindingIn is one finding of a review pass, in the shape of mergewake's
// prstatus.Finding: ID is the thread id once the finding is posted, else
// mergewake's per-pass id; Location is "path:line"; Summary is one line of at
// most 200 characters. ThreadID names the thread explicitly when ID is not
// one (a thread whose node id equals ID is linked without it). Body is
// optional and, if sent, capped at 8 KB. Its ordinal is its position in the
// write.
type ChangeFindingIn struct {
	ID            string `json:"id"`
	Location      string `json:"location,omitempty"`
	Severity      string `json:"severity,omitempty"`
	Blocking      bool   `json:"blocking,omitempty"`
	Summary       string `json:"summary,omitempty"`
	URL           string `json:"url,omitempty"`
	AuthorReplied bool   `json:"author_replied,omitempty"`
	ThreadID      string `json:"thread_id,omitempty"`
	Body          string `json:"body,omitempty"`
}

// ChangeFindingsWrite is one review pass's findings: append-only, keyed by
// (change, reviewed_head, round, pass, lens, ordinal). A pass with no
// findings still creates its reviews row. Source defaults to "review".
type ChangeFindingsWrite struct {
	ReviewedHead string            `json:"reviewed_head"`
	Round        int               `json:"round"`
	Pass         int               `json:"pass"`
	Lens         string            `json:"lens"`
	Findings     []ChangeFindingIn `json:"findings"`
	ObservedAt   time.Time         `json:"observed_at"`
	Source       string            `json:"source,omitempty"`
}

// ChangeGateWrite is mergewake's gate decision for one head. DecidedAt is its
// observed_at. Source defaults to "review".
type ChangeGateWrite struct {
	HeadSHA        string    `json:"head_sha"`
	State          string    `json:"state"`
	Reasons        []string  `json:"reasons"`
	PolicyRevision string    `json:"policy_revision,omitempty"`
	DecidedAt      time.Time `json:"decided_at"`
	Source         string    `json:"source,omitempty"`
}

// ChangeBatch is any subset of the five sections for one PR.
type ChangeBatch struct {
	ChangeRef
	State    *ChangeStateWrite    `json:"state,omitempty"`
	Checks   *ChangeChecksWrite   `json:"checks,omitempty"`
	Threads  *ChangeThreadsWrite  `json:"threads,omitempty"`
	Findings *ChangeFindingsWrite `json:"findings,omitempty"`
	Gate     *ChangeGateWrite     `json:"gate,omitempty"`
}

// ChangeSectionResult says what became of one section: applied, or ignored
// with a reason ("stale"). Redacted is set when a secret-like text field was
// stored as NULL.
type ChangeSectionResult struct {
	Applied  bool   `json:"applied"`
	Reason   string `json:"reason,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

// ChangeWriteResult answers a write: the change and one result per section
// sent.
type ChangeWriteResult struct {
	ChangeID int64                          `json:"change_id"`
	Sections map[string]ChangeSectionResult `json:"sections"`
}

// ValidChangeRepo reports whether repo is an owner/name pair.
func ValidChangeRepo(repo string) bool { return changeRepoRE.MatchString(repo) }

func checkChangeRef(r ChangeRef) error {
	if !ValidChangeRepo(r.Repo) {
		return invalidChangeWrite("repo must be owner/name")
	}
	if r.PR < 1 || r.PR > math.MaxInt32 {
		return invalidChangeWrite("pr must be a positive number")
	}
	return nil
}

// checkField validates one identifier-like field: UTF-8 text without NUL, at
// most max bytes, and not secret-like (a secret in an identifier refuses the
// write; only free text is redacted).
func checkField(name, v string, max int) error {
	switch {
	case len(v) > max:
		return invalidChangeWrite("%s is at most %d bytes", name, max)
	case !validText(v, max) || !utf8.ValidString(v):
		return invalidChangeWrite("%s must be UTF-8 text without NUL", name)
	}
	return guard.Reject(v)
}

// checkFreeText validates a free-text field that is capped and screened
// rather than refused: only a malformed encoding is an error.
func checkFreeText(name, v string) error {
	if strings.ContainsRune(v, 0) || !utf8.ValidString(v) {
		return invalidChangeWrite("%s must be UTF-8 text without NUL", name)
	}
	return nil
}

func checkObserved(name string, at, now time.Time) error {
	switch {
	case at.IsZero():
		return invalidChangeWrite("%s is required", name)
	case at.After(now.Add(observedSkew)):
		return invalidChangeWrite("%s is in the future", name)
	}
	return nil
}

func checkSource(v string, required bool) error {
	if v == "" && !required {
		return nil
	}
	if !changeSources[v] {
		return invalidChangeWrite("source must be webhook, poll, fresh or review")
	}
	return nil
}

func checkHead(name, v string) error {
	if !headSHARE.MatchString(v) {
		return invalidChangeWrite("%s must be 40 lowercase hex characters", name)
	}
	return nil
}

func checkChangeState(w ChangeStateWrite, now time.Time) error {
	if err := checkHead("head_sha", w.HeadSHA); err != nil {
		return err
	}
	for _, f := range [][2]string{{"base_sha", w.BaseSHA}, {"merge_sha", w.MergeSHA}} {
		if f[1] != "" && !headSHARE.MatchString(f[1]) {
			return invalidChangeWrite("%s must be 40 lowercase hex characters", f[0])
		}
	}
	switch {
	case !changeStates[w.State]:
		return invalidChangeWrite("state must be open, merged or closed")
	case w.State != ChangeMerged && (w.MergeSHA != "" || w.MergedAt != nil):
		return invalidChangeWrite("merge_sha and merged_at need state merged")
	}
	if err := checkSource(w.Source, true); err != nil {
		return err
	}
	if err := checkObserved("observed_at", w.ObservedAt, now); err != nil {
		return err
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"base_ref", w.BaseRef, 255}, {"author", w.Author, 100}, {"delivery_id", w.DeliveryID, 128}} {
		if err := checkField(f.name, f.v, f.max); err != nil {
			return err
		}
	}
	return checkFreeText("title", w.Title)
}

func checkChangeChecks(w ChangeChecksWrite, now time.Time) error {
	if err := checkHead("head_sha", w.HeadSHA); err != nil {
		return err
	}
	if err := checkSource(w.Source, true); err != nil {
		return err
	}
	if err := checkObserved("observed_at", w.ObservedAt, now); err != nil {
		return err
	}
	if len(w.Checks) > maxChangeChecks {
		return invalidChangeWrite("checks has at most %d entries", maxChangeChecks)
	}
	seen := make(map[string]bool, len(w.Checks))
	for _, c := range w.Checks {
		switch {
		case c.Name == "":
			return invalidChangeWrite("a check needs a name")
		case seen[c.Name]:
			return invalidChangeWrite("check names are unique within a write")
		case !changeWordRE.MatchString(c.State) || (c.Conclusion != "" && !changeWordRE.MatchString(c.Conclusion)):
			return invalidChangeWrite("check state and conclusion are lowercase words")
		}
		seen[c.Name] = true
		if err := checkField("check name", c.Name, 200); err != nil {
			return err
		}
		if err := checkField("check url", c.URL, 1024); err != nil {
			return err
		}
	}
	return nil
}

func checkChangeThreads(w ChangeThreadsWrite, now time.Time) error {
	if err := checkSource(w.Source, true); err != nil {
		return err
	}
	if err := checkObserved("observed_at", w.ObservedAt, now); err != nil {
		return err
	}
	if len(w.Threads) > maxChangeThreads {
		return invalidChangeWrite("threads has at most %d entries", maxChangeThreads)
	}
	seen := make(map[string]bool, len(w.Threads))
	for _, t := range w.Threads {
		switch {
		case t.ThreadID == "":
			return invalidChangeWrite("a thread needs a thread_id")
		case seen[t.ThreadID]:
			return invalidChangeWrite("thread ids are unique within a write")
		case !threadAuthors[t.AuthorKind]:
			return invalidChangeWrite("author_kind must be mergewake, bot or human")
		case t.Severity != "" && !severities[t.Severity]:
			return invalidChangeWrite("severity must be critical, high, medium or low")
		case t.CommentCount < 0 || t.CommentCount > 100000 || (t.Line != nil && (*t.Line < 0 || *t.Line > math.MaxInt32)):
			return invalidChangeWrite("comment_count and line are non-negative numbers")
		}
		seen[t.ThreadID] = true
		for _, f := range []struct {
			name, v string
			max     int
		}{{"thread_id", t.ThreadID, 128}, {"thread url", t.URL, 1024}, {"thread path", t.Path, 1024}, {"thread author", t.Author, 100}} {
			if err := checkField(f.name, f.v, f.max); err != nil {
				return err
			}
		}
		if err := checkFreeText("excerpt", t.Excerpt); err != nil {
			return err
		}
	}
	return nil
}

func checkChangeFindings(w ChangeFindingsWrite, now time.Time) error {
	if err := checkHead("reviewed_head", w.ReviewedHead); err != nil {
		return err
	}
	switch {
	case w.Round < 0 || w.Round > 1_000_000 || w.Pass < 0 || w.Pass > 1_000_000:
		return invalidChangeWrite("round and pass are non-negative numbers")
	case !changeLensRE.MatchString(w.Lens):
		return invalidChangeWrite("lens must match [A-Za-z0-9_.:-]{1,64}")
	case len(w.Findings) > maxChangeFinds:
		return invalidChangeWrite("findings has at most %d entries", maxChangeFinds)
	}
	if err := checkSource(w.Source, false); err != nil {
		return err
	}
	if err := checkObserved("observed_at", w.ObservedAt, now); err != nil {
		return err
	}
	for _, f := range w.Findings {
		if f.ID == "" {
			return invalidChangeWrite("a finding needs an id")
		}
		if f.Severity != "" && !severities[f.Severity] {
			return invalidChangeWrite("a finding's severity must be critical, high, medium or low")
		}
		for _, x := range []struct {
			name, v string
			max     int
		}{{"finding id", f.ID, 128}, {"finding location", f.Location, 1100}, {"finding url", f.URL, 1024}, {"finding thread_id", f.ThreadID, 128}} {
			if err := checkField(x.name, x.v, x.max); err != nil {
				return err
			}
		}
		if err := checkFreeText("finding summary", f.Summary); err != nil {
			return err
		}
		if err := checkFreeText("finding body", f.Body); err != nil {
			return err
		}
	}
	return nil
}

func checkChangeGate(w ChangeGateWrite, now time.Time) error {
	if err := checkHead("head_sha", w.HeadSHA); err != nil {
		return err
	}
	switch {
	case !gateStates[w.State]:
		return invalidChangeWrite("state must be ready, blocked or pending")
	case len(w.Reasons) > maxGateReasons:
		return invalidChangeWrite("reasons has at most %d entries", maxGateReasons)
	case w.PolicyRevision != "" && !changeRevisionRE.MatchString(w.PolicyRevision):
		return invalidChangeWrite("policy_revision must be 7 to 64 lowercase hex characters")
	}
	for _, r := range w.Reasons {
		if !changeCodeRE.MatchString(r) {
			return invalidChangeWrite("a reason is a code of [a-z0-9_.:-]{1,64}")
		}
	}
	if err := checkSource(w.Source, false); err != nil {
		return err
	}
	return checkObserved("decided_at", w.DecidedAt, now)
}

// checkChangeBatch validates every section before any database work, so a
// batch with one bad section writes nothing.
func checkChangeBatch(b ChangeBatch, now time.Time) error {
	if err := checkChangeRef(b.ChangeRef); err != nil {
		return err
	}
	if b.State == nil && b.Checks == nil && b.Threads == nil && b.Findings == nil && b.Gate == nil {
		return invalidChangeWrite("send at least one of state, checks, threads, findings or gate")
	}
	// The first refusal in section order is the answer.
	var errs []error
	if b.State != nil {
		errs = append(errs, wrapSection(SectionState, checkChangeState(*b.State, now)))
	}
	if b.Checks != nil {
		errs = append(errs, wrapSection(SectionChecks, checkChangeChecks(*b.Checks, now)))
	}
	if b.Threads != nil {
		errs = append(errs, wrapSection(SectionThreads, checkChangeThreads(*b.Threads, now)))
	}
	if b.Findings != nil {
		errs = append(errs, wrapSection(SectionFindings, checkChangeFindings(*b.Findings, now)))
	}
	if b.Gate != nil {
		errs = append(errs, wrapSection(SectionGate, checkChangeGate(*b.Gate, now)))
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func wrapSection(section string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := guard.SecretPattern(err); ok {
		return err
	}
	if reason, ok := strings.CutPrefix(err.Error(), ErrInvalidChangeWrite.Error()+": "); ok {
		return invalidChangeWrite("%s: %s", section, reason)
	}
	return err
}

// changeLock is the locked changes row a write works against.
type changeLock struct {
	id                                                                            int64
	state                                                                         string
	head, base, mergeSHA, baseRef, title, author                                  *string
	mergedAt                                                                      *time.Time
	draft                                                                         *bool
	stateObserved, checksObserved, threadsObserved, findingsObserved, gateDecided *time.Time
}

// stubHead is the head a new row starts at: the first one a section names.
func (b ChangeBatch) stubHead() string {
	switch {
	case b.State != nil:
		return b.State.HeadSHA
	case b.Checks != nil:
		return b.Checks.HeadSHA
	case b.Findings != nil:
		return b.Findings.ReviewedHead
	case b.Gate != nil:
		return b.Gate.HeadSHA
	}
	return ""
}

// lockChangeTx resolves (repo, pr) to its changes row, creating an open stub
// the way RecordVerification does, and locks it FOR UPDATE: every section
// write of a PR runs one at a time on this row (lockorder.go).
func lockChangeTx(ctx context.Context, tx pgx.Tx, b ChangeBatch, now time.Time) (changeLock, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO changes(repo,pr_number,state,head_sha,opened_at,created_at,updated_at)
VALUES($1,$2,'open',$3,$4,$4,$4) ON CONFLICT(repo,pr_number) DO NOTHING`, b.Repo, b.PR, nullStr(b.stubHead()), now); err != nil {
		return changeLock{}, err
	}
	var c changeLock
	err := tx.QueryRow(ctx, `SELECT id,state,head_sha,base_sha,merge_sha,base_ref,title,author,merged_at,draft,state_observed_at,checks_observed_at,threads_observed_at,findings_observed_at,gate_decided_at
FROM changes WHERE repo=$1 AND pr_number=$2 FOR UPDATE`, b.Repo, b.PR).Scan(&c.id, &c.state, &c.head, &c.base, &c.mergeSHA, &c.baseRef, &c.title, &c.author, &c.mergedAt, &c.draft,
		&c.stateObserved, &c.checksObserved, &c.threadsObserved, &c.findingsObserved, &c.gateDecided)
	return c, err
}

// olderThan reports whether a write observed at `at` is older than the stored
// time. An equal time is not older: the write applies and, being the same
// data, changes nothing.
func olderThan(stored *time.Time, at time.Time) bool { return stored != nil && at.Before(*stored) }

var staleResult = ChangeSectionResult{Applied: false, Reason: staleReason}

// capRunes cuts s to at most n characters.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for k := range s {
		if i == n {
			return s[:k]
		}
		i++
	}
	return s
}

// capBytes cuts s to at most n bytes on a character boundary.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// screened turns a free-text field into its column value: NULL for empty,
// NULL with redacted for secret-like text (judged before it is cut, so no
// piece of a secret survives the cap), otherwise the capped text.
func screened(s string, cut func(string) string) (val *string, redacted bool) {
	if s == "" {
		return nil, false
	}
	if guard.Reject(s) != nil {
		return nil, true
	}
	s = cut(s)
	return &s, false
}

type setCol struct {
	name, typ string
	val       any
}

// updateChangeCols sets columns on a locked changes row and touches nothing
// when the row already holds these values. touch, when set, is the
// updated_at written along with a real change.
func updateChangeCols(ctx context.Context, tx pgx.Tx, id int64, touch *time.Time, cols ...setCol) error {
	args := []any{id}
	var sets, cur, next []string
	for i, c := range cols {
		p := fmt.Sprintf("$%d::%s", i+2, c.typ)
		sets, cur, next = append(sets, c.name+"="+p), append(cur, c.name), append(next, p)
		args = append(args, c.val)
	}
	if touch != nil {
		args = append(args, *touch)
		sets = append(sets, fmt.Sprintf("updated_at=$%d::timestamptz", len(args)))
	}
	_, err := tx.Exec(ctx, `UPDATE changes SET `+strings.Join(sets, ",")+` WHERE id=$1 AND (`+strings.Join(cur, ",")+`) IS DISTINCT FROM (`+strings.Join(next, ",")+`)`, args...)
	return err
}

func execBatch(ctx context.Context, tx pgx.Tx, b *pgx.Batch) error {
	if b.Len() == 0 {
		return nil
	}
	br := tx.SendBatch(ctx, b)
	for i := 0; i < b.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return err
		}
	}
	return br.Close()
}

func applyStateTx(ctx context.Context, tx pgx.Tx, c changeLock, w ChangeStateWrite, now time.Time) (ChangeSectionResult, error) {
	if olderThan(c.stateObserved, w.ObservedAt) {
		return staleResult, nil
	}
	title, redacted := screened(w.Title, func(s string) string { return capBytes(s, maxChangeTitle) })
	if w.Title == "" {
		title = c.title
	}
	// merged is terminal: a later open or closed write leaves state, head and
	// base as they were (the way upsertChangeTx keeps a merged row).
	state, head, base := w.State, nullStr(w.HeadSHA), nullStr(w.BaseSHA)
	if base == nil {
		base = c.base
	}
	if c.state == ChangeMerged && w.State != ChangeMerged {
		state, head, base = ChangeMerged, c.head, c.base
	}
	mergeSHA, mergedAt := c.mergeSHA, c.mergedAt
	if w.MergeSHA != "" {
		mergeSHA = &w.MergeSHA
	}
	if w.State == ChangeMerged && mergedAt == nil {
		at := w.ObservedAt
		if w.MergedAt != nil {
			at = *w.MergedAt
		}
		mergedAt = &at
	}
	pick := func(v string, old *string) *string {
		if v == "" {
			return old
		}
		return &v
	}
	draft := c.draft
	if w.Draft != nil {
		draft = w.Draft
	}
	err := updateChangeCols(ctx, tx, c.id, &now,
		setCol{"state", "text", state}, setCol{"head_sha", "text", head}, setCol{"base_sha", "text", base},
		setCol{"merge_sha", "text", mergeSHA}, setCol{"merged_at", "timestamptz", mergedAt}, setCol{"draft", "boolean", draft},
		setCol{"base_ref", "text", pick(w.BaseRef, c.baseRef)}, setCol{"title", "text", title}, setCol{"author", "text", pick(w.Author, c.author)},
		setCol{"state_observed_at", "timestamptz", w.ObservedAt}, setCol{"state_source", "text", w.Source},
		setCol{"state_delivery_id", "text", nullStr(w.DeliveryID)})
	return ChangeSectionResult{Applied: true, Redacted: redacted}, err
}

func applyChecksTx(ctx context.Context, tx pgx.Tx, c changeLock, w ChangeChecksWrite) (ChangeSectionResult, error) {
	if olderThan(c.checksObserved, w.ObservedAt) {
		return staleResult, nil
	}
	b := &pgx.Batch{}
	names := make([]string, 0, len(w.Checks))
	for _, k := range w.Checks {
		names = append(names, k.Name)
		b.Queue(`INSERT INTO change_checks(change_id,head_sha,name,state,conclusion,url,required,started_at,completed_at,observed_at,source,gone)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,false)
ON CONFLICT(change_id,head_sha,name) DO UPDATE SET state=EXCLUDED.state,conclusion=EXCLUDED.conclusion,url=EXCLUDED.url,required=EXCLUDED.required,
	started_at=EXCLUDED.started_at,completed_at=EXCLUDED.completed_at,observed_at=EXCLUDED.observed_at,source=EXCLUDED.source,gone=false
WHERE (change_checks.state,change_checks.conclusion,change_checks.url,change_checks.required,change_checks.started_at,change_checks.completed_at,change_checks.observed_at,change_checks.source,change_checks.gone)
	IS DISTINCT FROM (EXCLUDED.state,EXCLUDED.conclusion,EXCLUDED.url,EXCLUDED.required,EXCLUDED.started_at,EXCLUDED.completed_at,EXCLUDED.observed_at,EXCLUDED.source,EXCLUDED.gone)`,
			c.id, w.HeadSHA, k.Name, k.State, nullStr(k.Conclusion), nullStr(k.URL), k.Required, k.StartedAt, k.CompletedAt, w.ObservedAt, w.Source)
	}
	if w.Complete {
		b.Queue(`UPDATE change_checks SET gone=true,observed_at=$3,source=$4 WHERE change_id=$1 AND head_sha=$2 AND NOT gone AND name <> ALL($5::text[])`,
			c.id, w.HeadSHA, w.ObservedAt, w.Source, names)
	}
	if err := execBatch(ctx, tx, b); err != nil {
		return ChangeSectionResult{}, err
	}
	err := updateChangeCols(ctx, tx, c.id, nil, setCol{"checks_observed_at", "timestamptz", w.ObservedAt}, setCol{"checks_source", "text", w.Source})
	return ChangeSectionResult{Applied: true}, err
}

func applyThreadsTx(ctx context.Context, tx pgx.Tx, c changeLock, w ChangeThreadsWrite) (ChangeSectionResult, error) {
	if olderThan(c.threadsObserved, w.ObservedAt) {
		return staleResult, nil
	}
	b := &pgx.Batch{}
	ids := make([]string, 0, len(w.Threads))
	redactedAny := false
	for _, t := range w.Threads {
		ids = append(ids, t.ThreadID)
		excerpt, redacted := screened(t.Excerpt, func(s string) string { return capRunes(s, MaxThreadExcerpt) })
		redactedAny = redactedAny || redacted
		b.Queue(`INSERT INTO change_threads(change_id,thread_id,url,path,line,author,author_kind,severity,resolved,outdated,replied,comment_count,last_comment_at,excerpt,redacted,observed_at,source,gone)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,false)
ON CONFLICT(change_id,thread_id) DO UPDATE SET url=EXCLUDED.url,path=EXCLUDED.path,line=EXCLUDED.line,author=EXCLUDED.author,author_kind=EXCLUDED.author_kind,severity=EXCLUDED.severity,
	resolved=EXCLUDED.resolved,outdated=EXCLUDED.outdated,replied=EXCLUDED.replied,comment_count=EXCLUDED.comment_count,last_comment_at=EXCLUDED.last_comment_at,
	excerpt=EXCLUDED.excerpt,redacted=EXCLUDED.redacted,observed_at=EXCLUDED.observed_at,source=EXCLUDED.source,gone=false
WHERE (change_threads.url,change_threads.path,change_threads.line,change_threads.author,change_threads.author_kind,change_threads.severity,change_threads.resolved,change_threads.outdated,change_threads.replied,
	change_threads.comment_count,change_threads.last_comment_at,change_threads.excerpt,change_threads.redacted,change_threads.observed_at,change_threads.source,change_threads.gone)
	IS DISTINCT FROM (EXCLUDED.url,EXCLUDED.path,EXCLUDED.line,EXCLUDED.author,EXCLUDED.author_kind,EXCLUDED.severity,EXCLUDED.resolved,EXCLUDED.outdated,EXCLUDED.replied,
	EXCLUDED.comment_count,EXCLUDED.last_comment_at,EXCLUDED.excerpt,EXCLUDED.redacted,EXCLUDED.observed_at,EXCLUDED.source,EXCLUDED.gone)`,
			c.id, t.ThreadID, nullStr(t.URL), nullStr(t.Path), t.Line, nullStr(t.Author), t.AuthorKind, nullStr(t.Severity), t.Resolved, t.Outdated, t.Replied,
			t.CommentCount, t.LastCommentAt, excerpt, redacted, w.ObservedAt, w.Source)
	}
	if w.Complete {
		b.Queue(`UPDATE change_threads SET gone=true,observed_at=$2,source=$3 WHERE change_id=$1 AND NOT gone AND thread_id <> ALL($4::text[])`,
			c.id, w.ObservedAt, w.Source, ids)
	}
	if err := execBatch(ctx, tx, b); err != nil {
		return ChangeSectionResult{}, err
	}
	err := updateChangeCols(ctx, tx, c.id, nil, setCol{"threads_observed_at", "timestamptz", w.ObservedAt}, setCol{"threads_source", "text", w.Source})
	return ChangeSectionResult{Applied: true, Redacted: redactedAny}, err
}

func applyFindingsTx(ctx context.Context, tx pgx.Tx, c changeLock, w ChangeFindingsWrite, now time.Time) (ChangeSectionResult, error) {
	if olderThan(c.findingsObserved, w.ObservedAt) {
		return staleResult, nil
	}
	source := w.Source
	if source == "" {
		source = defaultFindingSrc
	}
	// The pass's reviews row: a bare one carrying only its key when no event
	// has reported it (insertReviewTx fills a bare row later).
	var reviewID int64
	if err := tx.QueryRow(ctx, `WITH ins AS (
	INSERT INTO reviews(change_id,reviewed_head,round,pass,lens,recorded_at) VALUES($1,$2,$3,$4,$5,$6)
	ON CONFLICT(change_id,reviewed_head,lens,round,pass) DO NOTHING RETURNING id)
SELECT id FROM ins UNION ALL SELECT id FROM reviews WHERE change_id=$1 AND reviewed_head=$2 AND lens=$5 AND round=$3 AND pass=$4 LIMIT 1`,
		c.id, w.ReviewedHead, w.Round, w.Pass, w.Lens, now).Scan(&reviewID); err != nil {
		return ChangeSectionResult{}, err
	}
	b := &pgx.Batch{}
	redactedAny := false
	for i, f := range w.Findings {
		summary, sr := screened(f.Summary, func(s string) string { return capRunes(s, MaxFindingSummary) })
		body, br := screened(f.Body, func(s string) string { return capBytes(s, MaxFindingBody) })
		redacted := sr || br
		redactedAny = redactedAny || redacted
		b.Queue(`INSERT INTO change_findings(change_id,reviewed_head,round,pass,lens,ordinal,review_id,ext_id,location,severity,blocking,summary,url,author_replied,thread_id,body,redacted,observed_at,source)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) ON CONFLICT DO NOTHING`,
			c.id, w.ReviewedHead, w.Round, w.Pass, w.Lens, i, reviewID, f.ID, nullStr(f.Location), nullStr(f.Severity), f.Blocking, summary, nullStr(f.URL), f.AuthorReplied, nullStr(f.ThreadID), body, redacted, w.ObservedAt, source)
	}
	if err := execBatch(ctx, tx, b); err != nil {
		return ChangeSectionResult{}, err
	}
	err := updateChangeCols(ctx, tx, c.id, nil, setCol{"findings_observed_at", "timestamptz", w.ObservedAt}, setCol{"findings_source", "text", source})
	return ChangeSectionResult{Applied: true, Redacted: redactedAny}, err
}

func applyGateTx(ctx context.Context, tx pgx.Tx, c changeLock, w ChangeGateWrite) (ChangeSectionResult, error) {
	if olderThan(c.gateDecided, w.DecidedAt) {
		return staleResult, nil
	}
	source := w.Source
	if source == "" {
		source = "review"
	}
	reasons := w.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	raw, err := json.Marshal(reasons)
	if err != nil {
		return ChangeSectionResult{}, err
	}
	err = updateChangeCols(ctx, tx, c.id, nil,
		setCol{"gate_state", "text", w.State}, setCol{"gate_head", "text", w.HeadSHA}, setCol{"gate_reasons", "jsonb", string(raw)},
		setCol{"gate_decided_at", "timestamptz", w.DecidedAt}, setCol{"gate_policy_revision", "text", nullStr(w.PolicyRevision)}, setCol{"gate_source", "text", source})
	return ChangeSectionResult{Applied: true}, err
}

// ApplyChangeBatch applies any subset of the five sections for one PR in one
// transaction, each judged for freshness on its own: a stale section is
// reported and skipped, every other section still applies. A section that
// fails validation fails the whole batch before anything is written. It locks
// the PR's changes row and then only that PR's child rows (lockorder.go) and
// takes no task row.
func (s *Store) ApplyChangeBatch(ctx context.Context, b ChangeBatch) (ChangeWriteResult, error) {
	now := time.Now().UTC()
	if err := checkChangeBatch(b, now); err != nil {
		return ChangeWriteResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChangeWriteResult{}, err
	}
	defer tx.Rollback(ctx)
	c, err := lockChangeTx(ctx, tx, b, now)
	if err != nil {
		return ChangeWriteResult{}, err
	}
	out := ChangeWriteResult{ChangeID: c.id, Sections: map[string]ChangeSectionResult{}}
	if b.State != nil {
		if out.Sections[SectionState], err = applyStateTx(ctx, tx, c, *b.State, now); err != nil {
			return ChangeWriteResult{}, err
		}
	}
	if b.Checks != nil {
		if out.Sections[SectionChecks], err = applyChecksTx(ctx, tx, c, *b.Checks); err != nil {
			return ChangeWriteResult{}, err
		}
	}
	if b.Threads != nil {
		if out.Sections[SectionThreads], err = applyThreadsTx(ctx, tx, c, *b.Threads); err != nil {
			return ChangeWriteResult{}, err
		}
	}
	if b.Findings != nil {
		if out.Sections[SectionFindings], err = applyFindingsTx(ctx, tx, c, *b.Findings, now); err != nil {
			return ChangeWriteResult{}, err
		}
	}
	if b.Gate != nil {
		if out.Sections[SectionGate], err = applyGateTx(ctx, tx, c, *b.Gate); err != nil {
			return ChangeWriteResult{}, err
		}
	}
	return out, tx.Commit(ctx)
}

// ApplyChangeState writes the state section of a PR.
func (s *Store) ApplyChangeState(ctx context.Context, ref ChangeRef, w ChangeStateWrite) (ChangeWriteResult, error) {
	return s.ApplyChangeBatch(ctx, ChangeBatch{ChangeRef: ref, State: &w})
}

// ApplyChangeChecks writes the checks section of a PR.
func (s *Store) ApplyChangeChecks(ctx context.Context, ref ChangeRef, w ChangeChecksWrite) (ChangeWriteResult, error) {
	return s.ApplyChangeBatch(ctx, ChangeBatch{ChangeRef: ref, Checks: &w})
}

// ApplyChangeThreads writes the threads section of a PR.
func (s *Store) ApplyChangeThreads(ctx context.Context, ref ChangeRef, w ChangeThreadsWrite) (ChangeWriteResult, error) {
	return s.ApplyChangeBatch(ctx, ChangeBatch{ChangeRef: ref, Threads: &w})
}

// ApplyChangeFindings writes one review pass's findings for a PR.
func (s *Store) ApplyChangeFindings(ctx context.Context, ref ChangeRef, w ChangeFindingsWrite) (ChangeWriteResult, error) {
	return s.ApplyChangeBatch(ctx, ChangeBatch{ChangeRef: ref, Findings: &w})
}

// ApplyChangeGate writes mergewake's gate decision for a PR.
func (s *Store) ApplyChangeGate(ctx context.Context, ref ChangeRef, w ChangeGateWrite) (ChangeWriteResult, error) {
	return s.ApplyChangeBatch(ctx, ChangeBatch{ChangeRef: ref, Gate: &w})
}

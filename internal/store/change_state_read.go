package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// findingStatusSQL derives a finding's state at read time; findings have no
// status column (contract §6). With a thread row it is the thread's:
// resolved (or gone) is "resolved", anything else "open". Without a thread
// row (never posted, or the row not written yet) it is "open" while the
// finding's reviewed_head is the change's current head and "superseded" once
// the head has moved. It uses the aliases f (change_findings), c (changes)
// and t (a LEFT JOIN of change_threads); one expression serves the one-PR
// read and the list counts.
const findingStatusSQL = `(CASE WHEN t.thread_id IS NOT NULL THEN (CASE WHEN t.resolved OR t.gone THEN 'resolved' ELSE 'open' END)
	WHEN f.reviewed_head = c.head_sha THEN 'open' ELSE 'superseded' END)`

// findingThreadJoin links a finding to its thread: by its thread_id, else by
// its id, which is the thread's node id once mergewake has posted the finding.
const findingThreadJoin = `LEFT JOIN change_threads t ON t.change_id=f.change_id AND t.thread_id=COALESCE(f.thread_id,f.ext_id)`

// ChangeInclude selects the sections of the one-PR read besides the change
// itself and its freshness, which are always present.
type ChangeInclude struct{ Checks, Threads, Findings, Gate bool }

// AllChangeSections includes every section.
var AllChangeSections = ChangeInclude{Checks: true, Threads: true, Findings: true, Gate: true}

// ParseChangeInclude parses the include query value: a comma list of checks,
// threads, findings and gate. Empty means all.
func ParseChangeInclude(v string) (ChangeInclude, error) {
	if v == "" {
		return AllChangeSections, nil
	}
	var in ChangeInclude
	for _, tok := range strings.Split(v, ",") {
		switch tok {
		case SectionChecks:
			in.Checks = true
		case SectionThreads:
			in.Threads = true
		case SectionFindings:
			in.Findings = true
		case SectionGate:
			in.Gate = true
		default:
			return ChangeInclude{}, invalidChangeWrite("include must list checks, threads, findings or gate")
		}
	}
	return in, nil
}

// ChangeReadOptions shapes the one-PR read. AllHeads adds the checks of
// earlier heads to the current head's.
type ChangeReadOptions struct {
	AllHeads bool
	Include  ChangeInclude
}

// ChangeSummary is the PR's scalar state with the state section's freshness.
type ChangeSummary struct {
	ID         int64      `json:"id"`
	Repo       string     `json:"repo"`
	PRNumber   int        `json:"pr_number"`
	PR         string     `json:"pr"`
	State      string     `json:"state"`
	Draft      *bool      `json:"draft,omitempty"`
	Title      *string    `json:"title,omitempty"`
	Author     *string    `json:"author,omitempty"`
	BaseRef    *string    `json:"base_ref,omitempty"`
	HeadSHA    *string    `json:"head_sha,omitempty"`
	BaseSHA    *string    `json:"base_sha,omitempty"`
	MergeSHA   *string    `json:"merge_sha,omitempty"`
	OpenedAt   time.Time  `json:"opened_at"`
	MergedAt   *time.Time `json:"merged_at,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	Source     *string    `json:"source,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// ChangeGate is mergewake's last gate decision. Current is false once the
// change's head has moved past the head the decision was made for.
type ChangeGate struct {
	State          string    `json:"state"`
	Head           string    `json:"head_sha"`
	Current        bool      `json:"current"`
	Reasons        []string  `json:"reasons"`
	PolicyRevision *string   `json:"policy_revision,omitempty"`
	DecidedAt      time.Time `json:"decided_at"`
	Source         *string   `json:"source,omitempty"`
}

// ChangeCheck is one stored check.
type ChangeCheck struct {
	HeadSHA     string     `json:"head_sha"`
	Name        string     `json:"name"`
	State       string     `json:"state"`
	Conclusion  *string    `json:"conclusion,omitempty"`
	URL         *string    `json:"url,omitempty"`
	Required    *bool      `json:"required,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	ObservedAt  time.Time  `json:"observed_at"`
	Source      string     `json:"source"`
}

// ChangeThread is one stored review thread.
type ChangeThread struct {
	ThreadID      string     `json:"thread_id"`
	URL           *string    `json:"url,omitempty"`
	Path          *string    `json:"path,omitempty"`
	Line          *int       `json:"line,omitempty"`
	Author        *string    `json:"author,omitempty"`
	AuthorKind    string     `json:"author_kind"`
	Severity      *string    `json:"severity,omitempty"`
	Resolved      bool       `json:"resolved"`
	Outdated      bool       `json:"outdated"`
	Replied       bool       `json:"replied"`
	CommentCount  *int       `json:"comment_count,omitempty"`
	LastCommentAt *time.Time `json:"last_comment_at,omitempty"`
	Excerpt       *string    `json:"excerpt,omitempty"`
	Redacted      bool       `json:"redacted,omitempty"`
	ObservedAt    time.Time  `json:"observed_at"`
	Source        string     `json:"source"`
}

// ChangeFinding is one stored finding in mergewake's prstatus.Finding field
// names (id, location, severity, blocking, summary, url, author_replied), so a
// reader can pass them through unchanged, with the pass it belongs to and its
// derived Status ("open", "resolved" or "superseded", see findingStatusSQL).
// ThreadID is the linked thread, if any; AuthorReplied is set when mergewake
// said so or the linked thread says the PR author answered.
type ChangeFinding struct {
	ID            string    `json:"id"`
	Location      *string   `json:"location,omitempty"`
	Severity      *string   `json:"severity,omitempty"`
	Blocking      bool      `json:"blocking"`
	Summary       *string   `json:"summary,omitempty"`
	URL           *string   `json:"url,omitempty"`
	AuthorReplied bool      `json:"author_replied"`
	Status        string    `json:"status"`
	ThreadID      *string   `json:"thread_id,omitempty"`
	Body          *string   `json:"body,omitempty"`
	Redacted      bool      `json:"redacted,omitempty"`
	ReviewedHead  string    `json:"reviewed_head"`
	Round         int       `json:"round"`
	Pass          int       `json:"pass"`
	Lens          string    `json:"lens"`
	Ordinal       int       `json:"ordinal"`
	ReviewID      int64     `json:"review_id"`
	ObservedAt    time.Time `json:"observed_at"`
	Source        string    `json:"source"`
}

// SectionFreshness is when a section was last observed and from where.
type SectionFreshness struct {
	ObservedAt time.Time `json:"observed_at"`
	Source     *string   `json:"source,omitempty"`
}

// ChangeFreshness has one entry per section; nil means never written.
type ChangeFreshness struct {
	State    *SectionFreshness `json:"state"`
	Checks   *SectionFreshness `json:"checks"`
	Threads  *SectionFreshness `json:"threads"`
	Findings *SectionFreshness `json:"findings"`
	Gate     *SectionFreshness `json:"gate"`
}

// ChangeView is the one-PR read. A section the include option left out is nil
// and absent from the JSON; an included empty one is [].
type ChangeView struct {
	Change    ChangeSummary    `json:"change"`
	Gate      *ChangeGate      `json:"gate,omitempty"`
	Checks    *[]ChangeCheck   `json:"checks,omitempty"`
	Threads   *[]ChangeThread  `json:"threads,omitempty"`
	Findings  *[]ChangeFinding `json:"findings,omitempty"`
	Freshness ChangeFreshness  `json:"freshness"`
}

func freshness(at *time.Time, source *string) *SectionFreshness {
	if at == nil {
		return nil
	}
	return &SectionFreshness{ObservedAt: *at, Source: source}
}

// GetChangeState reads one PR's state: the change, its gate, the checks of
// its current head (every head with AllHeads), threads unresolved first and
// findings open first, with per-section freshness. found is false for a PR
// hk has never seen. The reads share one snapshot.
func (s *Store) GetChangeState(ctx context.Context, ref ChangeRef, opt ChangeReadOptions) (ChangeView, bool, error) {
	if err := checkChangeRef(ref); err != nil {
		return ChangeView{}, false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ChangeView{}, false, err
	}
	defer tx.Rollback(ctx)
	var v ChangeView
	var (
		c                                           = &v.Change
		checksAt, threadsAt, findingsAt, gateAt     *time.Time
		checksSrc, threadsSrc, findingsSrc, gateSrc *string
		gateState, gateHead, gatePolicy             *string
		gateRaw                                     []byte
	)
	err = tx.QueryRow(ctx, `SELECT id,repo,pr_number,state,draft,title,author,base_ref,head_sha,base_sha,merge_sha,opened_at,merged_at,state_observed_at,state_source,updated_at,
	checks_observed_at,checks_source,threads_observed_at,threads_source,findings_observed_at,findings_source,gate_decided_at,gate_source,gate_state,gate_head,gate_reasons,gate_policy_revision
FROM changes WHERE repo=$1 AND pr_number=$2`, ref.Repo, ref.PR).Scan(&c.ID, &c.Repo, &c.PRNumber, &c.State, &c.Draft, &c.Title, &c.Author, &c.BaseRef, &c.HeadSHA, &c.BaseSHA, &c.MergeSHA,
		&c.OpenedAt, &c.MergedAt, &c.ObservedAt, &c.Source, &c.UpdatedAt, &checksAt, &checksSrc, &threadsAt, &threadsSrc, &findingsAt, &findingsSrc, &gateAt, &gateSrc,
		&gateState, &gateHead, &gateRaw, &gatePolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChangeView{}, false, nil
	}
	if err != nil {
		return ChangeView{}, false, err
	}
	c.PR = changeURL(c.Repo, c.PRNumber)
	v.Freshness = ChangeFreshness{State: freshness(c.ObservedAt, c.Source), Checks: freshness(checksAt, checksSrc), Threads: freshness(threadsAt, threadsSrc),
		Findings: freshness(findingsAt, findingsSrc), Gate: freshness(gateAt, gateSrc)}
	head := ""
	if c.HeadSHA != nil {
		head = *c.HeadSHA
	}
	if opt.Include.Gate && gateState != nil && gateHead != nil && gateAt != nil {
		g := ChangeGate{State: *gateState, Head: *gateHead, Current: *gateHead == head, Reasons: []string{}, PolicyRevision: gatePolicy, DecidedAt: *gateAt, Source: gateSrc}
		if _, err = scanJSONB(gateRaw, &g.Reasons); err != nil {
			return ChangeView{}, false, err
		}
		v.Gate = &g
	}
	if opt.Include.Checks {
		rows, err := tx.Query(ctx, `SELECT head_sha,name,state,conclusion,url,required,started_at,completed_at,observed_at,source FROM change_checks
WHERE change_id=$1 AND NOT gone AND ($2 OR head_sha=$3) ORDER BY (head_sha=$3) DESC, head_sha, name`, c.ID, opt.AllHeads, head)
		if err != nil {
			return ChangeView{}, false, err
		}
		checks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChangeCheck, error) {
			var k ChangeCheck
			err := r.Scan(&k.HeadSHA, &k.Name, &k.State, &k.Conclusion, &k.URL, &k.Required, &k.StartedAt, &k.CompletedAt, &k.ObservedAt, &k.Source)
			return k, err
		})
		if err != nil {
			return ChangeView{}, false, err
		}
		if checks == nil {
			checks = []ChangeCheck{}
		}
		v.Checks = &checks
	}
	if opt.Include.Threads {
		rows, err := tx.Query(ctx, `SELECT thread_id,url,path,line,author,author_kind,severity,resolved,outdated,replied,comment_count,last_comment_at,excerpt,redacted,observed_at,source
FROM change_threads WHERE change_id=$1 AND NOT gone ORDER BY resolved, last_comment_at DESC NULLS LAST, thread_id`, c.ID)
		if err != nil {
			return ChangeView{}, false, err
		}
		threads, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChangeThread, error) {
			var t ChangeThread
			err := r.Scan(&t.ThreadID, &t.URL, &t.Path, &t.Line, &t.Author, &t.AuthorKind, &t.Severity, &t.Resolved, &t.Outdated, &t.Replied, &t.CommentCount, &t.LastCommentAt, &t.Excerpt, &t.Redacted, &t.ObservedAt, &t.Source)
			return t, err
		})
		if err != nil {
			return ChangeView{}, false, err
		}
		if threads == nil {
			threads = []ChangeThread{}
		}
		v.Threads = &threads
	}
	if opt.Include.Findings {
		rows, err := tx.Query(ctx, `SELECT f.ext_id,f.location,f.severity,f.blocking,f.summary,f.url,f.author_replied OR COALESCE(t.replied,false),`+findingStatusSQL+`,COALESCE(f.thread_id,t.thread_id),f.body,f.redacted,
	f.reviewed_head,f.round,f.pass,f.lens,f.ordinal,f.review_id,f.observed_at,f.source
FROM change_findings f JOIN changes c ON c.id=f.change_id `+findingThreadJoin+`
WHERE f.change_id=$1 ORDER BY f.review_id DESC, f.ordinal`, c.ID)
		if err != nil {
			return ChangeView{}, false, err
		}
		findings, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChangeFinding, error) {
			var f ChangeFinding
			err := r.Scan(&f.ID, &f.Location, &f.Severity, &f.Blocking, &f.Summary, &f.URL, &f.AuthorReplied, &f.Status, &f.ThreadID, &f.Body, &f.Redacted,
				&f.ReviewedHead, &f.Round, &f.Pass, &f.Lens, &f.Ordinal, &f.ReviewID, &f.ObservedAt, &f.Source)
			return f, err
		})
		if err != nil {
			return ChangeView{}, false, err
		}
		// Open first; the query order (newest pass, then ordinal) is kept within each group.
		slices.SortStableFunc(findings, func(a, b ChangeFinding) int {
			return boolRank(b.Status == "open") - boolRank(a.Status == "open")
		})
		if findings == nil {
			findings = []ChangeFinding{}
		}
		v.Findings = &findings
	}
	return v, true, tx.Commit(ctx)
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

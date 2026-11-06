package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// secretLike builds a credential-shaped value at run time: gitleaks scans
// every committed file, so no literal of this shape may appear in one.
func secretLike() string { return "gh" + "p_" + strings.Repeat("a", 36) }

func boolPtr(b bool) *bool { return &b }

func validState(at time.Time) ChangeStateWrite {
	return ChangeStateWrite{HeadSHA: testSHA1, BaseSHA: testSHA2, BaseRef: "main", State: ChangeOpen, Draft: boolPtr(false), Title: "Add PR state", Author: "octo", ObservedAt: at, Source: "webhook"}
}

// ---- validation (no database) ----

func TestChangeBatchValidation(t *testing.T) {
	now := time.Now().UTC()
	ref := ChangeRef{Repo: "example/app", PR: 7}
	ok := ChangeBatch{ChangeRef: ref, State: ptr(validState(now))}
	if err := checkChangeBatch(ok, now); err != nil {
		t.Fatalf("valid batch refused: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*ChangeBatch)
		reason string
	}{
		{"bad repo", func(b *ChangeBatch) { b.Repo = "no-slash" }, "repo must be owner/name"},
		{"bad pr", func(b *ChangeBatch) { b.PR = 0 }, "pr must be a positive number"},
		{"empty", func(b *ChangeBatch) { b.State = nil }, "send at least one"},
		{"bad head", func(b *ChangeBatch) { b.State.HeadSHA = "abc" }, "state: head_sha"},
		{"bad state", func(b *ChangeBatch) { b.State.State = "draft" }, "state: state must be"},
		{"merge fields on open", func(b *ChangeBatch) { b.State.MergeSHA = testSHA3 }, "need state merged"},
		{"bad source", func(b *ChangeBatch) { b.State.Source = "email" }, "state: source"},
		{"no observed_at", func(b *ChangeBatch) { b.State.ObservedAt = time.Time{} }, "observed_at is required"},
		{"future observed_at", func(b *ChangeBatch) { b.State.ObservedAt = now.Add(time.Hour) }, "in the future"},
		{"nul in title", func(b *ChangeBatch) { b.State.Title = "a\x00b" }, "title must be UTF-8"},
		{"bad second section", func(b *ChangeBatch) {
			b.Checks = &ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: now, Source: "poll", Checks: []ChangeCheckIn{{Name: "ci", State: "Queued"}}}
		}, "checks: check state"},
		{"duplicate check", func(b *ChangeBatch) {
			b.Checks = &ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: now, Source: "poll", Checks: []ChangeCheckIn{{Name: "ci", State: "queued"}, {Name: "ci", State: "queued"}}}
		}, "unique within a write"},
		{"too many checks", func(b *ChangeBatch) {
			cs := make([]ChangeCheckIn, maxChangeChecks+1)
			for i := range cs {
				cs[i] = ChangeCheckIn{Name: strings.Repeat("n", 3) + string(rune('a'+i%26)) + strings.Repeat("x", i/26), State: "queued"}
			}
			b.Checks = &ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: now, Source: "poll", Checks: cs}
		}, "at most"},
		{"thread author kind", func(b *ChangeBatch) {
			b.Threads = &ChangeThreadsWrite{ObservedAt: now, Source: "poll", Threads: []ChangeThreadIn{{ThreadID: "T1", AuthorKind: "robot"}}}
		}, "author_kind"},
		{"finding severity", func(b *ChangeBatch) {
			b.Findings = &ChangeFindingsWrite{ReviewedHead: testSHA1, Lens: "correctness", ObservedAt: now, Findings: []ChangeFindingIn{{ID: "f", Severity: "urgent"}}}
		}, "severity"},
		{"finding id", func(b *ChangeBatch) {
			b.Findings = &ChangeFindingsWrite{ReviewedHead: testSHA1, Lens: "correctness", ObservedAt: now, Findings: []ChangeFindingIn{{Severity: "high"}}}
		}, "a finding needs an id"},
		{"finding lens", func(b *ChangeBatch) {
			b.Findings = &ChangeFindingsWrite{ReviewedHead: testSHA1, Lens: "has space", ObservedAt: now}
		}, "lens must match"},
		{"gate state", func(b *ChangeBatch) {
			b.Gate = &ChangeGateWrite{HeadSHA: testSHA1, State: "maybe", DecidedAt: now}
		}, "gate: state must be"},
		{"gate reason code", func(b *ChangeBatch) {
			b.Gate = &ChangeGateWrite{HeadSHA: testSHA1, State: "blocked", Reasons: []string{"Not A Code"}, DecidedAt: now}
		}, "a reason is a code"},
		{"gate policy revision", func(b *ChangeBatch) {
			b.Gate = &ChangeGateWrite{HeadSHA: testSHA1, State: "ready", PolicyRevision: "XYZ", DecidedAt: now}
		}, "policy_revision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := ChangeBatch{ChangeRef: ref, State: ptr(validState(now))}
			tc.mutate(&b)
			err := checkChangeBatch(b, now)
			if !errors.Is(err, ErrInvalidChangeWrite) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err=%v, want ErrInvalidChangeWrite containing %q", err, tc.reason)
			}
		})
	}
	// An identifier that looks like a credential refuses the write as a secret.
	b := ChangeBatch{ChangeRef: ref, State: ptr(validState(now))}
	b.State.Author = secretLike()
	if _, ok := guard.SecretPattern(checkChangeBatch(b, now)); !ok {
		t.Fatal("a secret-like author was accepted")
	}
}

func ptr[T any](v T) *T { return &v }

func TestChangeTextCaps(t *testing.T) {
	long := strings.Repeat("é", 5000) // 2 bytes each
	if got := capRunes(long, MaxThreadExcerpt); len([]rune(got)) != MaxThreadExcerpt {
		t.Fatalf("capRunes kept %d characters", len([]rune(got)))
	}
	if got := capBytes(long, MaxFindingBody); len(got) != MaxFindingBody || strings.ContainsRune(got, 0xFFFD) {
		t.Fatalf("capBytes kept %d bytes (broken character: %v)", len(got), strings.ContainsRune(got, 0xFFFD))
	}
	if got := capBytes(strings.Repeat("é", 5), 5); got != "éé" {
		t.Fatalf("capBytes cut mid-character: %q", got)
	}
	if v, redacted := screened(secretLike()+" tail", func(s string) string { return s }); v != nil || !redacted {
		t.Fatalf("secret text: %v %v", v, redacted)
	}
	if v, redacted := screened("", func(s string) string { return s }); v != nil || redacted {
		t.Fatalf("empty text: %v %v", v, redacted)
	}
}

func TestParseChangeInclude(t *testing.T) {
	if in, err := ParseChangeInclude(""); err != nil || in != AllChangeSections {
		t.Fatalf("empty: %+v %v", in, err)
	}
	if in, err := ParseChangeInclude("checks,threads"); err != nil || in != (ChangeInclude{Checks: true, Threads: true}) {
		t.Fatalf("checks,threads: %+v %v", in, err)
	}
	for _, bad := range []string{"checks,", "diff", "checks,,gate"} {
		if _, err := ParseChangeInclude(bad); !errors.Is(err, ErrInvalidChangeWrite) {
			t.Fatalf("%q: err=%v", bad, err)
		}
	}
}

// ---- database ----

type stateFixture struct {
	s    *Store
	ref  ChangeRef
	base time.Time
}

func newStateFixture(t *testing.T, name string) stateFixture {
	t.Helper()
	s := schemaStore(t, "change_state_"+name)
	return stateFixture{s: s, ref: ChangeRef{Repo: "example/" + name, PR: 1}, base: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)}
}

func (f stateFixture) at(minutes int) time.Time {
	return f.base.Add(time.Duration(minutes) * time.Minute)
}

func (f stateFixture) apply(t *testing.T, b ChangeBatch) ChangeWriteResult {
	t.Helper()
	b.ChangeRef = f.ref
	res, err := f.s.ApplyChangeBatch(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (f stateFixture) view(t *testing.T, opt ChangeReadOptions) ChangeView {
	t.Helper()
	if opt.Include == (ChangeInclude{}) {
		opt.Include = AllChangeSections
	}
	v, found, err := f.s.GetChangeState(t.Context(), f.ref, opt)
	if err != nil || !found {
		t.Fatalf("GetChangeState: found=%v err=%v", found, err)
	}
	return v
}

func (f stateFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.s.pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestChangeStateMigration(t *testing.T) {
	s := schemaStore(t, "change_state_mig")
	ctx := t.Context()
	var version int
	if err := s.pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("max(version)=%d err=%v, want %d", version, err, SchemaVersion)
	}
	for _, table := range []string{"change_checks", "change_threads", "change_findings"} {
		var found bool
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass(format('%I.%I', current_schema(), $1::text)) IS NOT NULL`, table).Scan(&found); err != nil || !found {
			t.Fatalf("%s: found=%v err=%v", table, found, err)
		}
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='changes' AND column_name IN
('draft','base_ref','title','author','state_observed_at','state_source','gate_state','gate_head','gate_reasons','gate_decided_at','gate_policy_revision','checks_observed_at','threads_observed_at')`).Scan(&n); err != nil || n != 13 {
		t.Fatalf("changes columns: %d err=%v", n, err)
	}
}

// Per section: a newer write applies, an older one is ignored and leaves the
// stored row as it was, an equal one changes nothing.
func TestChangeSectionStaleness(t *testing.T) {
	f := newStateFixture(t, "stale")
	ctx := t.Context()
	t.Run("state", func(t *testing.T) {
		if r := f.apply(t, ChangeBatch{State: ptr(validState(f.at(10)))}); !r.Sections["state"].Applied {
			t.Fatalf("first write: %+v", r)
		}
		var updated0 time.Time
		if err := f.s.pool.QueryRow(ctx, `SELECT updated_at FROM changes WHERE id=(SELECT id FROM changes WHERE repo=$1 AND pr_number=$2)`, f.ref.Repo, f.ref.PR).Scan(&updated0); err != nil {
			t.Fatal(err)
		}
		equal := f.apply(t, ChangeBatch{State: ptr(validState(f.at(10)))})
		if !equal.Sections["state"].Applied {
			t.Fatalf("equal write: %+v", equal)
		}
		var updated1 time.Time
		if err := f.s.pool.QueryRow(ctx, `SELECT updated_at FROM changes WHERE id=$1`, equal.ChangeID).Scan(&updated1); err != nil || !updated1.Equal(updated0) {
			t.Fatalf("an equal write touched the row: %v -> %v (%v)", updated0, updated1, err)
		}
		old := validState(f.at(5))
		old.Title, old.State, old.Source = "older title", ChangeClosed, "poll"
		r := f.apply(t, ChangeBatch{State: &old})
		if got := r.Sections["state"]; got.Applied || got.Reason != "stale" {
			t.Fatalf("older write: %+v", got)
		}
		v := f.view(t, ChangeReadOptions{})
		if *v.Change.Title != "Add PR state" || v.Change.State != ChangeOpen || *v.Change.Source != "webhook" {
			t.Fatalf("an older write changed the row: %+v", v.Change)
		}
		newer := validState(f.at(20))
		newer.Title, newer.Source = "newer title", "poll"
		if r = f.apply(t, ChangeBatch{State: &newer}); !r.Sections["state"].Applied {
			t.Fatalf("newer write: %+v", r)
		}
		if v = f.view(t, ChangeReadOptions{}); *v.Change.Title != "newer title" || *v.Change.Source != "poll" || !v.Change.ObservedAt.Equal(f.at(20)) {
			t.Fatalf("newer write not stored: %+v", v.Change)
		}
	})
	t.Run("checks", func(t *testing.T) {
		w := func(min int, state string) *ChangeChecksWrite {
			return &ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: f.at(min), Source: "webhook", Checks: []ChangeCheckIn{{Name: "ci", State: state}}}
		}
		f.apply(t, ChangeBatch{Checks: w(10, "queued")})
		if got := f.apply(t, ChangeBatch{Checks: w(5, "completed")}).Sections["checks"]; got.Applied || got.Reason != "stale" {
			t.Fatalf("older: %+v", got)
		}
		if st := *f.view(t, ChangeReadOptions{}).Checks; len(st) != 1 || st[0].State != "queued" {
			t.Fatalf("older write changed a check: %+v", st)
		}
		if got := f.apply(t, ChangeBatch{Checks: w(20, "completed")}).Sections["checks"]; !got.Applied {
			t.Fatalf("newer: %+v", got)
		}
		if st := *f.view(t, ChangeReadOptions{}).Checks; st[0].State != "completed" {
			t.Fatalf("newer write not stored: %+v", st)
		}
	})
	t.Run("threads", func(t *testing.T) {
		w := func(min int, resolved bool) *ChangeThreadsWrite {
			return &ChangeThreadsWrite{ObservedAt: f.at(min), Source: "poll", Threads: []ChangeThreadIn{{ThreadID: "T1", AuthorKind: "human", Resolved: resolved}}}
		}
		f.apply(t, ChangeBatch{Threads: w(10, false)})
		if got := f.apply(t, ChangeBatch{Threads: w(5, true)}).Sections["threads"]; got.Applied || got.Reason != "stale" {
			t.Fatalf("older: %+v", got)
		}
		if th := *f.view(t, ChangeReadOptions{}).Threads; th[0].Resolved {
			t.Fatal("older write resolved a thread")
		}
		f.apply(t, ChangeBatch{Threads: w(20, true)})
		if th := *f.view(t, ChangeReadOptions{}).Threads; !th[0].Resolved {
			t.Fatal("newer write did not resolve the thread")
		}
	})
	t.Run("gate", func(t *testing.T) {
		w := func(min int, state string) *ChangeGateWrite {
			return &ChangeGateWrite{HeadSHA: testSHA1, State: state, Reasons: []string{"ci_pending"}, PolicyRevision: "abcdef1234567", DecidedAt: f.at(min)}
		}
		f.apply(t, ChangeBatch{Gate: w(10, "pending")})
		if got := f.apply(t, ChangeBatch{Gate: w(5, "ready")}).Sections["gate"]; got.Applied || got.Reason != "stale" {
			t.Fatalf("older: %+v", got)
		}
		if g := f.view(t, ChangeReadOptions{}).Gate; g.State != "pending" || !g.Current || *g.PolicyRevision != "abcdef1234567" || len(g.Reasons) != 1 {
			t.Fatalf("gate: %+v", g)
		}
		f.apply(t, ChangeBatch{Gate: w(20, "ready")})
		if g := f.view(t, ChangeReadOptions{}).Gate; g.State != "ready" {
			t.Fatalf("gate after newer: %+v", g)
		}
	})
	t.Run("findings", func(t *testing.T) {
		w := func(min, pass int) *ChangeFindingsWrite {
			return &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: pass, Lens: "correctness", ObservedAt: f.at(min), Findings: []ChangeFindingIn{{ID: "f", Severity: "high", Summary: "t"}}}
		}
		f.apply(t, ChangeBatch{Findings: w(10, 1)})
		if got := f.apply(t, ChangeBatch{Findings: w(5, 2)}).Sections["findings"]; got.Applied || got.Reason != "stale" {
			t.Fatalf("older: %+v", got)
		}
		if n := f.count(t, `SELECT count(*) FROM change_findings WHERE change_id=(SELECT id FROM changes WHERE repo=$1 AND pr_number=$2) AND pass=2`, f.ref.Repo, f.ref.PR); n != 0 {
			t.Fatalf("a stale findings write inserted %d rows", n)
		}
		if got := f.apply(t, ChangeBatch{Findings: w(20, 2)}).Sections["findings"]; !got.Applied {
			t.Fatalf("newer: %+v", got)
		}
	})
	t.Run("sections are independent", func(t *testing.T) {
		// state is at 20, gate at 20: a batch stale for state still applies its other sections.
		old := validState(f.at(1))
		res := f.apply(t, ChangeBatch{State: &old, Gate: &ChangeGateWrite{HeadSHA: testSHA1, State: "blocked", DecidedAt: f.at(30)}})
		if res.Sections["state"].Applied || !res.Sections["gate"].Applied {
			t.Fatalf("batch: %+v", res.Sections)
		}
	})
}

func TestChangeChecksCompleteMarksGone(t *testing.T) {
	f := newStateFixture(t, "gone")
	w := func(min int, complete bool, names ...string) ChangeBatch {
		cs := make([]ChangeCheckIn, len(names))
		for i, n := range names {
			cs[i] = ChangeCheckIn{Name: n, State: "completed", Conclusion: "success", Required: boolPtr(true)}
		}
		return ChangeBatch{Checks: &ChangeChecksWrite{HeadSHA: testSHA1, Checks: cs, ObservedAt: f.at(min), Source: "poll", Complete: complete}}
	}
	f.apply(t, w(1, true, "build", "lint", "test"))
	if n := len(*f.view(t, ChangeReadOptions{}).Checks); n != 3 {
		t.Fatalf("checks=%d", n)
	}
	// A partial write never marks anything gone.
	f.apply(t, w(2, false, "build"))
	if n := len(*f.view(t, ChangeReadOptions{}).Checks); n != 3 {
		t.Fatalf("a partial write dropped checks: %d", n)
	}
	// A complete write with one name missing marks it gone, and only it.
	f.apply(t, w(3, true, "build", "test"))
	names := []string{}
	for _, c := range *f.view(t, ChangeReadOptions{}).Checks {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "build,test" {
		t.Fatalf("checks after complete write: %v", names)
	}
	if n := f.count(t, `SELECT count(*) FROM change_checks WHERE name='lint' AND gone`); n != 1 {
		t.Fatalf("lint gone rows=%d", n)
	}
	// A gone check that reports again is back; repeating the same complete write changes nothing.
	f.apply(t, w(4, true, "build", "lint", "test"))
	if n := len(*f.view(t, ChangeReadOptions{}).Checks); n != 3 {
		t.Fatalf("a returning check stayed gone: %d", n)
	}
	// complete with an empty set marks all of that head gone; other heads keep theirs.
	f.apply(t, ChangeBatch{Checks: &ChangeChecksWrite{HeadSHA: testSHA2, ObservedAt: f.at(5), Source: "poll", Checks: []ChangeCheckIn{{Name: "build", State: "queued"}}}})
	f.apply(t, ChangeBatch{Checks: &ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: f.at(6), Source: "poll", Complete: true, Checks: []ChangeCheckIn{}}})
	if n := f.count(t, `SELECT count(*) FROM change_checks WHERE head_sha=$1 AND NOT gone`, testSHA1); n != 0 {
		t.Fatalf("empty complete set left %d checks", n)
	}
	if n := f.count(t, `SELECT count(*) FROM change_checks WHERE head_sha=$1 AND NOT gone`, testSHA2); n != 1 {
		t.Fatalf("another head's checks were touched: %d", n)
	}
}

func TestChangeThreadsCompleteMarksGone(t *testing.T) {
	f := newStateFixture(t, "threadgone")
	th := func(id string) ChangeThreadIn { return ChangeThreadIn{ThreadID: id, AuthorKind: "bot"} }
	f.apply(t, ChangeBatch{Threads: &ChangeThreadsWrite{ObservedAt: f.at(1), Source: "poll", Complete: true, Threads: []ChangeThreadIn{th("A"), th("B")}}})
	f.apply(t, ChangeBatch{Threads: &ChangeThreadsWrite{ObservedAt: f.at(2), Source: "poll", Complete: true, Threads: []ChangeThreadIn{th("A")}}})
	got := *f.view(t, ChangeReadOptions{}).Threads
	if len(got) != 1 || got[0].ThreadID != "A" {
		t.Fatalf("threads: %+v", got)
	}
	if n := f.count(t, `SELECT count(*) FROM change_threads WHERE thread_id='B' AND gone`); n != 1 {
		t.Fatalf("B gone rows=%d", n)
	}
}

// One bad section fails the whole batch: no row, no child rows.
func TestChangeBatchAtomic(t *testing.T) {
	f := newStateFixture(t, "atomic")
	good := validState(f.at(1))
	b := ChangeBatch{ChangeRef: f.ref, State: &good,
		Checks:  &ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: f.at(1), Source: "poll", Checks: []ChangeCheckIn{{Name: "ci", State: "queued"}}},
		Threads: &ChangeThreadsWrite{ObservedAt: f.at(1), Source: "poll", Threads: []ChangeThreadIn{{ThreadID: "T", AuthorKind: "nobody"}}}}
	_, err := f.s.ApplyChangeBatch(t.Context(), b)
	if !errors.Is(err, ErrInvalidChangeWrite) || !strings.Contains(err.Error(), "threads: ") {
		t.Fatalf("err=%v", err)
	}
	if _, found, err := f.s.GetChangeState(t.Context(), f.ref, ChangeReadOptions{Include: AllChangeSections}); err != nil || found {
		t.Fatalf("a refused batch left a change: found=%v err=%v", found, err)
	}
	if n := f.count(t, `SELECT count(*) FROM change_checks`); n != 0 {
		t.Fatalf("a refused batch left %d checks", n)
	}
	// The same batch with the bad section fixed writes everything at once.
	b.Threads.Threads[0].AuthorKind = "human"
	res, err := f.s.ApplyChangeBatch(t.Context(), b)
	if err != nil || len(res.Sections) != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	for name, sec := range res.Sections {
		if !sec.Applied {
			t.Fatalf("%s not applied: %+v", name, sec)
		}
	}
	// A database failure after the first section rolls the earlier ones back
	// too: a findings write whose head violates nothing in the validator but
	// the reviews insert is forced to fail by a clashing check constraint.
	g := f
	g.ref.PR = 2 // a second PR in the same schema: a second schemaStore would clash on pg_trgm (hk 3196)
	if _, err = g.s.pool.Exec(t.Context(), `ALTER TABLE change_findings ADD CONSTRAINT never CHECK(false)`); err != nil {
		t.Fatal(err)
	}
	st := validState(g.at(1))
	_, err = g.s.ApplyChangeBatch(t.Context(), ChangeBatch{ChangeRef: g.ref, State: &st,
		Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Lens: "x", ObservedAt: g.at(1), Findings: []ChangeFindingIn{{ID: "f", Severity: "low"}}}})
	if err == nil {
		t.Fatal("a failing findings insert was accepted")
	}
	if _, found, _ := g.s.GetChangeState(t.Context(), g.ref, ChangeReadOptions{Include: AllChangeSections}); found {
		t.Fatal("the state section survived a failed batch")
	}
}

func TestChangeStateSemantics(t *testing.T) {
	f := newStateFixture(t, "semantics")
	// A checks write for an unseen PR creates an open stub at its head.
	f.apply(t, ChangeBatch{Checks: &ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: f.at(1), Source: "webhook", Checks: []ChangeCheckIn{{Name: "ci", State: "queued"}}}})
	v := f.view(t, ChangeReadOptions{})
	if v.Change.State != ChangeOpen || *v.Change.HeadSHA != testSHA1 || v.Change.ObservedAt != nil || v.Freshness.State != nil || v.Freshness.Checks == nil {
		t.Fatalf("stub: %+v freshness=%+v", v.Change, v.Freshness)
	}
	// The state write fills it; omitted optional fields keep their value on the next one.
	f.apply(t, ChangeBatch{State: ptr(validState(f.at(2)))})
	again := ChangeStateWrite{HeadSHA: testSHA3, State: ChangeOpen, ObservedAt: f.at(3), Source: "poll"}
	f.apply(t, ChangeBatch{State: &again})
	v = f.view(t, ChangeReadOptions{})
	if *v.Change.HeadSHA != testSHA3 || *v.Change.Title != "Add PR state" || *v.Change.BaseSHA != testSHA2 || *v.Change.BaseRef != "main" || *v.Change.Author != "octo" || *v.Change.Draft {
		t.Fatalf("omitted fields were not kept: %+v", v.Change)
	}
	// Merged is terminal: head, base and state stay when a later write says open.
	merged := ChangeStateWrite{HeadSHA: testSHA3, State: ChangeMerged, MergeSHA: testSHA2, ObservedAt: f.at(4), Source: "webhook"}
	f.apply(t, ChangeBatch{State: &merged})
	reopen := ChangeStateWrite{HeadSHA: testSHA1, State: ChangeOpen, Title: "late title", ObservedAt: f.at(5), Source: "poll"}
	f.apply(t, ChangeBatch{State: &reopen})
	v = f.view(t, ChangeReadOptions{})
	if v.Change.State != ChangeMerged || *v.Change.HeadSHA != testSHA3 || *v.Change.MergeSHA != testSHA2 || v.Change.MergedAt == nil || *v.Change.Title != "late title" {
		t.Fatalf("merged change: %+v", v.Change)
	}
	// A secret-like title is stored as NULL and reported.
	bad := ChangeStateWrite{HeadSHA: testSHA3, State: ChangeMerged, Title: secretLike(), ObservedAt: f.at(6), Source: "poll"}
	if r := f.apply(t, ChangeBatch{State: &bad}); !r.Sections["state"].Applied || !r.Sections["state"].Redacted {
		t.Fatalf("redacted title: %+v", r.Sections)
	}
	if v = f.view(t, ChangeReadOptions{}); v.Change.Title != nil {
		t.Fatalf("title kept: %q", *v.Change.Title)
	}
}

func TestChangeGuardAndCaps(t *testing.T) {
	f := newStateFixture(t, "guard")
	long := strings.Repeat("é", 2500)
	threads := &ChangeThreadsWrite{ObservedAt: f.at(1), Source: "poll", Threads: []ChangeThreadIn{
		{ThreadID: "secret", AuthorKind: "human", Excerpt: "token " + secretLike()},
		{ThreadID: "long", AuthorKind: "human", Excerpt: long, Line: intPtr(12), CommentCount: 3},
		{ThreadID: "plain", AuthorKind: "mergewake", Severity: "high", Excerpt: "fine"},
	}}
	res := f.apply(t, ChangeBatch{Threads: threads})
	if !res.Sections["threads"].Applied || !res.Sections["threads"].Redacted {
		t.Fatalf("threads: %+v", res.Sections["threads"])
	}
	byID := map[string]ChangeThread{}
	for _, th := range *f.view(t, ChangeReadOptions{}).Threads {
		byID[th.ThreadID] = th
	}
	if s := byID["secret"]; s.Excerpt != nil || !s.Redacted {
		t.Fatalf("secret thread: %+v", s)
	}
	if l := byID["long"]; l.Excerpt == nil || len([]rune(*l.Excerpt)) != MaxThreadExcerpt || *l.Line != 12 {
		t.Fatalf("long thread: %+v", l)
	}
	if p := byID["plain"]; *p.Excerpt != "fine" || p.Redacted {
		t.Fatalf("plain thread: %+v", p)
	}
	findings := &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness", ObservedAt: f.at(2), Findings: []ChangeFindingIn{
		{ID: "a", Severity: "high", Summary: secretLike(), Body: "b"},
		{ID: "b", Severity: "low", Summary: strings.Repeat("s", 300), Body: strings.Repeat("x", MaxFindingBody+500)},
	}}
	if r := f.apply(t, ChangeBatch{Findings: findings}); !r.Sections["findings"].Redacted {
		t.Fatalf("findings: %+v", r.Sections["findings"])
	}
	got := *f.view(t, ChangeReadOptions{}).Findings
	var secret, longBody *ChangeFinding
	for i := range got {
		switch got[i].Ordinal {
		case 0:
			secret = &got[i]
		case 1:
			longBody = &got[i]
		}
	}
	if secret.Summary != nil || !secret.Redacted || secret.Body == nil || *secret.Body != "b" {
		t.Fatalf("secret finding: %+v", secret)
	}
	if longBody.Summary == nil || len([]rune(*longBody.Summary)) != MaxFindingSummary {
		t.Fatalf("summary not capped at %d characters: %+v", MaxFindingSummary, longBody)
	}
	if longBody.Body == nil || len(*longBody.Body) != MaxFindingBody || longBody.Redacted {
		t.Fatalf("long finding: %+v", longBody)
	}
}

func TestChangeFindingsLinkReviews(t *testing.T) {
	f := newStateFixture(t, "link")
	ctx := t.Context()
	w := func(min, pass int, n int) ChangeBatch {
		fs := make([]ChangeFindingIn, n)
		for i := range fs {
			fs[i] = ChangeFindingIn{ID: "e", Severity: "medium", Summary: "t"}
		}
		return ChangeBatch{Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: pass, Lens: "correctness", ObservedAt: f.at(min), Findings: fs}}
	}
	first := f.apply(t, w(1, 1, 2))
	if n := f.count(t, `SELECT count(*) FROM reviews WHERE change_id=$1`, first.ChangeID); n != 1 {
		t.Fatalf("reviews=%d", n)
	}
	var reviewID int64
	var verdict *string
	if err := f.s.pool.QueryRow(ctx, `SELECT id,verdict FROM reviews WHERE change_id=$1`, first.ChangeID).Scan(&reviewID, &verdict); err != nil || verdict != nil {
		t.Fatalf("review: %v %v", verdict, err)
	}
	if n := f.count(t, `SELECT count(*) FROM change_findings WHERE review_id=$1`, reviewID); n != 2 {
		t.Fatalf("findings linked to the review=%d", n)
	}
	// Append-only: the same pass again changes nothing; a longer list appends the new ordinals.
	f.apply(t, w(2, 1, 2))
	f.apply(t, w(3, 1, 3))
	if n := f.count(t, `SELECT count(*) FROM change_findings WHERE review_id=$1`, reviewID); n != 3 {
		t.Fatalf("findings after replay and growth=%d", n)
	}
	// A pass with no findings still creates its review row.
	f.apply(t, w(4, 2, 0))
	if n := f.count(t, `SELECT count(*) FROM reviews WHERE change_id=$1`, first.ChangeID); n != 2 {
		t.Fatalf("reviews after an empty pass=%d", n)
	}
	// A review a task event wrote first is linked, not replaced.
	tx, err := f.s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rv := TaskEventReview{ReviewedHead: testSHA1, Round: 2, Pass: 1, Lens: "security", Verdict: "pass", Family: "openai"}
	if err = insertReviewTx(ctx, tx, first.ChangeID, rv, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	preID := f.count(t, `SELECT id FROM reviews WHERE change_id=$1 AND lens='security'`, first.ChangeID)
	f.apply(t, ChangeBatch{Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 2, Pass: 1, Lens: "security", ObservedAt: f.at(5), Findings: []ChangeFindingIn{{ID: "g", Severity: "high"}}}})
	if n := f.count(t, `SELECT count(*) FROM change_findings WHERE review_id=$1`, preID); n != 1 {
		t.Fatalf("finding not linked to the existing review: %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM reviews WHERE change_id=$1 AND lens='security' AND verdict='pass' AND family='openai'`, first.ChangeID); n != 1 {
		t.Fatal("a findings write altered the task event's review")
	}
}

// A review row that only a findings write created is filled in by the first
// task event that reports the pass; a row that carries data is not touched.
func TestInsertReviewFillsBareRow(t *testing.T) {
	f := newStateFixture(t, "bare")
	ctx := t.Context()
	res := f.apply(t, ChangeBatch{Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness", ObservedAt: f.at(1)}})
	insert := func(rv TaskEventReview) {
		t.Helper()
		tx, err := f.s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err = insertReviewTx(ctx, tx, res.ChangeID, rv, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	key := TaskEventReview{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness"}
	first := key
	first.Verdict, first.Family = "changes_requested", "anthropic"
	insert(first)
	second := key
	second.Verdict, second.Family = "pass", "openai"
	insert(second)
	var verdict, family string
	if err := f.s.pool.QueryRow(ctx, `SELECT verdict,family FROM reviews WHERE change_id=$1`, res.ChangeID).Scan(&verdict, &family); err != nil {
		t.Fatal(err)
	}
	if verdict != "changes_requested" || family != "anthropic" {
		t.Fatalf("review=%s/%s: the first report must win", verdict, family)
	}
	if n := f.count(t, `SELECT count(*) FROM reviews WHERE change_id=$1`, res.ChangeID); n != 1 {
		t.Fatalf("reviews=%d", n)
	}
}

func TestChangeFindingStatusDerived(t *testing.T) {
	f := newStateFixture(t, "derived")
	f.apply(t, ChangeBatch{State: ptr(validState(f.at(1)))}) // head = testSHA1
	fs := func(head string, round int, threads ...string) *ChangeFindingsWrite {
		w := &ChangeFindingsWrite{ReviewedHead: head, Round: round, Lens: "correctness", ObservedAt: f.at(2 + round)}
		for _, id := range threads {
			w.Findings = append(w.Findings, ChangeFindingIn{ID: id, Severity: "high", Summary: id})
		}
		return w
	}
	// Round 1 on the current head: one open thread, one resolved, one unposted, one whose thread row is gone, one whose thread row does not exist yet.
	w := fs(testSHA1, 1, "open-thread", "resolved-thread", "unposted", "gone-thread", "later-thread")
	// A finding whose id is not the thread's names its thread explicitly.
	w.Findings = append(w.Findings, ChangeFindingIn{ID: "per-pass-7", Severity: "low", ThreadID: "resolved-thread"})
	f.apply(t, ChangeBatch{Findings: w, Threads: &ChangeThreadsWrite{ObservedAt: f.at(2), Source: "poll", Complete: false, Threads: []ChangeThreadIn{
		{ThreadID: "open-thread", AuthorKind: "mergewake", Replied: true},
		{ThreadID: "resolved-thread", AuthorKind: "mergewake", Resolved: true},
		{ThreadID: "gone-thread", AuthorKind: "mergewake"},
	}}})
	f.apply(t, ChangeBatch{Threads: &ChangeThreadsWrite{ObservedAt: f.at(3), Source: "poll", Complete: true, Threads: []ChangeThreadIn{
		{ThreadID: "open-thread", AuthorKind: "mergewake", Replied: true},
		{ThreadID: "resolved-thread", AuthorKind: "mergewake", Resolved: true},
	}}})
	status := func() map[string]ChangeFinding {
		out := map[string]ChangeFinding{}
		for _, x := range *f.view(t, ChangeReadOptions{}).Findings {
			out[x.ID] = x
		}
		return out
	}
	got := status()
	want := map[string]string{"open-thread": "open", "resolved-thread": "resolved", "unposted": "open", "gone-thread": "resolved", "later-thread": "open", "per-pass-7": "resolved"}
	for title, st := range want {
		if got[title].Status != st {
			t.Errorf("%s: status %q, want %q", title, got[title].Status, st)
		}
	}
	if !got["open-thread"].AuthorReplied || got["unposted"].AuthorReplied || got["open-thread"].ThreadID == nil || *got["open-thread"].ThreadID != "open-thread" {
		t.Errorf("replied flags: %+v", got)
	}
	// The head moves: findings with no live thread on the old head are superseded; thread-linked ones follow their thread.
	f.apply(t, ChangeBatch{State: &ChangeStateWrite{HeadSHA: testSHA2, State: ChangeOpen, ObservedAt: f.at(10), Source: "webhook"}})
	got = status()
	want = map[string]string{"open-thread": "open", "resolved-thread": "resolved", "unposted": "superseded", "gone-thread": "resolved", "later-thread": "superseded", "per-pass-7": "resolved"}
	for title, st := range want {
		if got[title].Status != st {
			t.Errorf("after head move, %s: status %q, want %q", title, got[title].Status, st)
		}
	}
	// A new round on the new head is open.
	nw := fs(testSHA2, 2)
	nw.Findings = []ChangeFindingIn{{ID: "fresh", Severity: "low"}}
	f.apply(t, ChangeBatch{Findings: nw})
	if got = status(); got["fresh"].Status != "open" {
		t.Errorf("fresh: %+v", got["fresh"])
	}
	// Open findings come first, newest pass first within each group.
	list := *f.view(t, ChangeReadOptions{}).Findings
	seenClosed := false
	for _, x := range list {
		if x.Status != "open" {
			seenClosed = true
		} else if seenClosed {
			t.Fatalf("an open finding follows a closed one: %+v", list)
		}
	}
	if list[0].ID != "fresh" {
		t.Errorf("newest open finding is not first: %q", list[0].ID)
	}
	// The list counts use the same derivation.
	cs, err := f.s.ListChangesByPR(t.Context(), repoPR(f.ref.Repo, f.ref.PR), 10)
	if err != nil || len(cs) != 1 {
		t.Fatalf("list: %v %v", cs, err)
	}
	if cs[0].OpenFindings != 2 || cs[0].OpenThreads != 1 {
		t.Fatalf("counts: findings=%d threads=%d, want 2 and 1", cs[0].OpenFindings, cs[0].OpenThreads)
	}
}

func TestChangeReadShape(t *testing.T) {
	f := newStateFixture(t, "shape")
	if _, found, err := f.s.GetChangeState(t.Context(), f.ref, ChangeReadOptions{Include: AllChangeSections}); err != nil || found {
		t.Fatalf("unseen PR: found=%v err=%v", found, err)
	}
	if _, _, err := f.s.GetChangeState(t.Context(), ChangeRef{Repo: "bad", PR: 1}, ChangeReadOptions{}); !errors.Is(err, ErrInvalidChangeWrite) {
		t.Fatalf("bad ref: %v", err)
	}
	f.apply(t, ChangeBatch{
		State: ptr(validState(f.at(1))),
		Checks: &ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: f.at(2), Source: "webhook", Checks: []ChangeCheckIn{
			{Name: "b-test", State: "completed", Conclusion: "failure"}, {Name: "a-build", State: "completed", Conclusion: "success", Required: boolPtr(true)}}},
		Threads: &ChangeThreadsWrite{ObservedAt: f.at(3), Source: "poll", Threads: []ChangeThreadIn{
			{ThreadID: "done", AuthorKind: "human", Resolved: true, LastCommentAt: ptr(f.at(9))},
			{ThreadID: "old", AuthorKind: "human", LastCommentAt: ptr(f.at(1))},
			{ThreadID: "new", AuthorKind: "human", LastCommentAt: ptr(f.at(8))}}},
		Gate: &ChangeGateWrite{HeadSHA: testSHA1, State: "blocked", Reasons: []string{"ci_failed", "threads_open"}, DecidedAt: f.at(4)},
	})
	// An older head's checks arrive and the head moves on.
	f.apply(t, ChangeBatch{State: &ChangeStateWrite{HeadSHA: testSHA2, State: ChangeOpen, ObservedAt: f.at(5), Source: "webhook"},
		Checks: &ChangeChecksWrite{HeadSHA: testSHA2, ObservedAt: f.at(6), Source: "webhook", Checks: []ChangeCheckIn{{Name: "a-build", State: "queued"}}}})
	v := f.view(t, ChangeReadOptions{})
	if len(*v.Checks) != 1 || (*v.Checks)[0].HeadSHA != testSHA2 {
		t.Fatalf("current-head checks: %+v", *v.Checks)
	}
	if g := v.Gate; g.State != "blocked" || g.Current || g.Head != testSHA1 || strings.Join(g.Reasons, ",") != "ci_failed,threads_open" {
		t.Fatalf("gate after head move: %+v", g)
	}
	ids := []string{}
	for _, th := range *v.Threads {
		ids = append(ids, th.ThreadID)
	}
	if strings.Join(ids, ",") != "new,old,done" {
		t.Fatalf("thread order (unresolved first, newest comment first): %v", ids)
	}
	all := f.view(t, ChangeReadOptions{AllHeads: true})
	names := []string{}
	for _, c := range *all.Checks {
		names = append(names, c.HeadSHA[:1]+c.Name)
	}
	if strings.Join(names, ",") != "2a-build,1a-build,1b-test" {
		t.Fatalf("all_heads checks: %v", names)
	}
	if v.Freshness.State == nil || v.Freshness.Checks == nil || v.Freshness.Threads == nil || v.Freshness.Gate == nil || v.Freshness.Findings != nil {
		t.Fatalf("freshness: %+v", v.Freshness)
	}
	trim := f.view(t, ChangeReadOptions{Include: ChangeInclude{Checks: true}})
	if trim.Checks == nil || trim.Threads != nil || trim.Findings != nil || trim.Gate != nil || trim.Freshness.Gate == nil {
		t.Fatalf("include=checks: %+v", trim)
	}
	if v.Findings == nil || len(*v.Findings) != 0 {
		t.Fatalf("an included empty section is []: %v", v.Findings)
	}
}

func TestChangeListCarriesPRState(t *testing.T) {
	f := newStateFixture(t, "listrow")
	st := validState(f.at(5))
	st.Draft = boolPtr(true)
	f.apply(t, ChangeBatch{State: &st,
		Checks:  &ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: f.at(2), Source: "poll", Checks: []ChangeCheckIn{{Name: "ci", State: "queued"}}},
		Threads: &ChangeThreadsWrite{ObservedAt: f.at(7), Source: "poll", Threads: []ChangeThreadIn{{ThreadID: "T1", AuthorKind: "human"}, {ThreadID: "T2", AuthorKind: "human", Resolved: true}}},
		Gate:    &ChangeGateWrite{HeadSHA: testSHA1, State: "pending", DecidedAt: f.at(6)}})
	cs, err := f.s.ListChangesByPR(t.Context(), repoPR(f.ref.Repo, f.ref.PR), 10)
	if err != nil || len(cs) != 1 {
		t.Fatalf("list: %v %v", cs, err)
	}
	c := cs[0]
	if c.GateState == nil || *c.GateState != "pending" || c.Draft == nil || !*c.Draft || c.OpenThreads != 1 || c.OpenFindings != 0 {
		t.Fatalf("row: %+v", c)
	}
	if c.FreshnessMin == nil || !c.FreshnessMin.Equal(f.at(2)) {
		t.Fatalf("freshness_min=%v, want the oldest section (%v)", c.FreshnessMin, f.at(2))
	}
}

// A finding in mergewake's prstatus.Finding shape survives the store: every
// field comes back under the same name, the body is optional, and the thread
// and the finding's own author_replied both set the flag a reader sees.
func TestChangeFindingRoundTrip(t *testing.T) {
	f := newStateFixture(t, "roundtrip")
	f.apply(t, ChangeBatch{State: ptr(validState(f.at(1)))})
	in := ChangeFindingIn{ID: "PRRT_kwDOabc", Location: "internal/store/changes.go:120", Severity: "high", Blocking: true,
		Summary: "unchecked error on commit", URL: "https://github.com/example/app/pull/1#discussion_r1", AuthorReplied: true}
	f.apply(t, ChangeBatch{Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 2, Pass: 1, Lens: "correctness", ObservedAt: f.at(2),
		Findings: []ChangeFindingIn{in, {ID: "per-pass-2"}}}})
	got := *f.view(t, ChangeReadOptions{}).Findings
	if len(got) != 2 {
		t.Fatalf("findings=%d", len(got))
	}
	x := got[0]
	if x.ID != in.ID || deref(x.Location) != in.Location || deref(x.Severity) != in.Severity || !x.Blocking || deref(x.Summary) != in.Summary ||
		deref(x.URL) != in.URL || !x.AuthorReplied || x.Body != nil || x.Redacted || x.Status != "open" || x.Round != 2 || x.Pass != 1 || x.Lens != "correctness" || x.ReviewedHead != testSHA1 {
		t.Fatalf("round trip: %+v", x)
	}
	// The optional fields stay unset, and a blocking/author_replied false is not blocking.
	y := got[1]
	if y.ID != "per-pass-2" || y.Location != nil || y.Severity != nil || y.Summary != nil || y.URL != nil || y.Blocking || y.AuthorReplied || y.ThreadID != nil {
		t.Fatalf("bare finding: %+v", y)
	}
	// Once its thread arrives resolved, the finding is resolved; the thread's replied flag counts as the author's.
	f.apply(t, ChangeBatch{Threads: &ChangeThreadsWrite{ObservedAt: f.at(3), Source: "poll", Threads: []ChangeThreadIn{{ThreadID: "PRRT_kwDOabc", AuthorKind: "mergewake", Resolved: true, Replied: true}, {ThreadID: "per-pass-2", AuthorKind: "mergewake", Replied: true}}}})
	got = *f.view(t, ChangeReadOptions{}).Findings
	byID := map[string]ChangeFinding{got[0].ID: got[0], got[1].ID: got[1]}
	if byID["PRRT_kwDOabc"].Status != "resolved" || byID["per-pass-2"].Status != "open" || !byID["per-pass-2"].AuthorReplied || deref(byID["per-pass-2"].ThreadID) != "per-pass-2" {
		t.Fatalf("after thread: %+v", byID)
	}
}

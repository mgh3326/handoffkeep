package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ticketBody is a body with the five ticket sections.
func ticketBody(what string) string {
	return "## Current\n" + what + "\n\n## Desired\nd\n\n## Key interfaces\nk\n\n## Out of scope\no\n\n## Acceptance (closed contract)\n1. a\n"
}

func intPtr(v int) *int { return &v }

type intakeCounts struct{ tasks, relations, documents, sets int }

func countIntakeTables(t *testing.T, pool *pgxpool.Pool) intakeCounts {
	t.Helper()
	var c intakeCounts
	err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tasks), (SELECT count(*) FROM task_relations), (SELECT count(*) FROM documents), (SELECT count(*) FROM ticket_sets)`).Scan(&c.tasks, &c.relations, &c.documents, &c.sets)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func liveRelations(t *testing.T, s *Store, id int64) []TaskRelation {
	t.Helper()
	xs, err := s.ListTaskRelations(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return xs
}

func TestIntakeCreatesOnceAndAllowsUnsetKindOnlyInTriageOrDeclined(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	in := IntakeInput{Source: "dots", SourceRef: "issue-7/comment-3", Title: "follow up on the release notes", Body: "the body", CreatedBy: "tester"}
	x, created, err := s.IntakeTask(ctx, in)
	if err != nil || !created {
		t.Fatalf("intake created=%t err=%v", created, err)
	}
	if x.State != StateTriage || x.Kind != "" || x.Lane != IntakeLane || x.Source != "dots" || x.SourceRef != "issue-7/comment-3" || x.StartedTriageAt == nil || x.Project != nil {
		t.Fatalf("intake task=%+v", x)
	}
	doc, found, err := s.GetDocument(ctx, x.BodyDoc)
	if err != nil || !found || doc.Body != "the body" {
		t.Fatalf("body_doc=%q found=%t err=%v doc=%+v", x.BodyDoc, found, err, doc)
	}
	// A repeat, even with other content, returns the recorded task.
	in.Title = "a different title"
	again, created, err := s.IntakeTask(ctx, in)
	if err != nil || created || again.ID != x.ID || again.Title != x.Title {
		t.Fatalf("repeat created=%t err=%v task=%+v", created, err, again)
	}
	if c := countIntakeTables(t, pool); c.tasks != 1 || c.documents != 1 {
		t.Fatalf("repeat wrote rows: %+v", c)
	}
	// The unset kind is valid in triage only: the CHECK refuses it elsewhere.
	_, err = pool.Exec(ctx, `UPDATE tasks SET state='backlog' WHERE id=$1`, x.ID)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" || pg.ConstraintName != "tasks_kind_check" {
		t.Fatalf("kind '' outside triage err=%v, want tasks_kind_check", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at) VALUES('l','t','','backlog','tester',now(),now(),now())`)
	if !errors.As(err, &pg) || pg.ConstraintName != "tasks_kind_check" {
		t.Fatalf("insert kind '' in backlog err=%v, want tasks_kind_check", err)
	}
	// tasks add keeps requiring a kind, and leaves source to intake.
	if _, err = s.CreateTask(ctx, Task{Lane: "l", Title: "no kind", CreatedBy: "tester", Project: projectPtr(testProjectName)}); err == nil {
		t.Fatal("CreateTask without a kind succeeded")
	}
	if _, err = s.CreateTask(ctx, Task{Lane: "l", Title: "forged source", Kind: "fix", CreatedBy: "tester", Project: projectPtr(testProjectName), Source: "dots", SourceRef: "x"}); err == nil {
		t.Fatal("CreateTask with a source succeeded")
	}
	// Bad input writes nothing.
	for name, bad := range map[string]IntakeInput{
		"no source":    {SourceRef: "r", Title: "t", CreatedBy: "tester"},
		"no ref":       {Source: "dots", Title: "t", CreatedBy: "tester"},
		"bad kind":     {Source: "dots", SourceRef: "r2", Title: "t", Kind: "decision", CreatedBy: "tester"},
		"two lines":    {Source: "dots", SourceRef: "r3", Title: "a\nb", CreatedBy: "tester"},
		"bad project":  {Source: "dots", SourceRef: "r4", Title: "t", Project: projectPtr("not-a-project"), CreatedBy: "tester"},
		"neg priority": {Source: "dots", SourceRef: "r5", Title: "t", Priority: -1, CreatedBy: "tester"},
	} {
		if _, _, err := s.IntakeTask(ctx, bad); err == nil {
			t.Errorf("%s: intake succeeded", name)
		}
	}
	if c := countIntakeTables(t, pool); c.tasks != 1 {
		t.Fatalf("refused intake wrote tasks: %+v", c)
	}
}

func TestTriageGateOnIntakeTask(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	n := 0
	intake := func(in IntakeInput) Task {
		t.Helper()
		n++
		in.Source, in.SourceRef, in.CreatedBy = "linear", fmt.Sprintf("gate-%d", n), "tester"
		if in.Title == "" {
			in.Title = fmt.Sprintf("gate item %d", n)
		}
		x, _, err := s.IntakeTask(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	// Each missing field refuses the exit and leaves the task in triage.
	for name, in := range map[string]IntakeInput{
		"priority": {Kind: "fix", Project: projectPtr(testProjectName)},
		"project":  {Kind: "fix", Priority: 2},
		"kind":     {Priority: 2, Project: projectPtr(testProjectName)},
	} {
		x := intake(in)
		for _, to := range []string{StateBacklog, StateUnstarted} {
			if _, err := s.TransitionTaskWith(ctx, x.ID, TransitionInput{To: to, By: "desk"}); !errors.Is(err, ErrTriageGate) {
				t.Errorf("missing %s to %s: err=%v, want ErrTriageGate", name, to, err)
			}
		}
		if got := stateOf(t, s, x.ID); got.State != StateTriage || got.TriagedAt != nil {
			t.Errorf("missing %s: task left triage: %+v", name, got)
		}
	}
	// The exit can carry the values; the gate judges what the task leaves with.
	x := intake(IntakeInput{})
	if _, err := s.TransitionTaskWith(ctx, x.ID, TransitionInput{To: StateBacklog, By: "desk", Priority: intPtr(0), Kind: "fix", Project: projectPtr(testProjectName)}); !errors.Is(err, ErrTriageGate) {
		t.Fatalf("priority 0 err=%v, want ErrTriageGate", err)
	}
	got, err := s.TransitionTaskWith(ctx, x.ID, TransitionInput{To: StateUnstarted, By: "desk", Priority: intPtr(3), Kind: "fix", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateUnstarted || got.Priority != 3 || got.Kind != "fix" || got.Project == nil || *got.Project != testProjectName || got.TriagedAt == nil {
		t.Fatalf("accepted task=%+v", got)
	}
	ev := lastEvent(t, s, x.ID)
	if ev.Kind != TaskEventTransition || ev.From != StateTriage || ev.To != StateUnstarted || ev.By != "desk" {
		t.Fatalf("exit event=%+v", ev)
	}
	fields := map[string]string{}
	for _, e := range stateOf(t, s, x.ID).Events {
		if e.Field != "" {
			fields[e.Field] = e.FromValue + "->" + e.ToValue
		}
	}
	if fields["priority"] != "0->3" || fields["kind"] != "->fix" || fields["project"] != "->"+testProjectName {
		t.Fatalf("field events=%v", fields)
	}
	// Triage fields are refused anywhere but on the way out to backlog or unstarted.
	if _, err := s.TransitionTaskWith(ctx, x.ID, TransitionInput{To: StateBacklog, By: "desk", Priority: intPtr(4)}); err == nil {
		t.Fatal("priority on a non-triage transition succeeded")
	}
	if _, err := s.TransitionTaskWith(ctx, x.ID, TransitionInput{To: StateBacklog, By: "desk", Set: true}); err == nil {
		t.Fatal("set on a non-triage transition succeeded")
	}
	// A decline needs a reason; every decline sets triaged_at.
	other := flowTask(t, s, pool, "implement", StateBacklog, false)
	for _, to := range []string{StateDropped, StateDuplicate} {
		d := intake(IntakeInput{})
		in := TransitionInput{To: to, By: "desk"}
		if to == StateDuplicate {
			in.DuplicateOf = other.ID
		}
		if _, err := s.TransitionTaskWith(ctx, d.ID, in); !errors.Is(err, ErrTriageGate) || !strings.Contains(err.Error(), "reason") {
			t.Fatalf("decline to %s without reason err=%v", to, err)
		}
		in.Note = "not ours"
		declined, err := s.TransitionTaskWith(ctx, d.ID, in)
		if err != nil || declined.State != to || declined.TriagedAt == nil {
			t.Fatalf("decline to %s: %+v err=%v", to, declined, err)
		}
		if ev := lastEvent(t, s, d.ID); ev.By != "desk" || ev.Note != "not ours" {
			t.Fatalf("decline event=%+v", ev)
		}
		// The unset kind survives the decline, so the task cannot be reopened.
		if declined.Kind != "" {
			t.Fatalf("declined kind=%q", declined.Kind)
		}
		if _, err := s.ReopenTask(ctx, d.ID, "desk", "changed our mind"); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("reopen kind-less %s err=%v, want ErrTaskConflict", to, err)
		}
	}
	d := intake(IntakeInput{})
	if _, err := s.TransitionTaskWith(ctx, d.ID, TransitionInput{To: StateDropped, By: "desk", Note: "x", Kind: "fix"}); err == nil {
		t.Fatal("triage fields on a decline succeeded")
	}
}

func ticketSetFixture(ref string) TicketSetInput {
	return TicketSetInput{
		Source: "repo-session", SourceRef: ref, CreatedBy: "tester",
		Parent: Ticket{Title: "set parent " + ref, Body: ticketBody("parent")},
		Children: []Ticket{
			{Ref: "a", Title: "first slice " + ref, Body: ticketBody("a")},
			{Ref: "b", Title: "second slice " + ref, Body: ticketBody("b"), BlockedBy: []TicketBlocker{{Ref: "a"}}},
		},
	}
}

func TestTicketSetIsAtomicAndIdempotent(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	existing := flowTask(t, s, pool, "implement", StateBacklog, false)
	in := ticketSetFixture("set-1")
	in.Children = append(in.Children, Ticket{Ref: "c", Title: "third slice", Body: ticketBody("c"), BlockedBy: []TicketBlocker{{Ref: "b"}, {TaskID: existing.ID}}})
	before := countIntakeTables(t, pool)
	set, created, err := s.EmitTicketSet(ctx, in)
	if err != nil || !created {
		t.Fatalf("emit created=%t err=%v", created, err)
	}
	if len(set.Children) != 3 || set.ParentID < 1 {
		t.Fatalf("set=%+v", set)
	}
	after := countIntakeTables(t, pool)
	if after.tasks-before.tasks != 4 || after.documents-before.documents != 4 || after.sets-before.sets != 1 || after.relations-before.relations != 3 {
		t.Fatalf("emit wrote %+v -> %+v", before, after)
	}
	ids := map[string]int64{}
	for _, c := range set.Children {
		ids[c.Ref] = c.ID
		x := stateOf(t, s, c.ID)
		if x.State != StateTriage || x.ParentID == nil || *x.ParentID != set.ParentID || x.Source != "repo-session" || x.SourceRef != "set-1/"+c.Ref {
			t.Fatalf("child %s=%+v", c.Ref, x)
		}
		doc, found, err := s.GetDocument(ctx, x.BodyDoc)
		if err != nil || !found || !strings.Contains(doc.Body, "## Acceptance") {
			t.Fatalf("child %s body_doc=%q found=%t err=%v", c.Ref, x.BodyDoc, found, err)
		}
	}
	if p := stateOf(t, s, set.ParentID); p.State != StateTriage || p.SourceRef != "set-1" {
		t.Fatalf("parent=%+v", p)
	}
	wantEdges := map[[2]int64]bool{{ids["a"], ids["b"]}: true, {ids["b"], ids["c"]}: true, {existing.ID, ids["c"]}: true}
	for _, r := range append(liveRelations(t, s, ids["b"]), liveRelations(t, s, existing.ID)...) {
		if r.Kind == RelationBlocks {
			delete(wantEdges, [2]int64{r.SrcID, r.DstID})
		}
	}
	if len(wantEdges) != 0 {
		t.Fatalf("missing blocks edges %v", wantEdges)
	}
	// The same payload returns the same ids and writes nothing.
	again, created, err := s.EmitTicketSet(ctx, in)
	if err != nil || created || again.ParentID != set.ParentID || fmt.Sprint(again.Children) != fmt.Sprint(set.Children) {
		t.Fatalf("repeat created=%t err=%v set=%+v want %+v", created, err, again, set)
	}
	if c := countIntakeTables(t, pool); c != after {
		t.Fatalf("repeat wrote %+v -> %+v", after, c)
	}
	// A changed payload is a conflict and changes nothing.
	changed := in
	changed.Children = append([]Ticket(nil), in.Children...)
	changed.Children[0].Title = "first slice, renamed"
	if _, _, err := s.EmitTicketSet(ctx, changed); !errors.Is(err, ErrTicketSetConflict) {
		t.Fatalf("changed payload err=%v, want ErrTicketSetConflict", err)
	}
	if c := countIntakeTables(t, pool); c != after {
		t.Fatalf("conflict wrote %+v -> %+v", after, c)
	}
	if x := stateOf(t, s, ids["a"]); x.Title != "first slice set-1" {
		t.Fatalf("conflict changed a child: %+v", x)
	}

	// Refusals write nothing at all.
	refused := map[string]struct {
		in   TicketSetInput
		want error
	}{}
	missing := ticketSetFixture("set-missing")
	missing.Children[1].Body = strings.Replace(ticketBody("b"), "## Out of scope", "## Notes", 1)
	refused["missing heading"] = struct {
		in   TicketSetInput
		want error
	}{missing, ErrTicketSetInvalid}
	unknown := ticketSetFixture("set-unknown")
	unknown.Children[1].BlockedBy = []TicketBlocker{{Ref: "zz"}}
	refused["unknown ref"] = struct {
		in   TicketSetInput
		want error
	}{unknown, ErrTicketSetInvalid}
	cycle := ticketSetFixture("set-cycle")
	cycle.Children[0].BlockedBy = []TicketBlocker{{Ref: "b"}}
	refused["cycle"] = struct {
		in   TicketSetInput
		want error
	}{cycle, ErrRelationCycle}
	gone := ticketSetFixture("set-gone")
	gone.Children[1].BlockedBy = []TicketBlocker{{TaskID: 999999999}}
	refused["missing task id"] = struct {
		in   TicketSetInput
		want error
	}{gone, ErrTicketSetInvalid}
	badProject := ticketSetFixture("set-project")
	badProject.Children[1].Project = projectPtr("not-a-project")
	refused["unknown project"] = struct {
		in   TicketSetInput
		want error
	}{badProject, ErrTaskProjectUnknown}
	for name, tc := range refused {
		if _, _, err := s.EmitTicketSet(ctx, tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v, want %v", name, err, tc.want)
		}
		if c := countIntakeTables(t, pool); c != after {
			t.Errorf("%s wrote %+v -> %+v", name, after, c)
		}
	}
}

func TestMissingTicketSections(t *testing.T) {
	if got := missingTicketSections(ticketBody("x")); len(got) != 0 {
		t.Fatalf("full body lacks %v", got)
	}
	fenced := "```\n## Acceptance\n```\n## Current\n## Desired\n## Key interfaces\n## Out of scope\n"
	if got := missingTicketSections(fenced); fmt.Sprint(got) != "[Acceptance]" {
		t.Fatalf("fenced heading counted: missing=%v", got)
	}
	if got := missingTicketSections("#Current\n## Currently\n## Acceptance criteria\n"); fmt.Sprint(got) != "[Current Desired Key interfaces Out of scope]" {
		t.Fatalf("missing=%v", got)
	}
}

func TestTicketSetAcceptAndDeclineCascade(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	in := ticketSetFixture("accept")
	in.Children[0].Priority, in.Children[0].Kind = 7, "verify"
	in.Children = append(in.Children, Ticket{Ref: "c", Title: "declined alone", Body: ticketBody("c")})
	set, _, err := s.EmitTicketSet(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, c := range set.Children {
		ids[c.Ref] = c.ID
	}
	if _, err := s.TransitionTaskWith(ctx, ids["c"], TransitionInput{To: StateDropped, By: "desk", Note: "out of scope"}); err != nil {
		t.Fatal(err)
	}
	parent, err := s.TransitionTaskWith(ctx, set.ParentID, TransitionInput{To: StateBacklog, By: "desk", Set: true, Priority: intPtr(3), Kind: "implement", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	if parent.State != StateBacklog || parent.TriagedAt == nil {
		t.Fatalf("parent=%+v", parent)
	}
	a, b, c := stateOf(t, s, ids["a"]), stateOf(t, s, ids["b"]), stateOf(t, s, ids["c"])
	if a.State != StateBacklog || a.Priority != 7 || a.Kind != "verify" || a.Project == nil || *a.Project != testProjectName || a.TriagedAt == nil {
		t.Fatalf("a keeps its own values: %+v", a)
	}
	if b.State != StateBacklog || b.Priority != 3 || b.Kind != "implement" || b.Project == nil || *b.Project != testProjectName || b.TriagedAt == nil {
		t.Fatalf("b inherits: %+v", b)
	}
	if c.State != StateDropped {
		t.Fatalf("c left triage before the set and must stay dropped: %+v", c)
	}
	if ev := lastEvent(t, s, b.ID); ev.From != StateTriage || ev.To != StateBacklog || ev.By != "desk" {
		t.Fatalf("b exit event=%+v", ev)
	}

	// Declining a set's parent declines its children still in triage, with
	// or without set; a child already accepted stays where it is.
	decline := ticketSetFixture("decline")
	ds, _, err := s.EmitTicketSet(ctx, decline)
	if err != nil {
		t.Fatal(err)
	}
	accepted := ds.Children[0].ID
	if _, err := s.TransitionTaskWith(ctx, accepted, TransitionInput{To: StateUnstarted, By: "desk", Priority: intPtr(1), Kind: "fix", Project: projectPtr(testProjectName)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTaskWith(ctx, ds.ParentID, TransitionInput{To: StateDropped, By: "desk", Note: "not now"}); err != nil {
		t.Fatal(err)
	}
	if x := stateOf(t, s, accepted); x.State != StateUnstarted {
		t.Fatalf("accepted child=%+v", x)
	}
	dropped := stateOf(t, s, ds.Children[1].ID)
	if dropped.State != StateDropped || dropped.TriagedAt == nil || !strings.Contains(lastEvent(t, s, dropped.ID).Note, "not now") {
		t.Fatalf("triage child=%+v", dropped)
	}

	// An accept without set moves the parent alone.
	alone := ticketSetFixture("alone")
	as, _, err := s.EmitTicketSet(ctx, alone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTaskWith(ctx, as.ParentID, TransitionInput{To: StateBacklog, By: "desk", Priority: intPtr(2), Kind: "fix", Project: projectPtr(testProjectName)}); err != nil {
		t.Fatal(err)
	}
	for _, ch := range as.Children {
		if x := stateOf(t, s, ch.ID); x.State != StateTriage {
			t.Fatalf("accept without set moved child %+v", x)
		}
	}
}

func TestTriageListShowsASetAsOneRow(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	single, _, err := s.IntakeTask(ctx, IntakeInput{Source: "dots", SourceRef: "one", Title: "lone intake item", CreatedBy: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	in := ticketSetFixture("listed")
	in.Children[1].Title = "rotate the deploy signing key"
	set, _, err := s.EmitTicketSet(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	open, err := s.CreateTask(ctx, Task{Lane: "l", Title: "rotate the deploy signing keys", Kind: "ops", CreatedBy: "tester", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListTriage(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Task.ID != single.ID || rows[1].Task.ID != set.ParentID {
		t.Fatalf("rows=%+v", rows)
	}
	r := rows[1]
	if !r.TicketSet || r.ChildCount != 2 || r.ChildrenInTriage != 2 || r.Task.Title != "set parent listed" || r.AgeSeconds < 0 {
		t.Fatalf("set row=%+v", r)
	}
	if len(r.Similar) != 1 || r.Similar[0].ID != open.ID {
		t.Fatalf("set row similar=%+v, want #%d", r.Similar, open.ID)
	}
	if rows[0].TicketSet || rows[0].ChildCount != 0 {
		t.Fatalf("single row=%+v", rows[0])
	}
	// Once the parent leaves triage alone, its triage children are listed.
	if _, err := s.TransitionTaskWith(ctx, set.ParentID, TransitionInput{To: StateBacklog, By: "desk", Priority: intPtr(2), Kind: "fix", Project: projectPtr(testProjectName)}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListTriage(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("after accepting the parent alone rows=%d", len(rows))
	}
}

func TestDuplicateCheckAtCreate(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	base := func(title string) Task {
		return Task{Lane: "dup", Title: title, Kind: "fix", CreatedBy: "tester", Project: projectPtr(testProjectName)}
	}
	open, err := s.CreateTask(ctx, base("fix the login redirect loop"))
	if err != nil {
		t.Fatal(err)
	}
	// A dropped task with the same title is not a candidate.
	if _, err := pool.Exec(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at) VALUES('dup','fix the login redirect loop','fix','dropped','tester',now(),now(),now())`); err != nil {
		t.Fatal(err)
	}
	before := countIntakeTables(t, pool)
	_, err = s.CreateTaskChecked(ctx, base("Fix login redirect loop"), DuplicateCheck{})
	var similar *SimilarTasksError
	if !errors.As(err, &similar) || len(similar.Tasks) != 1 || similar.Tasks[0].ID != open.ID || similar.Tasks[0].Similarity < SimilarTaskThreshold {
		t.Fatalf("similar create err=%v", err)
	}
	if c := countIntakeTables(t, pool); c != before {
		t.Fatalf("refused create wrote %+v -> %+v", before, c)
	}
	linked, err := s.CreateTaskChecked(ctx, base("Fix login redirect loop"), DuplicateCheck{Link: open.ID})
	if err != nil {
		t.Fatal(err)
	}
	rels := liveRelations(t, s, linked.ID)
	if len(rels) != 1 || rels[0].Kind != RelationRelated || (rels[0].SrcID != open.ID && rels[0].DstID != open.ID) {
		t.Fatalf("link relations=%+v", rels)
	}
	if _, err := s.CreateTaskChecked(ctx, base("Fix login redirect loop"), DuplicateCheck{New: true}); err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := s.CreateTaskChecked(ctx, base("add a metrics endpoint for quota"), DuplicateCheck{}); err != nil {
		t.Fatalf("dissimilar title: %v", err)
	}
	if _, err := s.CreateTaskChecked(ctx, base("x"), DuplicateCheck{Link: 999999999}); err == nil {
		t.Fatal("link to a missing task succeeded")
	}
	if _, err := s.CreateTaskChecked(ctx, base("y"), DuplicateCheck{Link: open.ID, New: true}); err == nil {
		t.Fatal("link and new together succeeded")
	}
}

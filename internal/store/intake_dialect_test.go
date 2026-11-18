package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Dialect-neutral cases of the intake area (hk 5074, A3): IntakeTask,
// EmitTicketSet and ListTriage on PostgreSQL and MySQL. They run from
// tasks_area_cases_test.go.

const intakeTestBody = "## Current\nx\n## Desired\ny\n## Key interfaces\nz\n## Out of scope\nw\n## Acceptance\nok\n"

func init() {
	registerTaskAreaCase("intake/task-idempotent", func(t *testing.T, s *Store) {
		ctx := t.Context()
		in := IntakeInput{Source: "intake-test", SourceRef: "ref/1", Title: "Intake idempotent title", CreatedBy: "tester", Priority: 2}
		x, created, err := s.IntakeTask(ctx, in)
		if err != nil || !created {
			t.Fatalf("first intake = %+v created=%v err=%v", x, created, err)
		}
		if x.State != StateTriage || x.Lane != IntakeLane || x.Title != in.Title || x.Source != in.Source || x.SourceRef != in.SourceRef || x.CreatedBy != "tester" || x.Priority != 2 {
			t.Fatalf("first intake row = %+v", x)
		}
		if x.StartedTriageAt == nil || x.ParentID != nil || x.BodyDoc != "" {
			t.Fatalf("first intake triage clock, parent or body = %+v", x)
		}
		// A repeated (source, source_ref) returns the recorded task and changes nothing.
		changed := in
		changed.Title = "A different title"
		again, created, err := s.IntakeTask(ctx, changed)
		if err != nil || created || again.ID != x.ID || again.Title != in.Title {
			t.Fatalf("repeat intake = %+v created=%v err=%v, want task %d unchanged", again, created, err, x.ID)
		}
		other := in
		other.SourceRef = "ref/2"
		second, created, err := s.IntakeTask(ctx, other)
		if err != nil || !created || second.ID == x.ID {
			t.Fatalf("second ref = %+v created=%v err=%v", second, created, err)
		}
		// A task project the server does not know is refused and writes nothing.
		unknown := in
		unknown.SourceRef = "ref/3"
		unknown.Project = func() *string { p := "no-such-project"; return &p }()
		if _, _, err := s.IntakeTask(ctx, unknown); !errors.Is(err, ErrTaskProjectUnknown) {
			t.Fatalf("unknown project err = %v, want ErrTaskProjectUnknown", err)
		}
		unknown.Project = nil
		if _, created, err := s.IntakeTask(ctx, unknown); err != nil || !created {
			t.Fatalf("ref/3 after the refused project created=%v err=%v, want a clean create", created, err)
		}
	})

	registerTaskAreaCase("intake/task-body", func(t *testing.T, s *Store) {
		in := IntakeInput{Source: "intake-test", SourceRef: "body/1", Title: "Intake with a body", Body: "details", CreatedBy: "tester"}
		x, created, err := s.IntakeTask(t.Context(), in)
		if err != nil || !created || x.BodyDoc != fmt.Sprintf("task-body/%d", x.ID) {
			t.Fatalf("intake with a body = %+v created=%v err=%v", x, created, err)
		}
	})

	registerTaskAreaCase("intake/task-refusals", func(t *testing.T, s *Store) {
		ctx := t.Context()
		base := IntakeInput{Source: "intake-test", SourceRef: "refuse/1", Title: "A valid title", CreatedBy: "tester"}
		empty := ""
		for name, mutate := range map[string]func(*IntakeInput){
			"source with a space": func(in *IntakeInput) { in.Source = "bad source" },
			"empty source_ref":    func(in *IntakeInput) { in.SourceRef = "" },
			"two-line title":      func(in *IntakeInput) { in.Title = "one\ntwo" },
			"empty title":         func(in *IntakeInput) { in.Title = "" },
			"unknown kind":        func(in *IntakeInput) { in.Kind = "nonsense" },
			"negative priority":   func(in *IntakeInput) { in.Priority = -1 },
			"empty project":       func(in *IntakeInput) { in.Project = &empty },
			"empty created_by":    func(in *IntakeInput) { in.CreatedBy = "" },
		} {
			in := base
			mutate(&in)
			if _, created, err := s.IntakeTask(ctx, in); err == nil || created {
				t.Fatalf("%s: intake accepted (created=%v err=%v)", name, created, err)
			}
		}
		// Nothing was written: the valid item still creates.
		if _, created, err := s.IntakeTask(ctx, base); err != nil || !created {
			t.Fatalf("valid item after the refusals created=%v err=%v", created, err)
		}
	})

	registerTaskAreaCase("intake/task-concurrent", func(t *testing.T, s *Store) {
		in := IntakeInput{Source: "intake-test", SourceRef: "race/1", Title: "Concurrent delivery", CreatedBy: "tester"}
		const workers = 8
		type result struct {
			id      int64
			created bool
			err     error
		}
		results := make([]result, workers)
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				x, created, err := s.IntakeTask(t.Context(), in)
				results[i] = result{x.ID, created, err}
			}()
		}
		wg.Wait()
		creates := 0
		for _, r := range results {
			if r.err != nil {
				t.Fatalf("concurrent intake: %v", r.err)
			}
			if r.id != results[0].id {
				t.Fatalf("deliveries returned different tasks: %+v", results)
			}
			if r.created {
				creates++
			}
		}
		if creates != 1 {
			t.Fatalf("%d deliveries created the task, want exactly 1", creates)
		}
	})

	registerTaskAreaCase("intake/ticket-set", func(t *testing.T, s *Store) {
		ctx := t.Context()
		set := TicketSetInput{
			Source: "intake-test", SourceRef: "set/1", CreatedBy: "tester",
			Parent: Ticket{Title: "Set parent", Body: intakeTestBody},
			Children: []Ticket{
				{Ref: "a", Title: "Child a", Body: intakeTestBody},
				{Ref: "b", Title: "Child b", Body: intakeTestBody, BlockedBy: []TicketBlocker{{Ref: "a"}}},
			},
		}
		// Shape refusals happen before the database.
		bad := set
		bad.Parent = Ticket{Title: "No sections", Body: "text"}
		if _, _, err := s.EmitTicketSet(ctx, bad); !errors.Is(err, ErrTicketSetInvalid) {
			t.Fatalf("missing sections err = %v, want ErrTicketSetInvalid", err)
		}
		cycle := set
		cycle.Children = []Ticket{
			{Ref: "a", Title: "Child a", Body: intakeTestBody, BlockedBy: []TicketBlocker{{Ref: "b"}}},
			{Ref: "b", Title: "Child b", Body: intakeTestBody, BlockedBy: []TicketBlocker{{Ref: "a"}}},
		}
		if _, _, err := s.EmitTicketSet(ctx, cycle); !errors.Is(err, ErrRelationCycle) {
			t.Fatalf("cycle err = %v, want ErrRelationCycle", err)
		}
		// A blocker task that does not exist is refused after the lookup.
		ghost := set
		ghost.SourceRef = "set/ghost"
		ghost.Children = []Ticket{{Ref: "a", Title: "Child a", Body: intakeTestBody, BlockedBy: []TicketBlocker{{TaskID: 999999999}}}}
		if _, _, err := s.EmitTicketSet(ctx, ghost); !errors.Is(err, ErrTicketSetInvalid) {
			t.Fatalf("missing blocker err = %v, want ErrTicketSetInvalid", err)
		}
		// A task already holding a source_ref of the set is a conflict.
		held := set
		held.SourceRef = "set/held"
		if _, created, err := s.IntakeTask(ctx, IntakeInput{Source: "intake-test", SourceRef: "set/held/a", Title: "Holds the child ref", CreatedBy: "tester"}); err != nil || !created {
			t.Fatalf("holding task created=%v err=%v", created, err)
		}
		if _, _, err := s.EmitTicketSet(ctx, held); !errors.Is(err, ErrTicketSetConflict) {
			t.Fatalf("held source_ref err = %v, want ErrTicketSetConflict", err)
		}

		got, created, err := s.EmitTicketSet(ctx, set)
		if err != nil || !created || got.ID == 0 || got.ParentID == 0 || len(got.Children) != 2 || got.Children[0].Ref != "a" || got.Children[1].Ref != "b" {
			t.Fatalf("emit = %+v created=%v err=%v", got, created, err)
		}
		replay, created, err := s.EmitTicketSet(ctx, set)
		if err != nil || created || replay.ID != got.ID || replay.ParentID != got.ParentID || len(replay.Children) != 2 || replay.Children[0].ID != got.Children[0].ID || replay.Children[1].ID != got.Children[1].ID {
			t.Fatalf("replay = %+v created=%v err=%v, want %+v", replay, created, err, got)
		}
		changed := set
		changed.Parent.Title = "Set parent, changed"
		if _, _, err := s.EmitTicketSet(ctx, changed); !errors.Is(err, ErrTicketSetConflict) {
			t.Fatalf("changed payload err = %v, want ErrTicketSetConflict", err)
		}
		kids, err := s.ListTaskChildren(ctx, got.ParentID)
		if err != nil || len(kids) != 2 {
			t.Fatalf("children of the parent = %+v err=%v", kids, err)
		}
	})

	registerTaskAreaCase("intake/triage-list", func(t *testing.T, s *Store) {
		ctx := t.Context()
		if _, err := s.ListTriage(ctx, 0); err == nil {
			t.Fatal("ListTriage(0) accepted")
		}
		if _, err := s.ListTriage(ctx, 1001); err == nil {
			t.Fatal("ListTriage(1001) accepted")
		}
		x, created, err := s.IntakeTask(ctx, IntakeInput{Source: "intake-test", SourceRef: "triage/1", Title: "Triage list entry", CreatedBy: "tester"})
		if err != nil || !created {
			t.Fatalf("intake created=%v err=%v", created, err)
		}
		rows, err := s.ListTriage(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		var found *TriageRow
		for i := range rows {
			if rows[i].Task.ID == x.ID {
				found = &rows[i]
			}
		}
		if found == nil {
			t.Fatalf("triage queue lacks task %d: %+v", x.ID, rows)
		}
		if found.TicketSet || found.ChildCount != 0 || found.ChildrenInTriage != 0 || found.AgeSeconds < 0 || found.Similar == nil || len(found.Similar) != 0 {
			t.Fatalf("triage row = %+v", *found)
		}
		one, err := s.ListTriage(ctx, 1)
		if err != nil || len(one) != 1 {
			t.Fatalf("ListTriage(1) = %d rows err=%v", len(one), err)
		}
	})

	registerTaskAreaCase("intake/triage-similar", func(t *testing.T, s *Store) {
		ctx := t.Context()
		project := "intake-similar"
		_, err := s.AddTaskProject(ctx, project, "tester")
		if err != nil {
			t.Fatalf("AddTaskProject: %v", err)
		}
		title := "Rewrite the intake duplicate detection for similar titles"
		open, err := s.CreateTask(ctx, Task{Lane: "intake-similar", Title: title, Kind: "implement", CreatedBy: "tester", Project: &project})
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		near, created, err := s.IntakeTask(ctx, IntakeInput{Source: "intake-test", SourceRef: "similar/1", Title: "Rewrite the intake duplicate detection for similar title", CreatedBy: "tester"})
		if err != nil || !created {
			t.Fatalf("intake created=%v err=%v", created, err)
		}
		far, created, err := s.IntakeTask(ctx, IntakeInput{Source: "intake-test", SourceRef: "similar/2", Title: "Zebra crossing paint schedule", CreatedBy: "tester"})
		if err != nil || !created {
			t.Fatalf("intake created=%v err=%v", created, err)
		}
		rows, err := s.ListTriage(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, r := range rows {
			switch r.Task.ID {
			case near.ID:
				seen++
				if len(r.Similar) != 1 || r.Similar[0].ID != open.ID || r.Similar[0].Title != title || r.Similar[0].State != StateBacklog || r.Similar[0].Similarity < SimilarTaskThreshold || r.Similar[0].Similarity > 1 {
					t.Fatalf("near-duplicate similar = %+v, want the open task %d", r.Similar, open.ID)
				}
			case far.ID:
				seen++
				if len(r.Similar) != 0 {
					t.Fatalf("unrelated title similar = %+v", r.Similar)
				}
			}
		}
		if seen != 2 {
			t.Fatalf("triage rows seen = %d, want both intake tasks", seen)
		}
	})
}

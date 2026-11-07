package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// projectOwnerStore is a throwaway schema: these tests count event rows and
// seed fixed names, so they never share a schema with another test.
func projectOwnerStore(t *testing.T) (*Store, string, context.Context) {
	t.Helper()
	_, url := testdb.Schema(t, "project_owner")
	s, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, url, context.Background()
}

// TestProjectDesignLaneMigrationAppliesOnce proves the registered migration
// runs on a fresh schema, records its own ID once, and that a second Open
// against the same database runs nothing.
func TestProjectDesignLaneMigrationAppliesOnce(t *testing.T) {
	s, url, ctx := projectOwnerStore(t)
	var applied int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE id=$1`, projectDesignLaneMigrationID).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("migration row=%d err=%v", applied, err)
	}
	var defaultLane string
	if err := s.pool.QueryRow(ctx, `SELECT design_lane FROM task_projects WHERE name='handoffkeep'`).Scan(&defaultLane); err != nil {
		t.Fatalf("design_lane column missing: %v", err)
	}
	if defaultLane != "" {
		t.Fatalf("fresh design_lane=%q, want empty", defaultLane)
	}
	if current, err := migrateCurrent(ctx, s.pool, migrationRegistry); err != nil || !current {
		t.Fatalf("schema not current after migrate: current=%v err=%v", current, err)
	}
	// A second Open on the migrated schema must not re-run the migration.
	second, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(second.Close)
	if err = second.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE id=$1`, projectDesignLaneMigrationID).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("second Open recorded the ID %d times err=%v", applied, err)
	}
}

// TestSetProjectDesignLaneEvents covers the closed contract: a set, a change
// and a clear each write exactly one event carrying previous and new lane; an
// unchanged set writes none; an unknown project is ErrProjectNotFound and a
// malformed lane is refused before any write.
func TestSetProjectDesignLaneEvents(t *testing.T) {
	s, _, ctx := projectOwnerStore(t)
	// An unchanged set on a project nobody has claimed (both empty) is a no-op.
	if owner, err := s.SetProjectDesignLane(ctx, "handoffkeep", "", "tester", "still unowned"); err != nil || owner.DesignLane != "" || owner.EffectiveOwner != OperatorDeskLane {
		t.Fatalf("no-op set owner=%+v err=%v", owner, err)
	}
	if events := ownerEventCount(t, s, ctx, "handoffkeep"); events != 0 {
		t.Fatalf("no-op set wrote %d events", events)
	}

	owner, err := s.SetProjectDesignLane(ctx, "handoffkeep", "handoffkeep-design", "tester", "decision/2026-11-07/design-session-desk-authority")
	if err != nil {
		t.Fatal(err)
	}
	if owner.Name != "handoffkeep" || owner.DesignLane != "handoffkeep-design" || owner.EffectiveOwner != "handoffkeep-design" || owner.UpdatedAt == nil {
		t.Fatalf("set owner=%+v", owner)
	}
	assertOwnerEvent(t, s, ctx, "handoffkeep", 1, "", "handoffkeep-design", "tester")

	// Same value again: no event, and the response still names the owner.
	if owner, err = s.SetProjectDesignLane(ctx, "handoffkeep", "handoffkeep-design", "tester", "repeat"); err != nil || owner.DesignLane != "handoffkeep-design" {
		t.Fatalf("repeat set owner=%+v err=%v", owner, err)
	}
	if events := ownerEventCount(t, s, ctx, "handoffkeep"); events != 1 {
		t.Fatalf("unchanged set wrote an event: %d", events)
	}

	// Change.
	if owner, err = s.SetProjectDesignLane(ctx, "handoffkeep", "panewire-design", "operator", "handover"); err != nil || owner.DesignLane != "panewire-design" {
		t.Fatalf("change owner=%+v err=%v", owner, err)
	}
	assertOwnerEvent(t, s, ctx, "handoffkeep", 2, "handoffkeep-design", "panewire-design", "operator")

	// Clear: empty lane, fallback effective owner, one more event.
	if owner, err = s.SetProjectDesignLane(ctx, "handoffkeep", "", "operator", "session closed"); err != nil || owner.DesignLane != "" || owner.EffectiveOwner != OperatorDeskLane {
		t.Fatalf("clear owner=%+v err=%v", owner, err)
	}
	assertOwnerEvent(t, s, ctx, "handoffkeep", 3, "panewire-design", "", "operator")

	// Clearing an already-empty project is the same no-op.
	if _, err = s.SetProjectDesignLane(ctx, "handoffkeep", "", "operator", "already cleared"); err != nil {
		t.Fatalf("re-clear err=%v", err)
	}
	if events := ownerEventCount(t, s, ctx, "handoffkeep"); events != 3 {
		t.Fatalf("re-clear wrote an event: %d", events)
	}

	// Unknown project and malformed lane.
	if _, err = s.SetProjectDesignLane(ctx, "not-a-project", "x-design", "tester", ""); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("unknown project err=%v, want ErrProjectNotFound", err)
	}
	if _, err = s.SetProjectDesignLane(ctx, "handoffkeep", "bad lane!", "tester", ""); err == nil || errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("malformed lane err=%v, want a validation error", err)
	}
	if _, err = s.SetProjectDesignLane(ctx, "handoffkeep", "handoffkeep-design", "", ""); err == nil {
		t.Fatal("empty actor accepted")
	}
	if events := ownerEventCount(t, s, ctx, "handoffkeep"); events != 3 {
		t.Fatalf("refused writes left %d events", events)
	}
}

func ownerEventCount(t *testing.T, s *Store, ctx context.Context, project string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM project_owner_events WHERE project=$1`, project).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertOwnerEvent checks the want-th event of a project in write order (1 is
// the oldest). Reading ascending keeps the assertion independent of how many
// events follow it.
func assertOwnerEvent(t *testing.T, s *Store, ctx context.Context, project string, want int, previous, lane, actor string) {
	t.Helper()
	var prev, design, by string
	err := s.pool.QueryRow(ctx, `SELECT previous_lane, design_lane, actor FROM project_owner_events WHERE project=$1 ORDER BY id ASC OFFSET $2 LIMIT 1`, project, want-1).Scan(&prev, &design, &by)
	if err != nil {
		t.Fatalf("event %d missing: %v", want, err)
	}
	if prev != previous || design != lane || by != actor {
		t.Fatalf("event %d=(previous=%q lane=%q actor=%q), want (%q,%q,%q)", want, prev, design, by, previous, lane, actor)
	}
	if n := ownerEventCount(t, s, ctx, project); n != want {
		t.Fatalf("events=%d, want %d", n, want)
	}
}

// TestListProjectOwnersFallsBackToOperatorDesk proves every configured project
// is listed in name order and an unclaimed one resolves to operator-desk while
// a claimed one names its lane.
func TestListProjectOwnersFallsBackToOperatorDesk(t *testing.T) {
	s, _, ctx := projectOwnerStore(t)
	owners, err := s.ListProjectOwners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 18 {
		t.Fatalf("owners=%d, want the 18 seeded projects", len(owners))
	}
	for i := 1; i < len(owners); i++ {
		if owners[i-1].Name >= owners[i].Name {
			t.Fatalf("owners not in name order: %q then %q", owners[i-1].Name, owners[i].Name)
		}
	}
	for _, owner := range owners {
		if owner.DesignLane != "" || owner.EffectiveOwner != OperatorDeskLane || owner.UpdatedAt != nil {
			t.Fatalf("unclaimed project %q owner=%+v", owner.Name, owner)
		}
	}
	if _, err = s.SetProjectDesignLane(ctx, "wrk", "wrk-design", "tester", ""); err != nil {
		t.Fatal(err)
	}
	owners, err = s.ListProjectOwners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range owners {
		switch owner.Name {
		case "wrk":
			if owner.DesignLane != "wrk-design" || owner.EffectiveOwner != "wrk-design" || owner.UpdatedAt == nil {
				t.Fatalf("wrk owner=%+v", owner)
			}
		default:
			if owner.DesignLane != "" || owner.EffectiveOwner != OperatorDeskLane {
				t.Fatalf("unchanged project %q owner=%+v", owner.Name, owner)
			}
		}
	}
	// One project read matches the list row.
	one, err := s.GetProjectOwner(ctx, "wrk")
	if err != nil || one.DesignLane != "wrk-design" || one.EffectiveOwner != "wrk-design" {
		t.Fatalf("GetProjectOwner=%+v err=%v", one, err)
	}
	if _, err = s.GetProjectOwner(ctx, "not-a-project"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("unknown project err=%v, want ErrProjectNotFound", err)
	}
}

// TestProjectOwnerHistoryNewestFirst covers the listing cap, ordering and the
// unknown-project refusal.
func TestProjectOwnerHistoryNewestFirst(t *testing.T) {
	s, _, ctx := projectOwnerStore(t)
	for _, lane := range []string{"a-design", "b-design", "", "c-design"} {
		if _, err := s.SetProjectDesignLane(ctx, "herdr", lane, "tester", ""); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.ProjectOwnerHistory(ctx, "herdr", 20)
	if err != nil || len(events) != 4 {
		t.Fatalf("history events=%d err=%v", len(events), err)
	}
	if events[0].DesignLane != "c-design" || events[3].PreviousLane != "" || events[3].DesignLane != "a-design" {
		t.Fatalf("history=%+v", events)
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].ID <= events[i].ID {
			t.Fatalf("history not newest-first: %v", events)
		}
	}
	limited, err := s.ProjectOwnerHistory(ctx, "herdr", 2)
	if err != nil || len(limited) != 2 || limited[0].DesignLane != "c-design" {
		t.Fatalf("limited history=%+v err=%v", limited, err)
	}
	if _, err = s.ProjectOwnerHistory(ctx, "not-a-project", 2); err != nil {
		t.Fatalf("history of an unknown project is empty, not an error: %v", err)
	}
}

// TestProjectMetadataLeavesDesignLane pins step 3: the existing metadata
// writers stay unchanged and must not clear or read design_lane.
func TestProjectMetadataLeavesDesignLane(t *testing.T) {
	s, _, ctx := projectOwnerStore(t)
	if _, err := s.SetProjectDesignLane(ctx, "auto_trader", "auto_trader-design", "operator", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProjectMetadata(ctx, ProjectMetadata{Name: "auto_trader", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour)
	if _, err := s.PatchProjectMetadata(ctx, "auto_trader", ProjectMetadataPatch{SetStart: true, StartAt: &start}); err != nil {
		t.Fatal(err)
	}
	meta, err := s.GetProjectMetadata(ctx, "auto_trader")
	if err != nil || meta.Status != "active" {
		t.Fatalf("metadata=%+v err=%v", meta, err)
	}
	owner, err := s.GetProjectOwner(ctx, "auto_trader")
	if err != nil || owner.DesignLane != "auto_trader-design" || owner.EffectiveOwner != "auto_trader-design" {
		t.Fatalf("metadata write cleared the owner: %+v err=%v", owner, err)
	}
}

// TestProjectOwnerNoteRejectsSecret pins that the note goes through the same
// secret filter as other free text.
func TestProjectOwnerNoteRejectsSecret(t *testing.T) {
	s, _, ctx := projectOwnerStore(t)
	_, err := s.SetProjectDesignLane(ctx, "handoffkeep", "x-design", "tester", "token AKIA"+"ABCDEFGHIJKLMNOP")
	if err == nil || !strings.Contains(err.Error(), "secret_like_content") {
		t.Fatalf("secret note err=%v, want a secret_like_content refusal", err)
	}
	if events := ownerEventCount(t, s, ctx, "handoffkeep"); events != 0 {
		t.Fatalf("refused note wrote %d events", events)
	}
}

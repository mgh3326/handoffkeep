//go:build mysql

package store

import (
	"context"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestMySQLLaneCheckRunsOnMySQL runs the wake check's dialect-sensitive paths on MySQL:
// the findings queries, the notice insert under the MySQL transaction lock,
// the look insert with its duplicate, and the skip. PostgreSQL covers the same
// behaviour in lane_check_test.go.
func TestMySQLLaneCheckRunsOnMySQL(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AddTaskProject(ctx, "mylaneproj", "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProjectDesignLane(ctx, "mylaneproj", "mylane-design", "tester", "mysql lane check"); err != nil {
		t.Fatal(err)
	}
	project := "mylaneproj"
	kept, err := s.CreateTask(ctx, Task{Lane: "mylane-design", Title: "mysql kept work", Kind: "ops", Project: &project, Priority: 1, CreatedBy: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db().Exec(ctx, `UPDATE tasks SET created_at=? WHERE id=?`, time.Now().UTC().Add(-48*time.Hour), kept.ID); err != nil {
		t.Fatal(err)
	}
	ordered, err := s.CreateTask(ctx, Task{Lane: "mylane-design", Title: "mysql ordered work", Kind: "ops", Project: &project, Priority: 3, CreatedBy: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskOrder(ctx, CreateTaskOrderInput{TaskID: ordered.ID, Words: "go", Actor: "operator", Channel: TaskOrderChannelCLI, Verified: true, Key: "mysql-lane-key"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	checks, err := s.LaneChecks(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var got LaneCheck
	for _, c := range checks {
		if c.Lane == "mylane-design" {
			got = c
		}
	}
	if got.FindingsHash == "" {
		t.Fatalf("no check for the lane: %+v", checks)
	}
	if ids := laneCheckFinding(got, LaneFindingKeptBacklog).TaskIDs; len(ids) != 1 || ids[0] != kept.ID {
		t.Fatalf("kept_backlog = %v, want [%d]", ids, kept.ID)
	}
	if ids := laneCheckFinding(got, LaneFindingOrderUnacked).TaskIDs; len(ids) != 1 || ids[0] != ordered.ID {
		t.Fatalf("order_unacked = %v, want [%d]", ids, ordered.ID)
	}
	if ids := laneCheckFinding(got, LaneFindingRoomToRun).TaskIDs; len(ids) == 0 {
		t.Fatal("room_to_run missing")
	}
	wrote, err := s.RunLaneCheck(ctx, now)
	if err != nil || wrote != 1 {
		t.Fatalf("RunLaneCheck = %d, %v; want 1", wrote, err)
	}
	if wrote, err = s.RunLaneCheck(ctx, now); err != nil || wrote != 0 {
		t.Fatalf("repeat RunLaneCheck = %d, %v; want 0", wrote, err)
	}
	look, changed, err := s.RecordLaneLook(ctx, "mylane-design", got.NoticeEventID, "looked", "mylane-design", now)
	if err != nil || !changed || look.FindingsHash != got.FindingsHash {
		t.Fatalf("look = %+v changed=%t err=%v", look, changed, err)
	}
	if _, changed, err = s.RecordLaneLook(ctx, "mylane-design", got.NoticeEventID, "again", "mylane-design", now); err != nil || changed {
		t.Fatalf("repeat look changed=%t err=%v", changed, err)
	}
	// The look stops the next bucket's notice.
	if wrote, err = s.RunLaneCheck(ctx, now.Add(6*time.Hour)); err != nil || wrote != 0 {
		t.Fatalf("after look RunLaneCheck = %d, %v; want 0", wrote, err)
	}
}

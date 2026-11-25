package store

import (
	"fmt"
	"strings"
	"testing"
)

// dotsNoteMySQLStore, set by dots_note_mysql_test.go under -tags=mysql, opens
// a MySQL store for the MySQL leg of each dots-note test. Without the tag the
// leg does not exist, so CI's PostgreSQL shards never need a MySQL URL.
var dotsNoteMySQLStore func(t *testing.T) *Store

// forEachDotsNoteDialect runs one dots-note scenario on PostgreSQL and, when
// the build provides it, on MySQL. Each leg gets its own fresh store.
func forEachDotsNoteDialect(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	t.Run("postgres", func(t *testing.T) { fn(t, schemaStore(t, "dotsnote")) })
	if dotsNoteMySQLStore != nil {
		t.Run("mysql", func(t *testing.T) { fn(t, dotsNoteMySQLStore(t)) })
	}
}

type dotsNoteRow struct {
	Kind, Lane, EventID, Text string
}

// dotsNoteRows reads every dots_note outbox row, in insertion order.
func dotsNoteRows(t *testing.T, s *Store) []dotsNoteRow {
	t.Helper()
	rows, err := s.db().Query(t.Context(), `SELECT kind,target_lane,event_id,text FROM notification_outbox WHERE kind='dots_note' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []dotsNoteRow
	for rows.Next() {
		var r dotsNoteRow
		if err := rows.Scan(&r.Kind, &r.Lane, &r.EventID, &r.Text); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// dotsNoteProject seeds a project; a non-empty designLane is its design lane,
// and an empty one leaves the project with no design lane.
func dotsNoteProject(t *testing.T, s *Store, project, designLane string) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.AddTaskProject(ctx, project, "tester"); err != nil {
		t.Fatal(err)
	}
	if designLane != "" {
		if _, err := s.SetProjectDesignLane(ctx, project, designLane, "tester", "dots note test"); err != nil {
			t.Fatal(err)
		}
	}
}

// dotsNoteTask creates one backlog task in lane for project.
func dotsNoteTask(t *testing.T, s *Store, lane, project, title string) Task {
	t.Helper()
	p := project
	x, err := s.CreateTask(t.Context(), Task{Lane: lane, Title: title, Kind: "ops", Project: &p, Priority: 3, CreatedBy: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// dotsNoteMCPComment writes one comment through the MCP path as author.
func dotsNoteMCPComment(t *testing.T, s *Store, taskID int64, author, key, body string) (TaskComment, bool) {
	t.Helper()
	out, replayed, err := s.CreateMCPTaskCommentProjected(t.Context(), taskID, body, MCPWrite{
		Tool: "task_comment", Actor: "chatgpt:tester", Channel: "chatgpt", Author: author, Key: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.TaskComment, replayed
}

// dotsNoteIntake files one intake through the dots path.
func dotsNoteIntake(t *testing.T, s *Store, project, sourceRef, title string) (Task, bool) {
	t.Helper()
	p := project
	x, created, err := s.IntakeDotsTask(t.Context(), IntakeInput{Source: "chatgpt", SourceRef: sourceRef, Title: title, Project: &p, CreatedBy: DotsLabel})
	if err != nil {
		t.Fatal(err)
	}
	return x, created
}

// TestDotsNoteOnDesignLaneTask: a dots comment on a task in the design lane
// queues one dots_note to that lane, with the exact text.
func TestDotsNoteOnDesignLaneTask(t *testing.T) {
	forEachDotsNoteDialect(t, func(t *testing.T, s *Store) {
		dotsNoteProject(t, s, "dn-proj", "dn-design")
		task := dotsNoteTask(t, s, "dn-design", "dn-proj", "design lane task")
		c, replayed := dotsNoteMCPComment(t, s, task.ID, DotsLabel, "dn-design-1", "first line of the note\nsecond line")
		if replayed {
			t.Fatal("first comment reported as replayed")
		}
		rows := dotsNoteRows(t, s)
		if len(rows) != 1 {
			t.Fatalf("dots_note rows = %d, want 1: %+v", len(rows), rows)
		}
		got := rows[0]
		if got.Kind != "dots_note" || got.Lane != "dn-design" || got.EventID != fmt.Sprintf("dots-comment-%d", c.ID) {
			t.Fatalf("row = %+v", got)
		}
		want := fmt.Sprintf(`[hk] dots note on hk %d "design lane task" (%s) comment #%d: first line of the note | a dots opinion for the design session to sort, not an order | read: handoffkeep tasks comments %d --after-id %d`,
			task.ID, task.State, c.ID, task.ID, c.ID-1)
		if got.Text != want {
			t.Fatalf("text =\n%q\nwant\n%q", got.Text, want)
		}
	})
}

// TestDotsNoteOnWorkerClaimedTask: a task a worker in another lane has
// claimed still notes the project's design lane, and the text names the claim.
func TestDotsNoteOnWorkerClaimedTask(t *testing.T) {
	forEachDotsNoteDialect(t, func(t *testing.T, s *Store) {
		dotsNoteProject(t, s, "dn-proj", "dn-design")
		task := dotsNoteTask(t, s, "dn-worker-lane", "dn-proj", "claimed task")
		claimed, err := s.ClaimTask(t.Context(), task.ID, "dn-worker", "job-dn-claim", "")
		if err != nil {
			t.Fatal(err)
		}
		c, _ := dotsNoteMCPComment(t, s, task.ID, DotsLabel, "dn-claimed-1", "a note on claimed work")
		rows := dotsNoteRows(t, s)
		if len(rows) != 1 || rows[0].Lane != "dn-design" {
			t.Fatalf("dots_note rows = %+v, want one to dn-design", rows)
		}
		want := fmt.Sprintf(`[hk] dots note on hk %d "claimed task" (%s, claimed by dn-worker) comment #%d: a note on claimed work | a dots opinion for the design session to sort, not an order | read: handoffkeep tasks comments %d --after-id %d`,
			task.ID, claimed.State, c.ID, task.ID, c.ID-1)
		if rows[0].Text != want {
			t.Fatalf("text =\n%q\nwant\n%q", rows[0].Text, want)
		}
	})
}

// TestDotsNoteOnIntakeInTriage: a dots intake lands in triage, and its note
// goes to the project's design lane, not to triage.
func TestDotsNoteOnIntakeInTriage(t *testing.T) {
	forEachDotsNoteDialect(t, func(t *testing.T, s *Store) {
		dotsNoteProject(t, s, "dn-proj", "dn-design")
		task, created := dotsNoteIntake(t, s, "dn-proj", "dn-intake-1", "dots intake title")
		if !created || task.Lane != IntakeLane {
			t.Fatalf("intake created=%v lane=%q, want created in %q", created, task.Lane, IntakeLane)
		}
		rows := dotsNoteRows(t, s)
		if len(rows) != 1 {
			t.Fatalf("dots_note rows = %d, want 1: %+v", len(rows), rows)
		}
		if rows[0].Lane != "dn-design" || rows[0].Lane == IntakeLane {
			t.Fatalf("note lane = %q, want dn-design", rows[0].Lane)
		}
		if rows[0].EventID != fmt.Sprintf("dots-intake-%d", task.ID) {
			t.Fatalf("event id = %q", rows[0].EventID)
		}
		want := fmt.Sprintf(`[hk] dots filed intake hk %d "dots intake title" (%s) | a dots proposal for the design session to sort, not an order | read: handoffkeep tasks show %d`,
			task.ID, task.State, task.ID)
		if rows[0].Text != want {
			t.Fatalf("text =\n%q\nwant\n%q", rows[0].Text, want)
		}
	})
}

// TestDotsNoteNoneForDotsLaneTask: a dots comment on a task in lane dots
// queues nothing, because dots_reply already queues its own notices there.
func TestDotsNoteNoneForDotsLaneTask(t *testing.T) {
	forEachDotsNoteDialect(t, func(t *testing.T, s *Store) {
		dotsNoteProject(t, s, "dn-proj", "dn-design")
		x, created, err := s.IntakeTask(t.Context(), IntakeInput{Source: "dots", SourceRef: "dn-dots-lane", Title: "dots lane request", Lane: "dots", Project: strPtr("dn-proj"), CreatedBy: "caller"})
		if err != nil || !created {
			t.Fatalf("dots lane task created=%v err=%v", created, err)
		}
		dotsNoteMCPComment(t, s, x.ID, DotsLabel, "dn-dots-lane-1", "a comment in the dots lane")
		if rows := dotsNoteRows(t, s); len(rows) != 0 {
			t.Fatalf("dots_note rows = %+v, want none", rows)
		}
	})
}

// TestDotsNoteNoneForSessionComment: a comment by anyone but dots queues no
// note, on the MCP path or through CreateTaskComment.
func TestDotsNoteNoneForSessionComment(t *testing.T) {
	forEachDotsNoteDialect(t, func(t *testing.T, s *Store) {
		dotsNoteProject(t, s, "dn-proj", "dn-design")
		task := dotsNoteTask(t, s, "dn-design", "dn-proj", "session comment task")
		dotsNoteMCPComment(t, s, task.ID, "dn-session", "dn-session-1", "a session comment on MCP")
		if _, err := s.CreateTaskComment(t.Context(), task.ID, "dn-session", "a session comment"); err != nil {
			t.Fatal(err)
		}
		if rows := dotsNoteRows(t, s); len(rows) != 0 {
			t.Fatalf("dots_note rows = %+v, want none", rows)
		}
	})
}

// TestDotsNoteReplayQueuesOnce: replaying the same MCP comment and retrying
// the same intake each leave one note.
func TestDotsNoteReplayQueuesOnce(t *testing.T) {
	forEachDotsNoteDialect(t, func(t *testing.T, s *Store) {
		dotsNoteProject(t, s, "dn-proj", "dn-design")
		task := dotsNoteTask(t, s, "dn-design", "dn-proj", "replay task")
		first, replayed := dotsNoteMCPComment(t, s, task.ID, DotsLabel, "dn-replay-1", "a comment to replay")
		again, replayed2 := dotsNoteMCPComment(t, s, task.ID, DotsLabel, "dn-replay-1", "a comment to replay")
		if replayed || !replayed2 || again.ID != first.ID {
			t.Fatalf("replay: first replayed=%v, again replayed=%v id %d vs %d", replayed, replayed2, first.ID, again.ID)
		}
		intake, created := dotsNoteIntake(t, s, "dn-proj", "dn-replay-intake", "replayed intake")
		intakeAgain, createdAgain := dotsNoteIntake(t, s, "dn-proj", "dn-replay-intake", "replayed intake")
		if !created || createdAgain || intakeAgain.ID != intake.ID {
			t.Fatalf("intake retry: created=%v again=%v id %d vs %d", created, createdAgain, intake.ID, intakeAgain.ID)
		}
		rows := dotsNoteRows(t, s)
		if len(rows) != 2 {
			t.Fatalf("dots_note rows = %d, want 2 (one comment, one intake): %+v", len(rows), rows)
		}
	})
}

// TestDotsNoteNoDesignLaneGoesToOwnerRule: a project with no design lane
// sends its note to operator-desk, the owner rule's fallback.
func TestDotsNoteNoDesignLaneGoesToOwnerRule(t *testing.T) {
	forEachDotsNoteDialect(t, func(t *testing.T, s *Store) {
		dotsNoteProject(t, s, "dn-nolane", "")
		task := dotsNoteTask(t, s, "dn-worker-lane", "dn-nolane", "no design lane task")
		dotsNoteMCPComment(t, s, task.ID, DotsLabel, "dn-nolane-1", "a note with no design lane")
		rows := dotsNoteRows(t, s)
		if len(rows) != 1 || rows[0].Lane != OperatorDeskLane {
			t.Fatalf("dots_note rows = %+v, want one to %s", rows, OperatorDeskLane)
		}
	})
}

// TestDotsNoteTextSamples pins the two note texts without a database: the
// title and the comment's first line are cut by runes, and the note stays
// under the forwarder's 2,048-byte cap.
func TestDotsNoteTextSamples(t *testing.T) {
	long := strings.Repeat("가", 120) // 120 runes, 360 bytes
	comment := dotsNoteCommentText(7, long, "backlog", "lane-x", 42, "\n\n  first line  \nsecond")
	wantComment := fmt.Sprintf(`[hk] dots note on hk 7 "%s" (backlog, claimed by lane-x) comment #42: first line | a dots opinion for the design session to sort, not an order | read: handoffkeep tasks comments 7 --after-id 41`, strings.Repeat("가", 80))
	if comment != wantComment {
		t.Fatalf("comment text =\n%q\nwant\n%q", comment, wantComment)
	}
	intake := dotsNoteIntakeText(9, long, "triage")
	wantIntake := fmt.Sprintf(`[hk] dots filed intake hk 9 "%s" (triage) | a dots proposal for the design session to sort, not an order | read: handoffkeep tasks show 9`, strings.Repeat("가", 80))
	if intake != wantIntake {
		t.Fatalf("intake text =\n%q\nwant\n%q", intake, wantIntake)
	}
	huge := strings.Repeat("나", 5000)
	if n := len(dotsNoteCommentText(1, huge, "backlog", "", 1, huge)); n >= 2048 {
		t.Fatalf("comment note is %d bytes, want under 2048", n)
	}
	t.Logf("sample comment note:\n%s", dotsNoteCommentText(7, "Example task", "developing", "worker-1", 42, "Please check the lock order here."))
	t.Logf("sample intake note:\n%s", dotsNoteIntakeText(9, "Example intake", "triage"))
}

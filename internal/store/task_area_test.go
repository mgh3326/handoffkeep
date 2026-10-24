package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func areaPtr(v string) *string { return &v }

func taskAreaLane(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("area-%d", time.Now().UnixNano())
}

// The vocabulary is a code constant: exactly these 13 values in this order.
// Changing the list is a code change, so this test is the pin.
func TestTaskAreaVocabularyIsPinned(t *testing.T) {
	want := []string{"blender", "unity", "web-qa", "ui-design", "image", "web-frontend", "go-backend", "python", "trading", "shell-wrk", "docs", "research", "ops"}
	got := TaskAreas()
	if len(got) != len(want) {
		t.Fatalf("TaskAreas() has %d values, want %d: %v", len(got), len(want), got)
	}
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("TaskAreas()[%d]=%q, want %q", i, got[i], v)
		}
	}
}

// The vocabulary cannot be changed at runtime: the accessor returns a copy,
// so writing to it neither changes what is accepted nor what the next caller
// sees.
func TestTaskAreasReturnsACopy(t *testing.T) {
	first := TaskAreas()
	first[0] = "anything"
	first[len(first)-1] = "blender"
	if ValidTaskArea("anything") {
		t.Fatal("writing to the returned slice changed the accepted vocabulary")
	}
	if again := TaskAreas(); again[0] != "blender" || again[len(again)-1] != "ops" {
		t.Fatalf("writing to the returned slice changed the vocabulary: %v", again)
	}
}

// Every vocabulary value is accepted and nothing near it is: validation is
// an exact match, with no case folding, trimming or normalisation.
func TestValidTaskAreaIsExactMatch(t *testing.T) {
	for _, v := range TaskAreas() {
		if !ValidTaskArea(v) {
			t.Errorf("ValidTaskArea(%q)=false for a vocabulary value", v)
		}
	}
	for _, bad := range []string{
		"", " ", "Blender", "BLENDER", "go", "golang", "web", "web_qa", "webqa", "shell", "wrk",
		" blender", "blender ", "blender\n", "blender\x00", "blen der",
		"blender,unity", "blender/unity", "docs.", "ops\t",
		"ｂlender",  // fullwidth b
		"blеnder",  // cyrillic е
		"bl​ender", // zero-width space
		"ops ",     // no-break space
		"ｏｐｓ",      // fullwidth ops
		"unitý",   // combining accent
		strings.Repeat("a", 4096),
	} {
		if ValidTaskArea(bad) {
			t.Errorf("ValidTaskArea(%q)=true, want a refusal", bad)
		}
	}
}

// The unknown-area error names the whole allowed set, in order, so a caller
// can fix the value without a second lookup.
func TestTaskAreaErrorNamesAllowedSet(t *testing.T) {
	err := ValidateTaskArea(areaPtr("Blender"))
	if !errors.Is(err, ErrTaskAreaUnknown) {
		t.Fatalf("ValidateTaskArea(Blender)=%v, want ErrTaskAreaUnknown", err)
	}
	if !strings.Contains(err.Error(), strings.Join(TaskAreas(), ", ")) {
		t.Fatalf("error %q does not list the allowed set in order", err)
	}
	if err := ValidateTaskArea(nil); err != nil {
		t.Fatalf("ValidateTaskArea(nil)=%v, want nil (area is optional)", err)
	}
	if err := ValidateTaskArea(areaPtr("docs")); err != nil {
		t.Fatalf("ValidateTaskArea(docs)=%v", err)
	}
}

// Task JSON always carries the key: null for an unset area, never omitted.
func TestTaskJSONAlwaysCarriesArea(t *testing.T) {
	raw, err := json.Marshal(Task{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if v, ok := out["area"]; !ok || string(v) != "null" {
		t.Fatalf("unset area encoded as %q (present=%t), want null", v, ok)
	}
	raw, _ = json.Marshal(Task{ID: 1, Area: areaPtr("docs")})
	if !strings.Contains(string(raw), `"area":"docs"`) {
		t.Fatalf("set area not encoded: %s", raw)
	}
}

// A create with an unknown area is refused before a row exists; a known one
// round-trips, and an absent one stays null.
func TestCreateTaskAreaValidation(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	lane := taskAreaLane(t)
	for _, bad := range []string{"", "Blender", "go", " docs"} {
		_, err := s.CreateTask(ctx, Task{Lane: lane, Title: "gated", Kind: "implement", CreatedBy: "area-test", Project: projectPtr(testProjectName), Area: areaPtr(bad)})
		if !errors.Is(err, ErrTaskAreaUnknown) {
			t.Fatalf("CreateTask(area=%q) err=%v, want ErrTaskAreaUnknown", bad, err)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE lane=$1`, lane).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused creates left %d rows err=%v", rows, err)
	}
	x, err := s.CreateTask(ctx, Task{Lane: lane, Title: "tagged", Kind: "implement", CreatedBy: "area-test", Project: projectPtr(testProjectName), Area: areaPtr("go-backend")})
	if err != nil || x.Area == nil || *x.Area != "go-backend" {
		t.Fatalf("CreateTask=%+v err=%v", x, err)
	}
	y, err := s.CreateTask(ctx, Task{Lane: lane, Title: "untagged", Kind: "implement", CreatedBy: "area-test", Project: projectPtr(testProjectName)})
	if err != nil || y.Area != nil {
		t.Fatalf("untagged CreateTask=%+v err=%v", y, err)
	}
	// Creating with an area writes no area event: the event records changes
	// to an existing task, and the create row itself is the history.
	var events int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND kind='area'`, x.ID).Scan(&events); err != nil || events != 0 {
		t.Fatalf("create wrote %d area events err=%v", events, err)
	}
}

// SetTaskArea writes one kind='area' event per change carrying old→new, and
// nothing when the value is unchanged. Unset renders as the empty string in
// from/to, the same convention as project.
func TestSetTaskAreaEvent(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	lane := taskAreaLane(t)
	x, err := s.CreateTask(ctx, Task{Lane: lane, Title: "tag me", Kind: "implement", CreatedBy: "area-test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.SetTaskArea(ctx, x.ID, areaPtr("Blender"), "area-test", "nope"); !errors.Is(err, ErrTaskAreaUnknown) {
		t.Fatalf("SetTaskArea(unknown) err=%v", err)
	}
	events := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND kind='area'`, x.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if events() != 0 {
		t.Fatal("a refused change wrote an event")
	}
	x, changed, err := s.SetTaskArea(ctx, x.ID, areaPtr("blender"), "area-test", "modelling work")
	if err != nil || !changed || x.Area == nil || *x.Area != "blender" {
		t.Fatalf("SetTaskArea=%+v changed=%t err=%v", x, changed, err)
	}
	var kind, from, to, by, note string
	if err = pool.QueryRow(ctx, `SELECT kind,"from","to","by",note FROM task_events WHERE task_id=$1 AND kind='area'`, x.ID).Scan(&kind, &from, &to, &by, &note); err != nil {
		t.Fatalf("area event missing: %v", err)
	}
	if kind != "area" || from != "" || to != "blender" || by != "area-test" || note != "modelling work" {
		t.Fatalf("event=(%s,%q,%q,%s,%s)", kind, from, to, by, note)
	}
	// Unchanged: changed=false and no event.
	x, changed, err = s.SetTaskArea(ctx, x.ID, areaPtr("blender"), "area-test", "same again")
	if err != nil || changed || events() != 1 {
		t.Fatalf("unchanged set changed=%t events=%d err=%v", changed, events(), err)
	}
	// Change: old and new both recorded.
	x, changed, err = s.SetTaskArea(ctx, x.ID, areaPtr("unity"), "area-test", "actually unity")
	if err != nil || !changed || *x.Area != "unity" || events() != 2 {
		t.Fatalf("change changed=%t events=%d err=%v", changed, events(), err)
	}
	if err = pool.QueryRow(ctx, `SELECT "from","to" FROM task_events WHERE task_id=$1 AND kind='area' ORDER BY id DESC LIMIT 1`, x.ID).Scan(&from, &to); err != nil || from != "blender" || to != "unity" {
		t.Fatalf("change event from=%q to=%q err=%v", from, to, err)
	}
	// Clear, then clear again (no-op), then re-set.
	x, changed, err = s.SetTaskArea(ctx, x.ID, nil, "area-test", "cleared")
	if err != nil || !changed || x.Area != nil || events() != 3 {
		t.Fatalf("clear=%+v changed=%t events=%d err=%v", x, changed, events(), err)
	}
	if err = pool.QueryRow(ctx, `SELECT "from","to" FROM task_events WHERE task_id=$1 AND kind='area' ORDER BY id DESC LIMIT 1`, x.ID).Scan(&from, &to); err != nil || from != "unity" || to != "" {
		t.Fatalf("clear event from=%q to=%q err=%v", from, to, err)
	}
	if _, changed, err = s.SetTaskArea(ctx, x.ID, nil, "area-test", "clear again"); err != nil || changed || events() != 3 {
		t.Fatalf("second clear changed=%t events=%d err=%v", changed, events(), err)
	}
	x, changed, err = s.SetTaskArea(ctx, x.ID, areaPtr("unity"), "area-test", "back")
	if err != nil || !changed || *x.Area != "unity" || events() != 4 {
		t.Fatalf("re-set changed=%t events=%d err=%v", changed, events(), err)
	}
	// The event is visible in GetTask history.
	got, ok, err := s.GetTask(ctx, x.ID)
	if err != nil || !ok {
		t.Fatalf("GetTask ok=%t err=%v", ok, err)
	}
	seen := 0
	for _, e := range got.Events {
		if e.Kind == TaskEventArea {
			seen++
		}
	}
	if seen != 4 || got.Area == nil || *got.Area != "unity" {
		t.Fatalf("GetTask area events=%d area=%v", seen, got.Area)
	}
	if _, _, err = s.SetTaskArea(ctx, 999999, areaPtr("docs"), "area-test", "ghost"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("SetTaskArea(missing) err=%v", err)
	}
	// Area is metadata, not lifecycle: terminal rows stay taggable and the
	// area event does not disturb the transition history.
	if _, err = s.TransitionTask(ctx, x.ID, "dropped", "area-test", "", nil, ""); err != nil {
		t.Fatalf("drop: %v", err)
	}
	x, changed, err = s.SetTaskArea(ctx, x.ID, areaPtr("docs"), "area-test", "post-close")
	if err != nil || !changed || x.State != "dropped" || *x.Area != "docs" {
		t.Fatalf("terminal SetTaskArea=%+v changed=%t err=%v", x, changed, err)
	}
}

// The set-area validation is store-side too, so a caller that bypasses the
// API cannot write a value outside the vocabulary.
func TestSetTaskAreaRejectsBadActor(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	x, err := s.CreateTask(ctx, Task{Lane: taskAreaLane(t), Title: "actor", Kind: "implement", CreatedBy: "area-test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.SetTaskArea(ctx, x.ID, areaPtr("docs"), "", "no actor"); err == nil {
		t.Fatal("empty actor accepted")
	}
}

// Concurrent set-area calls on one task serialise on the row lock: each call
// that changed the value wrote exactly one event, no event was lost, and the
// final column equals the last event's "to".
func TestSetTaskAreaConcurrent(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	x, err := s.CreateTask(ctx, Task{Lane: taskAreaLane(t), Title: "race", Kind: "implement", CreatedBy: "area-test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	// Eight callers all set the same value: exactly one changes it.
	var wg sync.WaitGroup
	var mu sync.Mutex
	changedCount := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, changed, err := s.SetTaskArea(ctx, x.ID, areaPtr("python"), "area-test", "same")
			if err != nil {
				t.Errorf("concurrent SetTaskArea: %v", err)
				return
			}
			if changed {
				mu.Lock()
				changedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var events int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND kind='area'`, x.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if changedCount != 1 || events != 1 {
		t.Fatalf("same-value race: changed=%d events=%d, want 1 and 1", changedCount, events)
	}
	// Different values race: every winning change is one event, and the
	// event chain is unbroken (each from equals the previous to).
	values := []string{"docs", "ops", "trading", "image", "research", "unity"}
	changedCount = 0
	for _, v := range values {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, changed, err := s.SetTaskArea(ctx, x.ID, areaPtr(v), "area-test", "race")
			if err != nil {
				t.Errorf("concurrent SetTaskArea(%s): %v", v, err)
				return
			}
			if changed {
				mu.Lock()
				changedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	rows, err := pool.Query(ctx, `SELECT "from","to" FROM task_events WHERE task_id=$1 AND kind='area' ORDER BY id`, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	prev, n := "", 0
	for rows.Next() {
		var from, to string
		if err = rows.Scan(&from, &to); err != nil {
			t.Fatal(err)
		}
		if from != prev {
			t.Fatalf("event %d from=%q, previous to=%q: chain broken", n, from, prev)
		}
		prev = to
		n++
	}
	if n != 1+changedCount {
		t.Fatalf("events=%d, want 1+%d changes", n, changedCount)
	}
	got, _, err := s.GetTask(ctx, x.ID)
	if err != nil || got.Area == nil || *got.Area != prev {
		t.Fatalf("final area=%v, last event to=%q err=%v", got.Area, prev, err)
	}
}

// ListTasks and ListTasksPage filter by area; the empty string selects rows
// with no area; an unknown value is refused rather than matching nothing.
func TestListTasksAreaFilter(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	lane := taskAreaLane(t)
	a, err := s.CreateTask(ctx, Task{Lane: lane, Title: "alpha", Kind: "implement", CreatedBy: "area-test", Project: projectPtr(testProjectName), Area: areaPtr("blender")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTask(ctx, Task{Lane: lane, Title: "beta", Kind: "implement", CreatedBy: "area-test", Project: projectPtr(testProjectName), Area: areaPtr("docs")})
	if err != nil {
		t.Fatal(err)
	}
	var legacy int64
	if err = pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at) VALUES($1,'legacy','implement','backlog','area-test',now(),now()) RETURNING id`, lane).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListTasks(ctx, lane, "", "", nil, nil, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("unfiltered=%d err=%v", len(all), err)
	}
	only, err := s.ListTasks(ctx, lane, "", "", nil, areaPtr("docs"), 10)
	if err != nil || len(only) != 1 || only[0].ID != b.ID || only[0].Area == nil || *only[0].Area != "docs" {
		t.Fatalf("area=docs filter=%+v err=%v", only, err)
	}
	none, err := s.ListTasks(ctx, lane, "", "", nil, areaPtr(""), 10)
	if err != nil || len(none) != 1 || none[0].ID != legacy || none[0].Area != nil {
		t.Fatalf("area='' filter=%+v err=%v", none, err)
	}
	page, err := s.ListTasksPage(ctx, lane, "", "", nil, areaPtr("blender"), 0, 10)
	if err != nil || len(page) != 1 || page[0].ID != a.ID {
		t.Fatalf("paged area=blender=%+v err=%v", page, err)
	}
	if _, err = s.ListTasks(ctx, lane, "", "", nil, areaPtr("Blender"), 10); !errors.Is(err, ErrTaskAreaUnknown) {
		t.Fatalf("unknown area filter err=%v, want ErrTaskAreaUnknown", err)
	}
	if _, err = s.ListTasksPage(ctx, lane, "", "", nil, areaPtr("go"), 0, 10); !errors.Is(err, ErrTaskAreaUnknown) {
		t.Fatalf("unknown paged area filter err=%v, want ErrTaskAreaUnknown", err)
	}
	// A valid but unused area is a legitimate filter returning zero rows.
	empty, err := s.ListTasks(ctx, lane, "", "", nil, areaPtr("trading"), 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("unused area filter=%+v err=%v", empty, err)
	}
	// Project and area compose.
	both, err := s.ListTasks(ctx, lane, "", "", projectPtr(testProjectName), areaPtr("blender"), 10)
	if err != nil || len(both) != 1 || both[0].ID != a.ID {
		t.Fatalf("project+area filter=%+v err=%v", both, err)
	}
}

// v18 against a schema holding rows created before the column: the column
// arrives nullable, the legacy row keeps NULL, the event CHECK admits 'area',
// and a second migrate is a no-op.
func TestTaskAreaMigrationIsAdditive(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	lane := taskAreaLane(t)
	for _, q := range []string{
		`DELETE FROM schema_version WHERE version=18`,
		`ALTER TABLE tasks DROP COLUMN area`,
		`ALTER TABLE task_events DROP CONSTRAINT task_events_kind_check`,
		`ALTER TABLE task_events ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane','decision','project'))`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var legacy int64
	if err := pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at) VALUES($1,'pre-v18','implement','backlog','area-test',now(),now()) RETURNING id`, lane).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("v18 migrate: %v", err)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT area IS NULL FROM tasks WHERE id=$1`, legacy).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("legacy row area NULL=%t err=%v", isNull, err)
	}
	var version int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=18`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version 18 rows=%d err=%v", version, err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=18`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version 18 rows after restart=%d err=%v", version, err)
	}
	x, changed, err := s.SetTaskArea(ctx, legacy, areaPtr("docs"), "area-test", "check widened")
	if err != nil || !changed || x.Area == nil || *x.Area != "docs" {
		t.Fatalf("SetTaskArea on v18 schema: %+v changed=%t err=%v", x, changed, err)
	}
}

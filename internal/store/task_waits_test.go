package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Pure checks of the waits store (hk 5128): input rules and the section
// builder. They need no database; the behaviour on both dialects is in
// task_waits_area_cases_test.go.

func TestCheckTaskWaitInputRules(t *testing.T) {
	cases := []struct {
		name string
		self int64
		in   TaskWaitInput
		bad  bool
	}{
		{"operator with no ref", 5, TaskWaitInput{OnKind: WaitOnOperator, Reason: "set the device key"}, false},
		{"operator refuses a ref", 5, TaskWaitInput{OnKind: WaitOnOperator, OnRef: "x", Reason: "r"}, true},
		{"lane needs a ref", 5, TaskWaitInput{OnKind: WaitOnLane, Reason: "r"}, true},
		{"lane with a ref", 5, TaskWaitInput{OnKind: WaitOnLane, OnRef: "handoffkeep-design", Reason: "r"}, false},
		{"unknown kind", 5, TaskWaitInput{OnKind: "robot", OnRef: "x", Reason: "r"}, true},
		{"task wait to another task", 5, TaskWaitInput{OnKind: WaitOnTask, OnRef: "9", Reason: "r"}, false},
		{"task wait to itself", 5, TaskWaitInput{OnKind: WaitOnTask, OnRef: "5", Reason: "r"}, true},
		{"task wait with a non-number", 5, TaskWaitInput{OnKind: WaitOnTask, OnRef: "9x", Reason: "r"}, true},
		{"task wait with an empty reason", 5, TaskWaitInput{OnKind: WaitOnTask, OnRef: "9", Reason: ""}, true},
		{"task wait with a leading zero", 5, TaskWaitInput{OnKind: WaitOnTask, OnRef: "09", Reason: "r"}, true},
		{"external spawn failure", 5, TaskWaitInput{OnKind: WaitOnExternal, OnRef: WaitRefSpawn, Reason: "502 at hub-accept"}, false},
		{"empty reason", 5, TaskWaitInput{OnKind: WaitOnOperator, Reason: ""}, true},
		{"reason too long", 5, TaskWaitInput{OnKind: WaitOnOperator, Reason: strings.Repeat("가", maxWaitReasonBytes)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkTaskWaitInput(c.self, c.in)
			if c.bad {
				if !errors.Is(err, ErrTaskWaitInvalid) {
					t.Fatalf("checkTaskWaitInput(%+v)=%v, want ErrTaskWaitInvalid", c.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkTaskWaitInput(%+v)=%v, want nil", c.in, err)
			}
		})
	}
}

func TestOtherTaskIDRejectsNonCanonicalIDs(t *testing.T) {
	if id, ok := otherTaskID("42", 7); !ok || id != 42 {
		t.Fatalf("otherTaskID(42,7)=%d,%t", id, ok)
	}
	for _, ref := range []string{"", "0", "-3", "+4", "04", "7", "4 2", "9999999999999999999999"} {
		if _, ok := otherTaskID(ref, 7); ok {
			t.Fatalf("otherTaskID(%q,7) accepted", ref)
		}
	}
}

func TestValidWaitSectionKeys(t *testing.T) {
	for _, k := range []string{"", WaitSectionNeedsOperator, WaitSectionFailedStart, WaitSectionWaitingOn, WaitSectionIdleStarted} {
		if !validWaitSection(k) {
			t.Fatalf("validWaitSection(%q)=false", k)
		}
	}
	for _, k := range []string{"all", "operator", "waiting-on", "NEEDS_OPERATOR"} {
		if validWaitSection(k) {
			t.Fatalf("validWaitSection(%q)=true", k)
		}
	}
}

func TestWaitAgeNeverNegative(t *testing.T) {
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	if got := waitAgeSeconds(now, now.Add(time.Hour)); got != 0 {
		t.Fatalf("future since: age=%d, want 0", got)
	}
	if got := waitAgeSeconds(now, now.Add(-90*time.Second)); got != 90 {
		t.Fatalf("90s old: age=%d", got)
	}
}

func TestBuildWaitsViewSectionsAndFilter(t *testing.T) {
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	older := now.Add(-2 * time.Hour)
	rows := []WaitRow{
		{Key: "wait:2", Section: WaitSectionWaitingOn, TaskID: 2, Since: now.Add(-time.Minute)},
		{Key: "decision:dr-1-1", Section: WaitSectionNeedsOperator, TaskID: 1, Since: older},
		{Key: "wait:3", Section: WaitSectionNeedsOperator, TaskID: 3, Since: now.Add(-time.Hour)},
	}
	idle := TaskDriftClass{Truncated: true}
	backlog := WaitBacklog{Count: 1, Rows: []WaitBacklogRow{{TaskID: 9, Title: "plain"}}}

	all := buildWaitsView(now, "", rows, idle, backlog)
	if len(all.Sections) != 4 || all.Sections[0].Key != WaitSectionNeedsOperator || all.Sections[3].Key != WaitSectionIdleStarted {
		t.Fatalf("sections=%+v", all.Sections)
	}
	op := all.Sections[0]
	if op.Count != 2 || len(op.Rows) != 2 || op.Rows[0].Key != "decision:dr-1-1" {
		t.Fatalf("needs_operator oldest first: %+v", op)
	}
	if op.Rows[0].AgeSeconds != int64(2*time.Hour/time.Second) {
		t.Fatalf("age of the oldest row=%d", op.Rows[0].AgeSeconds)
	}
	if !all.Sections[3].Truncated {
		t.Fatal("idle_started truncation not carried")
	}
	if all.Backlog.Count != 1 || len(all.Backlog.Rows) != 1 {
		t.Fatalf("backlog=%+v", all.Backlog)
	}

	one := buildWaitsView(now, WaitSectionWaitingOn, rows, idle, backlog)
	if one.Sections[0].Count != 2 || len(one.Sections[0].Rows) != 0 {
		t.Fatalf("filtered view keeps other sections' counts but not rows: %+v", one.Sections[0])
	}
	if len(one.Sections[2].Rows) != 1 || one.Sections[2].Rows[0].Key != "wait:2" {
		t.Fatalf("filtered waiting_on rows=%+v", one.Sections[2].Rows)
	}
	if len(one.Backlog.Rows) != 0 || one.Backlog.Count != 1 {
		t.Fatalf("a section filter hides backlog rows and keeps the count: %+v", one.Backlog)
	}
}

func TestInPlaceholdersPerDialect(t *testing.T) {
	pg := &pgTransaction{}
	if got := inPlaceholders(pg, 3); got != "$1,$2,$3" {
		t.Fatalf("postgres placeholders=%q", got)
	}
	my := &sqlTransaction{}
	if got := inPlaceholders(my, 2); got != "?,?" {
		t.Fatalf("mysql placeholders=%q", got)
	}
}

// An empty section and an empty backlog encode their rows as arrays, not null:
// the console parser rejects rows: null, so one empty section would hide the page.
func TestBuildWaitsViewEmptyRowsEncodeAsArrays(t *testing.T) {
	view := buildWaitsView(time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC), "", nil, TaskDriftClass{}, WaitBacklog{})
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sections []struct {
			Rows json.RawMessage `json:"rows"`
		} `json:"sections"`
		Backlog struct {
			Rows json.RawMessage `json:"rows"`
		} `json:"backlog"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Sections) != 4 {
		t.Fatalf("sections=%d", len(doc.Sections))
	}
	for i, sec := range doc.Sections {
		if string(sec.Rows) != "[]" {
			t.Fatalf("section %d rows=%s, want []", i, sec.Rows)
		}
	}
	if string(doc.Backlog.Rows) != "[]" {
		t.Fatalf("backlog rows=%s, want []", doc.Backlog.Rows)
	}
}

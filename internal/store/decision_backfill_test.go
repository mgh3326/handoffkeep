package store

import (
	"fmt"
	"testing"
	"time"
)

// One item per class per source, shaped the way ListOpenDecisionSources
// reports each source: task sources name their task, relay sources name a job
// (and the job's task), chat names neither. A class a source cannot produce
// from live data (a chat question naming a task) is still classified the same
// way, since the dry run must handle whatever production holds.
func TestClassifyOpenDecisionEveryClassEverySource(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	young, old := now.Add(-time.Hour), now.Add(-15*24*time.Hour)
	task := func(id int64) *int64 { return &id }
	items := []OpenDecisionItem{}
	want := map[string]string{}
	for _, source := range DecisionSources() {
		cases := map[string]OpenDecisionItem{
			DecisionClassLive:                {TaskID: task(1), TaskState: StateDeveloping, CreatedAt: old},
			DecisionClassSupersededCandidate: {TaskID: task(2), TaskState: StateMerged, CreatedAt: young},
			DecisionClassStaleCandidate:      {TaskID: task(3), TaskState: StateBacklog, CreatedAt: old},
			DecisionClassOrphan:              {CreatedAt: old},
		}
		if source == DecisionSourceJobEscalate || source == DecisionSourceLaneEvent {
			cases[DecisionClassLive] = OpenDecisionItem{JobID: "j-live", JobState: "spawned", CreatedAt: old}
			cases[DecisionClassSupersededCandidate] = OpenDecisionItem{JobID: "j-done", JobState: "completed", TaskID: task(4), TaskState: StateDeveloping, CreatedAt: young}
			cases[DecisionClassStaleCandidate] = OpenDecisionItem{JobID: "j-esc", JobState: "escalated", CreatedAt: old}
			cases[DecisionClassOrphan] = OpenDecisionItem{JobID: "j-unknown", CreatedAt: young}
		}
		for class, x := range cases {
			x.Source, x.SourceRef = source, fmt.Sprintf("%s-%s", source, class)
			items = append(items, x)
			want[x.SourceRef] = class
		}
	}
	report := ClassifyOpenDecisions(OpenDecisionSources{ServerTime: now, Items: items})
	if report.Total != len(items) || len(report.Rows) != len(items) || !report.DryRun {
		t.Fatalf("total=%d rows=%d dry_run=%v", report.Total, len(report.Rows), report.DryRun)
	}
	for _, row := range report.Rows {
		if row.Class != want[row.SourceRef] {
			t.Errorf("%s: class %s, want %s", row.SourceRef, row.Class, want[row.SourceRef])
		}
	}
	for _, c := range DecisionClasses() {
		if report.ByClass[c] != len(DecisionSources()) {
			t.Errorf("by_class[%s]=%d", c, report.ByClass[c])
		}
	}
	for _, s := range DecisionSources() {
		if report.BySource[s] != len(DecisionClasses()) {
			t.Errorf("by_source[%s]=%d", s, report.BySource[s])
		}
	}
	// Rows come in source order.
	for i := 1; i < len(report.Rows); i++ {
		a, b := report.Rows[i-1], report.Rows[i]
		if a.Source != b.Source && sourceIndex(a.Source) > sourceIndex(b.Source) {
			t.Fatalf("rows out of source order at %d: %s then %s", i, a.Source, b.Source)
		}
	}
}

func sourceIndex(source string) int {
	for i, s := range DecisionSources() {
		if s == source {
			return i
		}
	}
	return -1
}

func TestClassifyOpenDecisionRules(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	id := int64(9)
	for _, tc := range []struct {
		name string
		x    OpenDecisionItem
		want string
	}{
		{"task named but unknown", OpenDecisionItem{TaskID: &id, CreatedAt: now}, DecisionClassOrphan},
		{"icebox task", OpenDecisionItem{TaskID: &id, TaskState: StateIcebox, CreatedAt: now}, DecisionClassSupersededCandidate},
		{"lost job beside a live task", OpenDecisionItem{TaskID: &id, TaskState: StateReviewing, JobID: "j", JobState: "lost", CreatedAt: now}, DecisionClassSupersededCandidate},
		{"started task, old", OpenDecisionItem{TaskID: &id, TaskState: StateResearching, CreatedAt: now.Add(-60 * 24 * time.Hour)}, DecisionClassLive},
		{"claimed job, old", OpenDecisionItem{JobID: "j", JobState: "claimed", CreatedAt: now.Add(-60 * 24 * time.Hour)}, DecisionClassLive},
		{"backlog task, exactly 14 days", OpenDecisionItem{TaskID: &id, TaskState: StateBacklog, CreatedAt: now.Add(-DecisionStaleAge)}, DecisionClassLive},
		{"backlog task, past 14 days", OpenDecisionItem{TaskID: &id, TaskState: StateBacklog, CreatedAt: now.Add(-DecisionStaleAge - time.Second)}, DecisionClassStaleCandidate},
		{"escalated job, young", OpenDecisionItem{JobID: "j", JobState: "escalated", CreatedAt: now.Add(-time.Hour)}, DecisionClassLive},
	} {
		if got := ClassifyOpenDecision(tc.x, now); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	empty := ClassifyOpenDecisions(OpenDecisionSources{ServerTime: now})
	if empty.Total != 0 || len(empty.ByClass) != len(DecisionClasses()) || len(empty.BySource) != len(DecisionSources()) || empty.Rows == nil || empty.Truncated == nil {
		t.Fatalf("empty report=%+v", empty)
	}
}

// The parse matches ListOpenLaneDecisions' LIKE '[decision-answered] #<id>:%'
// with the id as Postgres prints it.
func TestLaneAnsweredID(t *testing.T) {
	for text, want := range map[string]int64{
		"[decision-answered] #12: yes":  12,
		"[decision-answered] #12:":      12,
		"[decision-answered] #12 yes":   0,
		"[decision-answered] #012: yes": 0,
		"[decision-answered] #+12: yes": 0,
		"[decision-answered] #: yes":    0,
		"[decision-answered] 12: yes":   0,
		" [decision-answered] #12: yes": 0,
		"[decision-needed] #12: yes":    0,
	} {
		got, ok := laneAnsweredID(text)
		if (want == 0) == ok || got != want {
			t.Errorf("%q: %d %v, want %d", text, got, ok, want)
		}
	}
}

func TestRelayTouchesDecisions(t *testing.T) {
	for _, tc := range []struct {
		x    RelayEvent
		want bool
	}{
		{RelayEvent{Kind: "job.completed"}, true},
		{RelayEvent{Kind: "job.lost"}, true},
		{RelayEvent{Kind: "job.joined"}, true},
		{RelayEvent{Kind: "job.revoked"}, true},
		{RelayEvent{Kind: "job.escalate"}, false},
		{RelayEvent{Kind: "lane.event", Text: "[decision-needed] q"}, false},
		{RelayEvent{Kind: "lane.event", Text: "[decision-answered] #3: a"}, true},
		{RelayEvent{Kind: "lane.event", EventID: "cli-decision-escalation-3-x", Text: "[decision] #3: a"}, true},
		{RelayEvent{Kind: "lane.event", EventID: "x", Text: "status"}, false},
	} {
		if got := relayTouchesDecisions(tc.x); got != tc.want {
			t.Errorf("%+v: %v, want %v", tc.x, got, tc.want)
		}
	}
}

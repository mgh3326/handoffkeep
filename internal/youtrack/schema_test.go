package youtrack

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// seedSchemaTenant builds the tenant hk 3836 starts from: HANDOFFKEE is an
// import project with partial bundles, MERGEWAKE is complete but unmapped, and
// factortrail has no YouTrack project at all.
func seedSchemaTenant(t *testing.T) (*fakeTenant, *fakeSchemaStore, *SchemaRunner) {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	f := newFakeTenant(t)
	f.seedBuiltinLinks()
	schema := DefaultSchema()
	st := &fakeSchemaStore{
		projects:   []string{"factortrail", "handoffkeep", "mergewake"},
		lanes:      []string{"lane-a", "lane-b"},
		milestones: map[string][]string{"handoffkeep": {"M1", "M2"}},
		rows:       map[string]store.YouTrackProject{},
	}
	vocab := Vocabulary{Kinds: store.TaskKinds(), Lanes: st.lanes, Areas: store.TaskAreas(), Milestones: st.milestones}

	// The import's project: only the values rows used, no resolved flag set.
	hk := f.addProject("HANDOFFKEE", "handoffkeep")
	st.rows["handoffkeep"] = store.YouTrackProject{HKProject: "handoffkeep", ProjectID: hk.ID, ShortName: "HANDOFFKEE"}
	f.attach(hk, schema.StateBundle, KindState, []BundleValue{{Name: "대기"}, {Name: "개발 중"}, {Name: "검토 중"}, {Name: "머지됨"}, {Name: "완료됨"}})
	f.attach(hk, schema.KindField, KindEnum, []BundleValue{{Name: "implement"}, {Name: "fix"}})
	f.attach(hk, schema.GradeField, KindEnum, []BundleValue{{Name: "A"}, {Name: "B"}})
	f.attach(hk, schema.TierField, KindEnum, []BundleValue{{Name: "T1"}})
	f.attach(hk, schema.LaneField, KindEnum, []BundleValue{{Name: "lane-a"}})

	// A project that has every field the import made, complete, but no mapping row.
	mw := f.addProject("MERGEWAKE", "mergewake")
	for _, spec := range schema.Desired(vocab, "mergewake")[:5] {
		f.attach(mw, spec.Name, spec.Kind, spec.Values)
	}
	return f, st, &SchemaRunner{Client: f.client(t, Config{}), Store: st, Schema: schema}
}

func planLines(p Plan) []string {
	var out []string
	for _, c := range p.Changes {
		out = append(out, c.Line())
	}
	return out
}

func TestSchemaPlanListsExactlyWhatIsMissing(t *testing.T) {
	f, _, runner := seedSchemaTenant(t)
	plan, err := runner.Plan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		// projects
		"factortrail\tproject\tcreate\tFACTORTRAI",
		"factortrail\tmapping\tput\tFACTORTRAI",
		"mergewake\tmapping\tput\tMERGEWAKE",
		// field definitions the tenant lacks, once each
		"-\t마감\tcreate-field\tdate",
		"-\t영역\tcreate-field\tenum[1]",
		"-\t마일스톤\tcreate-field\tenum[1]",
		"-\t예상 소요\tcreate-field\tperiod",
		// attachments: the new project gets every field, an import project the four new ones
		"factortrail\thk 상태\tattach\tstate[1] with 12 values",
		"factortrail\t종류\tattach\tenum[1] with 5 values",
		"factortrail\t등급\tattach\tenum[1] with 6 values",
		"factortrail\t티어\tattach\tenum[1] with 4 values",
		"factortrail\t레인\tattach\tenum[1] with 2 values",
		"factortrail\t마감\tattach\tdate",
		"factortrail\t영역\tattach\tenum[1] with 13 values",
		"factortrail\t마일스톤\tattach\tenum[1] with 0 values",
		"factortrail\t예상 소요\tattach\tperiod",
		"handoffkeep\t마감\tattach\tdate",
		"handoffkeep\t영역\tattach\tenum[1] with 13 values",
		"handoffkeep\t마일스톤\tattach\tenum[1] with 2 values",
		"handoffkeep\t예상 소요\tattach\tperiod",
		"mergewake\t마감\tattach\tdate",
		"mergewake\t영역\tattach\tenum[1] with 13 values",
		"mergewake\t마일스톤\tattach\tenum[1] with 0 values",
		"mergewake\t예상 소요\tattach\tperiod",
		// missing bundle values, in the declaration's order
		"handoffkeep\thk 상태\tadd-value\t분류 대기 (resolved=false ordinal=0)",
		"handoffkeep\thk 상태\tadd-value\t착수 전 (resolved=false ordinal=2)",
		"handoffkeep\thk 상태\tadd-value\t조사 중 (resolved=false ordinal=3)",
		"handoffkeep\thk 상태\tadd-value\t배포됨 (resolved=true ordinal=7)",
		"handoffkeep\thk 상태\tadd-value\t종료됨 (resolved=true ordinal=9)",
		"handoffkeep\thk 상태\tadd-value\t중복 (resolved=true ordinal=10)",
		"handoffkeep\thk 상태\tadd-value\t보류(보관) (resolved=false ordinal=11)",
		"handoffkeep\t종류\tadd-value\tdecide",
		"handoffkeep\t종류\tadd-value\tops",
		"handoffkeep\t종류\tadd-value\tverify",
		"handoffkeep\t등급\tadd-value\tS+",
		"handoffkeep\t등급\tadd-value\tS",
		"handoffkeep\t등급\tadd-value\tA+",
		"handoffkeep\t등급\tadd-value\tC",
		"handoffkeep\t티어\tadd-value\tT0",
		"handoffkeep\t티어\tadd-value\tT2",
		"handoffkeep\t티어\tadd-value\tT3",
		"handoffkeep\t레인\tadd-value\tlane-b",
		// flags and ordinals of the values already there
		"handoffkeep\thk 상태\tset-ordinal\t대기=1",
		"handoffkeep\thk 상태\tset-ordinal\t개발 중=4",
		"handoffkeep\thk 상태\tset-ordinal\t검토 중=5",
		"handoffkeep\thk 상태\tset-resolved\t머지됨=true",
		"handoffkeep\thk 상태\tset-ordinal\t머지됨=6",
		"handoffkeep\thk 상태\tset-resolved\t완료됨=true",
		"handoffkeep\thk 상태\tset-ordinal\t완료됨=8",
	}
	if got := planLines(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("plan lines differ\n got: %q\nwant: %q", got, want)
	}
	if len(plan.Reports) != 0 {
		t.Fatalf("reports=%+v, the tenant has the built-in link types", plan.Reports)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("plan wrote to YouTrack: %v", w)
	}
}

func TestSchemaApplyMakesThePlanTrueAndNeverDeletes(t *testing.T) {
	f, st, runner := seedSchemaTenant(t)
	ctx := context.Background()
	before := map[string]string{} // element id -> name, for every value already there
	for _, b := range f.bundles {
		for _, v := range b.Values {
			before[v.ID] = v.Name
		}
	}
	plan, err := runner.Plan(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := runner.Apply(ctx, plan)
	if err != nil || applied != len(plan.Changes) {
		t.Fatalf("applied=%d of %d err=%v", applied, len(plan.Changes), err)
	}
	again, err := runner.Plan(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Changes) != 0 {
		t.Fatalf("second plan is not empty: %q", planLines(again))
	}
	n := len(f.writes())
	if applied, err := runner.Apply(ctx, again); err != nil || applied != 0 || len(f.writes()) != n {
		t.Fatalf("second apply: applied=%d err=%v wrote %d more", applied, err, len(f.writes())-n)
	}

	for id, name := range before {
		found := false
		for _, b := range f.bundles {
			for _, v := range b.Values {
				found = found || (v.ID == id && v.Name == name)
			}
		}
		if !found {
			t.Errorf("value %s %q was lost or renamed", id, name)
		}
	}
	// The state bundle: every hk state, its resolved flag, its ordinal.
	var state *fakeBundle
	for _, b := range f.bundles {
		if b.Name == "HANDOFFKEE hk 상태" {
			state = b
		}
	}
	for i, s := range store.TaskStates() {
		var el *fakeElement
		for _, v := range state.Values {
			if v.Name == StateLabel(s) {
				el = v
			}
		}
		if el == nil || el.Resolved != StateResolved(s) || el.Ordinal != i {
			t.Errorf("state %s: %+v, want resolved=%t ordinal=%d", s, el, StateResolved(s), i)
		}
	}
	// The new project: made with the API user as leader, mapped, fully fielded.
	var ft *fakeProject
	for _, p := range f.projects {
		if p.Name == "factortrail" {
			ft = p
		}
	}
	if ft == nil || ft.Short != "FACTORTRAI" || ft.Leader != f.me.ID || len(ft.Fields) != 9 {
		t.Fatalf("factortrail project=%+v", ft)
	}
	if row := st.rows["factortrail"]; row.ProjectID != ft.ID || row.ShortName != "FACTORTRAI" {
		t.Fatalf("factortrail mapping=%+v", row)
	}
	if row := st.rows["mergewake"]; row.ShortName != "MERGEWAKE" || row.ProjectID == "" {
		t.Fatalf("mergewake mapping=%+v", row)
	}
	// The HK bucket and DEC are never made or touched by a run.
	for _, p := range f.projects {
		if p.Short == "HK" || p.Short == "DEC" {
			t.Errorf("run touched %s", p.Short)
		}
	}
}

func TestSchemaApplyRunsInFixedOrderOneRequestAtATime(t *testing.T) {
	f, _, runner := seedSchemaTenant(t)
	ctx := context.Background()
	plan, err := runner.Plan(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if got := f.maxLive.Load(); got != 1 {
		t.Fatalf("%d requests were in flight at once, want 1", got)
	}
	rank := func(req string) int {
		switch {
		case req == "POST /api/admin/projects":
			return 1
		case req == "POST /api/admin/customFieldSettings/customFields":
			return 2
		case strings.HasSuffix(req, "/customFields") || strings.Contains(req, "/bundles/") && strings.Count(req, "/") == 5:
			return 3 // attach, and the bundle each attach creates
		case strings.HasSuffix(req, "/values"):
			return 4
		}
		return 5
	}
	last := 0
	for _, req := range f.writes() {
		r := rank(req)
		// Creating a bundle belongs to its attach; it may interleave with attaches only.
		if r < last {
			t.Fatalf("write %q (phase %d) after phase %d: %v", req, r, last, f.writes())
		}
		last = r
	}
}

func TestSchemaApplyStopsAtTheFirstError(t *testing.T) {
	cases := []struct {
		name    string
		failOn  func(method, path string) bool
		noAfter func(req string) bool
	}{
		{"attach", func(m, p string) bool {
			return m == http.MethodPost && strings.HasPrefix(p, "/api/admin/projects/") && strings.HasSuffix(p, "/customFields")
		}, func(req string) bool { return strings.HasSuffix(req, "/values") || strings.Contains(req, "/values/") }},
		{"value", func(m, p string) bool { return m == http.MethodPost && strings.HasSuffix(p, "/values") },
			func(req string) bool { return strings.Contains(req, "/values/") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, runner := seedSchemaTenant(t)
			ctx := context.Background()
			plan, err := runner.Plan(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			failed := false
			f.hook = func(method, path string, _ int) (int, map[string]string) {
				if !failed && tc.failOn(method, path) {
					failed = true
					return http.StatusBadRequest, nil
				}
				return 0, nil
			}
			applied, err := runner.Apply(ctx, plan)
			if err == nil || !failed {
				t.Fatalf("apply err=%v failed=%t, want the injected failure", err, failed)
			}
			if applied >= len(plan.Changes) {
				t.Fatalf("applied %d of %d despite the failure", applied, len(plan.Changes))
			}
			// The failing request is the last write: nothing later was attempted.
			writes := f.writes()
			if len(writes) == 0 {
				t.Fatal("no writes")
			}
			for _, req := range writes {
				if tc.noAfter(req) {
					t.Errorf("a later-phase write %q landed after the failure", req)
				}
			}
			after := f.requests[len(f.requests)-1]
			if !tc.failOn(strings.Fields(after)[0], strings.Fields(after)[1]) {
				t.Errorf("last request %q is not the failed one", after)
			}
		})
	}
}

func TestSchemaRateLimitHonoursRetryAfter(t *testing.T) {
	f, _, _ := seedSchemaTenant(t)
	var waits []time.Duration
	limited := false
	f.hook = func(method, path string, _ int) (int, map[string]string) {
		if !limited && method == http.MethodPost && path == "/api/admin/projects" {
			limited = true
			return http.StatusTooManyRequests, map[string]string{"Retry-After": "17"}
		}
		return 0, nil
	}
	client := f.client(t, Config{RetryAfterCap: time.Minute, Sleep: func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}})
	runner := &SchemaRunner{Client: client, Store: &fakeSchemaStore{projects: []string{"factortrail"}, rows: map[string]store.YouTrackProject{}}, Schema: DefaultSchema()}
	plan, err := runner.Plan(context.Background(), "factortrail")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(waits, []time.Duration{17 * time.Second}) {
		t.Fatalf("waits=%v, want one wait of the server's 17s", waits)
	}
}

func TestSchemaProjectFlagLimitsTheRun(t *testing.T) {
	_, _, runner := seedSchemaTenant(t)
	plan, err := runner.Plan(context.Background(), "handoffkeep")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range plan.Changes {
		if c.Project != "" && c.Project != "handoffkeep" {
			t.Fatalf("--project handoffkeep planned %q", c.Line())
		}
	}
	if _, err := runner.Plan(context.Background(), "nope"); err == nil {
		t.Fatal("an unknown --project is refused")
	}
}

func TestSchemaReportsMissingLinkTypesAndNeverCreatesThem(t *testing.T) {
	f, _, runner := seedSchemaTenant(t)
	f.links = []LinkType{{ID: "105-0", Name: "Relates"}, {ID: "105-1", Name: "Duplicate", Directed: false}}
	ctx := context.Background()
	plan, err := runner.Plan(ctx, "mergewake")
	if err != nil {
		t.Fatal(err)
	}
	want := []Report{
		{Object: "link-type", Status: "missing", Value: "Subtask"},
		{Object: "link-type", Status: "mismatch", Value: "Duplicate directed=false want true"},
	}
	if !reflect.DeepEqual(plan.Reports, want) {
		t.Fatalf("reports=%+v, want %+v", plan.Reports, want)
	}
	if _, err := runner.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for _, req := range f.writes() {
		if strings.Contains(req, "issueLinkTypes") {
			t.Fatalf("apply wrote a link type: %s", req)
		}
	}
	if !strings.Contains(plan.Text(), "report\tlink-type\tmissing\tSubtask\n") {
		t.Fatalf("text form lacks the report line:\n%s", plan.Text())
	}
}

func TestSchemaAdoptsAnExistingProjectWithoutCreatingOne(t *testing.T) {
	f, st, runner := seedSchemaTenant(t)
	full, err := runner.Plan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Apply(context.Background(), full); err != nil {
		t.Fatal(err)
	}
	delete(st.rows, "mergewake")
	st.puts = nil
	projects := len(f.projects)
	plan, err := runner.Plan(context.Background(), "mergewake")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := planLines(plan), []string{"mergewake\tmapping\tput\tMERGEWAKE"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("plan=%q want %q", got, want)
	}
	if _, err := runner.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(f.projects) != projects || len(st.puts) != 1 {
		t.Fatalf("projects=%d puts=%v", len(f.projects), st.puts)
	}
}

func TestShortNameFollowsTheImportRule(t *testing.T) {
	cases := []struct {
		name  string
		taken []string
		want  string
	}{
		{"handoffkeep", nil, "HANDOFFKEE"},
		{"factortrail", nil, "FACTORTRAI"},
		{"ParkingArcade", nil, "PARKINGARC"},
		{"fleet-ops", nil, "FLEETOPS"},
		{"1stproject", nil, "P1STPROJEC"},
		{"x", nil, "X0"},
		{"auto_trader-operator", []string{"AUTOTRADER"}, "AUTOTRADE2"},
		{"auto_trader-operator", []string{"AUTOTRADER", "AUTOTRADE2"}, "AUTOTRADE3"},
		{"dec", []string{"DEC"}, "DEC2"},
	}
	for _, tc := range cases {
		taken := map[string]bool{}
		for _, k := range tc.taken {
			taken[k] = true
		}
		got, err := ShortNameFor(tc.name, taken)
		if err != nil || got != tc.want {
			t.Errorf("ShortNameFor(%q, %v) = %q, %v; want %q", tc.name, tc.taken, got, err, tc.want)
		}
	}
	if _, err := ShortNameFor("---", nil); err == nil {
		t.Error("a name with no letters or digits has no key")
	}
}

func TestDesiredSchemaStateBundleFollowsHKOrderAndFlags(t *testing.T) {
	spec := DefaultSchema().Desired(Vocabulary{}, "x")[0]
	if spec.Name != "hk 상태" || spec.Kind != KindState || len(spec.Values) != 12 {
		t.Fatalf("state spec=%+v", spec)
	}
	var resolved []string
	for i, v := range spec.Values {
		if v.Ordinal != i {
			t.Errorf("%s ordinal=%d want %d", v.Name, v.Ordinal, i)
		}
		if v.Resolved {
			resolved = append(resolved, v.Name)
		}
	}
	sort.Strings(resolved)
	want := []string{"머지됨", "배포됨", "완료됨", "종료됨", "중복"}
	sort.Strings(want)
	if !reflect.DeepEqual(resolved, want) {
		t.Fatalf("resolved=%v want %v", resolved, want)
	}
}

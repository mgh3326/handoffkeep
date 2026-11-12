package youtrack

import (
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Schema is the field contract: hk field, YouTrack custom field name, and the
// Korean value the operator sees. The project is not here: each hk project
// maps to its own YouTrack project in youtrack_projects. The labels match the
// console's STATE_LABEL table (web/console/src/shared/states.ts), so one state
// has one name on both screens.
type Schema struct {
	StateField    string
	LaneField     string
	KindField     string
	GradeField    string
	TierField     string
	HKIDField     string
	PriorityField string
	PRField       string
	ProjectField  string
	StallField    string
	StateBundle   string
	// The fields hk 3836 adds: hk data that had no YouTrack field.
	DueField       string
	AreaField      string
	MilestoneField string
	ExpectedField  string
}

func DefaultSchema() Schema {
	return Schema{
		StateField:    ytwrite.FieldState,
		LaneField:     ytwrite.FieldLane,
		KindField:     ytwrite.FieldKind,
		GradeField:    ytwrite.FieldGrade,
		TierField:     ytwrite.FieldTier,
		HKIDField:     ytwrite.FieldHKID,
		PriorityField: ytwrite.FieldPriority,
		PRField:       ytwrite.FieldPR,
		ProjectField:  ytwrite.FieldProject,
		StallField:    ytwrite.FieldStalled,
		StateBundle:   "hk 상태",

		DueField:       ytwrite.FieldDue,
		AreaField:      ytwrite.FieldArea,
		MilestoneField: ytwrite.FieldMilestone,
		ExpectedField:  ytwrite.FieldExpected,
	}
}

// DecisionProject is the short name of the YouTrack project that holds one
// issue per hk decision request. Its fields are the 결정 fields below; the
// lane and hk ID fields are shared with the task project.
const DecisionProject = "DEC"

// Decision issue fields (the DEC project). The names are the ones the Python
// mirror created, so the existing issues keep their values.
const (
	DecStateField       = "결정 상태"
	DecDueField         = "결정 기한"
	DecIDField          = "결정 ID"
	DecRecommendedField = "권장"
)

// StallLabel is the value of the 멈춤 enum on a stalled task.
const StallLabel = ytwrite.StalledValue

// StateLabels is the one hk state → YouTrack value table. The keys are the
// twelve canonical store states.
// It is ytwrite's table, which the issue_fields kind writes with.
var StateLabels = ytwrite.TaskStateLabels

// resolvedStates mark the state bundle values that mean finished work.
var resolvedStates = map[string]bool{
	"merged": true, "deployed": true, "completed": true, "dropped": true, "duplicate": true,
}

// StateLabel returns the YouTrack value for a canonical hk state. An unknown
// state is returned unchanged so a caller can still write it (and a fake can
// show the defect) rather than silently writing an empty field.
func StateLabel(state string) string {
	if label, ok := StateLabels[state]; ok {
		return label
	}
	return state
}

// StateFromValue maps a YouTrack state value back to an hk state. It accepts
// the Korean label, the normalized label, and the English state name, so a
// bundle renamed by the operator keeps working.
func StateFromValue(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	for state, label := range StateLabels {
		if strings.EqualFold(value, label) || normalizeLabel(value) == normalizeLabel(label) || strings.EqualFold(value, state) {
			return state, true
		}
	}
	return "", false
}

func normalizeLabel(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	return strings.ReplaceAll(v, " ", "")
}

// StateResolved reports whether a state value means finished work, for the
// state bundle element's isResolved flag.
func StateResolved(state string) bool { return resolvedStates[state] }

// Grades and Tiers are the closed value sets of the 등급 and 티어 enums, the
// ones the "(grade G, T)" title marker carries.
var (
	Grades = store.TaskGrades()
	Tiers  = []string{"T0", "T1", "T2", "T3"}
)

// FieldKind is how YouTrack stores a field: it picks the global field type,
// the project-field $type and whether the field has a bundle.
type FieldKind int

const (
	KindState  FieldKind = iota // state[1], a StateBundle with isResolved
	KindEnum                    // enum[1], an EnumBundle
	KindDate                    // date, no bundle
	KindPeriod                  // period, no bundle
)

// FieldSpec is one field the schema wants on every hk project. Values is the
// desired bundle content in order; a project's bundle gets the ones it lacks
// and never loses one.
type FieldSpec struct {
	Name   string
	Kind   FieldKind
	Values []BundleValue
}

// BundleValue is one wanted bundle element. Resolved is meaningful for a state
// bundle only; Ordinal is the position a state value takes (enum values keep
// the order they are added in).
type BundleValue struct {
	Name     string
	Resolved bool
	Ordinal  int
}

// Vocabulary is the hk data the desired schema is built from. Lanes and
// milestones are per hk project; the rest is shared by every project.
type Vocabulary struct {
	Kinds      []string
	Lanes      map[string][]string
	Areas      []string
	Milestones map[string][]string
}

// Desired is the one declaration of the schema one hk project should have. Plan
// and apply both read it, so a field or value is named in this function only.
func (s Schema) Desired(v Vocabulary, hkProject string) []FieldSpec {
	states := make([]BundleValue, 0, len(StateLabels))
	for i, state := range store.TaskStates() {
		states = append(states, BundleValue{Name: StateLabel(state), Resolved: StateResolved(state), Ordinal: i})
	}
	return []FieldSpec{
		{Name: s.StateBundle, Kind: KindState, Values: states},
		{Name: s.KindField, Kind: KindEnum, Values: enumValues(v.Kinds)},
		{Name: s.GradeField, Kind: KindEnum, Values: enumValues(Grades)},
		{Name: s.TierField, Kind: KindEnum, Values: enumValues(Tiers)},
		{Name: s.LaneField, Kind: KindEnum, Values: enumValues(v.Lanes[hkProject])},
		{Name: s.DueField, Kind: KindDate},
		{Name: s.AreaField, Kind: KindEnum, Values: enumValues(v.Areas)},
		{Name: s.MilestoneField, Kind: KindEnum, Values: enumValues(v.Milestones[hkProject])},
		{Name: s.ExpectedField, Kind: KindPeriod},
	}
}

// enumValues keeps the first of any names that normalize alike: YouTrack treats
// them as one value.
func enumValues(names []string) []BundleValue {
	seen := map[string]bool{}
	out := make([]BundleValue, 0, len(names))
	for _, name := range names {
		key := normalizeLabel(name)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, BundleValue{Name: name})
	}
	return out
}

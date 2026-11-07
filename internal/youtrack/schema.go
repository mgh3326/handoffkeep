package youtrack

import "strings"

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
	StateBundle   string
}

func DefaultSchema() Schema {
	return Schema{
		StateField:    "hk 상태",
		LaneField:     "레인",
		KindField:     "종류",
		GradeField:    "등급",
		TierField:     "티어",
		HKIDField:     "hk ID",
		PriorityField: "hk 우선순위",
		PRField:       "PR",
		StateBundle:   "hk 상태",
	}
}

// StateLabels is the one hk state → YouTrack value table. The keys are the
// twelve canonical store states.
var StateLabels = map[string]string{
	"triage":      "분류 대기",
	"backlog":     "대기",
	"unstarted":   "착수 전",
	"researching": "조사 중",
	"developing":  "개발 중",
	"reviewing":   "검토 중",
	"merged":      "머지됨",
	"deployed":    "배포됨",
	"completed":   "완료됨",
	"dropped":     "종료됨",
	"duplicate":   "중복",
	"icebox":      "보류(보관)",
}

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

package store

import (
	"encoding/json"
	"strings"
)

// DecisionTemplate is a read-time presentation of a producer's request. It
// never changes the request or its audit history.
type DecisionTemplate struct {
	Question string
	Options  DecisionOptions
	Reason   string
}

type decisionTemplate func(DecisionRequest, *DecisionOptions) (DecisionTemplate, bool)

var decisionTemplates = []decisionTemplate{mergewakeDeployTemplate}

// RenderDecisionTemplate returns the first matching presentation.
func RenderDecisionTemplate(request DecisionRequest, options *DecisionOptions) (DecisionTemplate, bool) {
	for _, render := range decisionTemplates {
		if result, ok := render(request, options); ok {
			return result, true
		}
	}
	return DecisionTemplate{}, false
}

// DecisionTemplateText renders a template for text-only destinations.
func DecisionTemplateText(t DecisionTemplate) string {
	heading, raw, ok := strings.Cut(t.Question, " {")
	if !ok {
		return t.Question
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte("{"+raw), &fields) != nil {
		return t.Question
	}
	var lines []string
	lines = append(lines, heading)
	for _, key := range []string{"대상 호스트", "저장소", "릴리스", "이전 배포", "승인이 필요한 경로", "사유", "판단"} {
		value, found := fields[key]
		if !found {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			var values []string
			if json.Unmarshal(value, &values) != nil {
				continue
			}
			if len(values) == 0 {
				text = "없음"
			} else {
				text = strings.Join(values, ", ")
			}
		}
		lines = append(lines, key+": "+text)
	}
	for _, option := range t.Options.Options {
		label := option.Key + ": " + option.Label
		if option.Recommended {
			label += " (권고)"
		}
		lines = append(lines, label)
	}
	return strings.Join(lines, "\n")
}

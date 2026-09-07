package store

import (
	"reflect"
	"strings"
	"testing"
)

func TestDecisionOptionsRoundTrip(t *testing.T) {
	tests := []DecisionOptions{
		{Options: []DecisionOption{{Key: "A", Label: "one"}}, AllowFree: true},
		{Options: []DecisionOption{{Key: "A", Label: "A 안"}, {Key: "B", Label: "두 번째: 선택"}, {Key: "C", Label: "셋"}, {Key: "D", Label: "넷"}, {Key: "E", Label: "다섯"}, {Key: "F", Label: "여섯", Recommended: true}}, AllowFree: true},
		{Options: []DecisionOption{{Key: "A", Label: "노드 로컬 저장", Recommended: true}, {Key: "B", Label: "중앙 저장 선행"}}, AllowFree: false},
	}
	for _, want := range tests {
		t.Run(FormatDecisionOptions(want), func(t *testing.T) {
			body, got, ok := ParseDecisionOptions("질문 본문\n" + FormatDecisionOptions(want))
			if !ok || body != "질문 본문" || !reflect.DeepEqual(got, want) {
				t.Fatalf("body=%q options=%+v ok=%t want=%+v", body, got, ok, want)
			}
		})
	}
}

func TestDecisionOptionsRejectsPartialGrammar(t *testing.T) {
	long := strings.Repeat("x", 121)
	for name, text := range map[string]string{
		"seven":         "body\n[options] A|a;B|b;C|c;D|d;E|e;F|f;A|again",
		"duplicate":     "body\n[options] A|a;A|b",
		"lower":         "body\n[options] a|a",
		"long":          "body\n[options] A|" + long,
		"unknown rec":   "body\n[options] A|a;rec=Z",
		"two rec":       "body\n[options] A|a;rec=A;rec=A",
		"unknown token": "body\n[options] A|a;x=1",
		"not final":     "body\n[options] A|a\ncontinued",
	} {
		t.Run(name, func(t *testing.T) {
			body, options, ok := ParseDecisionOptions(text)
			if ok || body != text || !reflect.DeepEqual(options, DecisionOptions{}) {
				t.Fatalf("body=%q options=%+v ok=%t", body, options, ok)
			}
		})
	}
}

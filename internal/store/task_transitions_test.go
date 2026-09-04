package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestTaskTransitionDocumentation keeps the rendered state diagram equivalent
// to TaskTransitions, the sole source of allowed server transitions.
func TestTaskTransitionDocumentation(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	edges := map[string]map[string]bool{}
	re := regexp.MustCompile(`(?m)^\s*([a-z_]+) --> ([a-z_]+)(?::.*)?$`)
	for _, match := range re.FindAllStringSubmatch(string(b), -1) {
		from, to := match[1], match[2]
		if _, known := TaskTransitions[from]; known {
			if edges[from] == nil {
				edges[from] = map[string]bool{}
			}
			edges[from][to] = true
		}
	}
	if !strings.Contains(string(b), "stateDiagram-v2") {
		t.Fatal("README has no task state diagram")
	}
	for from, targets := range TaskTransitions {
		for to := range targets {
			if !edges[from][to] {
				t.Errorf("README missing transition %s -> %s", from, to)
			}
		}
		for to := range edges[from] {
			if !targets[to] {
				t.Errorf("README documents forbidden transition %s -> %s", from, to)
			}
		}
	}
}

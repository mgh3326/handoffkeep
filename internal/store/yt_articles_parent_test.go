package store

import "testing"

func TestYTArticleAmends(t *testing.T) {
	for _, c := range []struct {
		name, key, body, want string
	}{
		{"names_policy", "decision/2026-11-12/x", "# Title\n\namends: policy/index\n", "policy/index"},
		{"trims_spaces", "decision/x", "  amends:   policy/observability  \n", "policy/observability"},
		{"first_line_counts", "decision/x", "amends: task/9\namends: policy/index\n", ""},
		{"not_a_decision", "design/x", "amends: policy/index\n", ""},
		{"no_line", "decision/x", "amends the policy\n", ""},
	} {
		if got := ytArticleAmends(c.key, c.body); got != c.want {
			t.Errorf("%s: ytArticleAmends = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestYTArticleFamily(t *testing.T) {
	for _, c := range []struct {
		key, want string
	}{
		{"design/operator-surfaces", "design"},
		{"decision/2026-11-12/model-routing", "decision"},
		{"README", ""},
		{"my docs/x", ""},
	} {
		if got := ytArticleFamily(1, c.key); got != c.want {
			t.Errorf("ytArticleFamily(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}

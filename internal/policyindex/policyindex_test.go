package policyindex

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func list(t *testing.T, rows ...Row) string {
	t.Helper()
	b, err := json.Marshal(List{Version: 1, Intro: "intro\n\n", Rows: rows, Tail: "## Tail\n"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func row(key, parent string) Row {
	return Row{Key: key, Parent: parent, Kind: "tool", Title: key + " title", Home: "home-" + key, Editor: "operator"}
}

func TestBuildNestsChildrenUnderTheirParent(t *testing.T) {
	out, err := Build(list(t,
		row("g", ""),
		row("a", "g"),
		row("b", "a"),
		row("c", "g"),
		row("t", ""),
	))
	if err != nil {
		t.Fatal(err)
	}
	want := "intro\n\n## Areas\n" +
		"- `g` g title | tool | home-g | operator\n" +
		"  - `a` a title | tool | home-a | operator\n" +
		"    - `b` b title | tool | home-b | operator\n" +
		"  - `c` c title | tool | home-c | operator\n" +
		"- `t` t title | tool | home-t | operator\n" +
		"\n## Tail\n"
	if out != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", out, want)
	}
}

func TestBuildLinksEveryRowOnce(t *testing.T) {
	b, err := os.ReadFile("testdata/policy-index-list.json")
	if err != nil {
		t.Fatal(err)
	}
	l, err := Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(string(b))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range l.Rows {
		if n := strings.Count(out, "- `"+r.Key+"` "); n != 1 {
			t.Errorf("row %q rendered %d times, want 1", r.Key, n)
		}
		if !strings.Contains(out, "| "+r.Home+" | ") {
			t.Errorf("row %q: home %q not in index", r.Key, r.Home)
		}
	}
	if !strings.HasPrefix(out, l.Intro) || !strings.HasSuffix(out, l.Tail) {
		t.Errorf("intro or tail not copied verbatim")
	}
}

func TestValidateRefusesBadLists(t *testing.T) {
	cases := map[string][]Row{
		"empty key":           {{Key: " ", Kind: "tool", Title: "t", Home: "h", Editor: "e"}},
		"duplicate key":       {row("a", ""), row("a", "")},
		"unknown parent":      {row("a", "missing")},
		"self parent":         {row("a", "a")},
		"cycle":               {row("a", "b"), row("b", "a")},
		"cycle behind a root": {row("r", ""), row("a", "b"), row("b", "a")},
		"empty home":          {{Key: "a", Kind: "tool", Title: "t", Home: " ", Editor: "e"}},
		"empty title":         {{Key: "a", Kind: "tool", Home: "h", Editor: "e"}},
		"empty editor":        {{Key: "a", Kind: "tool", Title: "t", Home: "h"}},
		"empty kind":          {{Key: "a", Title: "t", Home: "h", Editor: "e"}},
	}
	for name, rows := range cases {
		if _, err := Build(list(t, rows...)); err == nil {
			t.Errorf("%s: Build accepted the list", name)
		}
	}
}

func TestParseRefusesUnknownFieldsTrailingDataAndOtherVersions(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":   `{"version":1,"intro":"","rows":[],"tail":"","extra":1}`,
		"trailing object": `{"version":1,"intro":"","rows":[],"tail":""} {}`,
		"version 2":       `{"version":2,"intro":"","rows":[],"tail":""}`,
		"not json":        `policy/index body`,
	} {
		if _, err := Parse(body); err == nil {
			t.Errorf("%s: Parse accepted the body", name)
		}
	}
}

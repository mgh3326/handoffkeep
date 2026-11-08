package ciworkflow_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestPRStoreDBDecision runs pr-store-db's real decision step in a scratch git
// repository. A pull request tests every changed Go package that has
// DB-backed tests (hk 3528): a package is DB-backed when one of its _test.go
// files imports internal/testdb at the PR head, and a file belongs to the
// nearest enclosing directory that holds Go files. A change under
// internal/store or internal/testdb still tests ./internal/store/... (hk 3193).
// When the diff cannot be listed the step tests every DB-backed package, and
// when the DB-backed set cannot be computed it tests ./... . Events other than
// pull_request never test here.
func TestPRStoreDBDecision(t *testing.T) {
	var wf tierWorkflow
	if err := yaml.Unmarshal(readWorkflow(t, "ci.yml"), &wf); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, s := range wf.Jobs[prStoreDB].Steps {
		if strings.Contains(s.Run, "GITHUB_OUTPUT") {
			script = s.Run
		}
	}
	if script == "" {
		t.Fatal("pr-store-db has no decision step")
	}

	git := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(t *testing.T, dir, name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A DB-backed test file imports internal/testdb. The import is an escaped
	// literal here so that this file does not itself carry the marker.
	dbTest := func(name string) string {
		return "package " + filepath.Base(filepath.Dir(name)) + "\n\nimport (\n\t\"testing\"\n\n\t\"github.com/mgh3326/handoffkeep/internal/testdb\"\n)\n"
	}
	// The base: store, mcp, api and ui have DB-backed tests; guard has tests
	// that only mention testdb; testdb is the package tests reach PostgreSQL
	// through.
	base := map[string]string{
		"README.md":                        "readme\n",
		"internal/store/old.go":            "package store\n",
		"internal/store/store_test.go":     dbTest("internal/store/store_test.go"),
		"internal/testdb/testdb.go":        "package testdb\n",
		"internal/mcp/old.go":              "package mcp\n",
		"internal/mcp/mcp_test.go":         dbTest("internal/mcp/mcp_test.go"),
		"internal/api/api.go":              "package api\n",
		"internal/api/api_test.go":         dbTest("internal/api/api_test.go"),
		"internal/ui/ui.go":                "package ui\n",
		"internal/ui/ui_test.go":           dbTest("internal/ui/ui_test.go"),
		"internal/ui/templates/board.html": "<p>board</p>\n",
		"internal/guard/guard.go":          "package guard\n",
		"internal/guard/guard_test.go":     "package guard\n\n// Not DB-backed: it only names testdb.URL and \"internal/testdb/testdb.go\".\n",
	}
	everyDB := []string{"./internal/api", "./internal/mcp", "./internal/store/...", "./internal/testdb", "./internal/ui"}

	for _, c := range []struct {
		name    string
		event   string
		changed []string // files added or rewritten on top of the base
		dbTests []string // DB-backed test files added on top of the base
		removed []string // base files deleted
		base    string   // "" = the real base sha, else the literal value
		noIndex bool     // break the index so the DB-backed set cannot be computed
		want    []string // packages tested, sorted; nil = run=no
	}{
		{name: "workflow-only-skips", event: "pull_request", changed: []string{".github/workflows/ci.yml"}},
		{name: "docs-only-skips", event: "pull_request", changed: []string{"docs/ci.md", "README.md"}},
		{name: "non-db-package-skips", event: "pull_request", changed: []string{"internal/guard/guard.go", "internal/guard/guard_test.go"}},
		{name: "store-file-tests", event: "pull_request", changed: []string{"internal/store/task.go"}, want: []string{"./internal/store/..."}},
		{name: "testdb-file-tests", event: "pull_request", changed: []string{"internal/testdb/testdb.go"}, want: []string{"./internal/store/...", "./internal/testdb"}},
		{name: "similar-prefix-skips", event: "pull_request", changed: []string{"internal/storefront/x.go", "internal/store.go"}},
		{name: "moved-out-of-store-tests", event: "pull_request", removed: []string{"internal/store/old.go"}, changed: []string{"internal/guard/old.go"}, want: []string{"./internal/store/..."}},
		{name: "path-with-space-tests", event: "pull_request", changed: []string{"internal/store/a b.go"}, want: []string{"./internal/store/..."}},
		// #243 (hk 3465): store and mcp changed, only store ran, main went red.
		{name: "store-and-mcp-test-both", event: "pull_request", changed: []string{"internal/store/x.go", "internal/mcp/y.go"}, want: []string{"./internal/mcp", "./internal/store/..."}},
		{name: "db-package-outside-store-tests", event: "pull_request", changed: []string{"internal/api/y.go"}, want: []string{"./internal/api"}},
		{name: "removed-file-in-db-package-tests", event: "pull_request", removed: []string{"internal/mcp/old.go"}, want: []string{"./internal/mcp"}},
		{name: "package-asset-tests-its-package", event: "pull_request", changed: []string{"internal/ui/templates/board.html"}, want: []string{"./internal/ui"}},
		{name: "new-db-package-at-head-tests", event: "pull_request", changed: []string{"internal/fresh/fresh.go"}, dbTests: []string{"internal/fresh/fresh_test.go"}, want: []string{"./internal/fresh"}},
		{name: "db-marker-removed-at-head-skips", event: "pull_request", removed: []string{"internal/api/api_test.go"}},
		{name: "empty-base-tests-every-db-package", event: "pull_request", changed: []string{"NOTES.md"}, base: "empty", want: everyDB},
		{name: "unknown-base-tests-every-db-package", event: "pull_request", changed: []string{"NOTES.md"}, base: "0123456789012345678901234567890123456789", want: everyDB},
		{name: "unknown-set-tests-everything", event: "pull_request", changed: []string{"internal/guard/guard.go"}, noIndex: true, want: []string{"./..."}},
		{name: "push-skips", event: "push", changed: []string{"internal/store/task.go", "internal/mcp/y.go"}},
		{name: "dispatch-skips", event: "workflow_dispatch", changed: []string{"internal/store/task.go", "internal/mcp/y.go"}},
		{name: "workflow-call-skips", event: "workflow_call", changed: []string{"internal/store/task.go", "internal/mcp/y.go"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			git(t, dir, "init", "-q", "-b", "main")
			for name, content := range base {
				write(t, dir, name, content)
			}
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-q", "-m", "base")
			baseSHA := git(t, dir, "rev-parse", "HEAD")
			for _, f := range c.removed {
				if err := os.Remove(filepath.Join(dir, f)); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range c.changed {
				write(t, dir, f, f+" changed\n")
			}
			for _, f := range c.dbTests {
				write(t, dir, f, dbTest(f))
			}
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-q", "-m", "change")
			switch c.base {
			case "":
			case "empty":
				baseSHA = ""
			default:
				baseSHA = c.base
			}
			if c.noIndex {
				if err := os.WriteFile(filepath.Join(dir, ".git", "index"), []byte("not an index"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			out := filepath.Join(dir, "..", strings.ReplaceAll(c.name, "/", "_")+".out")
			cmd := exec.Command("bash", "-eo", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "EVENT=" + c.event, "BASE_SHA=" + baseSHA, "GITHUB_OUTPUT=" + out}
			log, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("decision step failed: %v\n%s", err, log)
			}
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			outputs := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				k, v, _ := strings.Cut(line, "=")
				if _, dup := outputs[k]; dup {
					t.Fatalf("GITHUB_OUTPUT sets %s twice:\n%s", k, raw)
				}
				outputs[k] = v
			}
			wantRun := "no"
			if c.want != nil {
				wantRun = "yes"
			}
			got := strings.Fields(outputs["packages"])
			if outputs["run"] != wantRun || !slices.Equal(got, c.want) {
				t.Fatalf("run=%q packages=%q, want run=%s packages=%q\nGITHUB_OUTPUT:\n%s\nlog:\n%s", outputs["run"], got, wantRun, c.want, raw, log)
			}
		})
	}
}

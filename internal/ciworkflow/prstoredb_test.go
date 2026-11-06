package ciworkflow_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestPRStoreDBDecision runs pr-store-db's real decision step in a scratch git
// repository: a pull request tests when it changes internal/store or
// internal/testdb, skips otherwise, and tests when the diff cannot be
// computed (hk 3193).
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
	write := func(t *testing.T, dir, name string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct {
		name    string
		event   string
		changed []string // files added on top of the base
		removed []string // base files deleted
		base    string   // "" = the real base sha, else the literal value
		want    string
	}{
		{name: "workflow-only-skips", event: "pull_request", changed: []string{".github/workflows/ci.yml"}, want: "no"},
		{name: "store-file-tests", event: "pull_request", changed: []string{"internal/store/task.go"}, want: "yes"},
		{name: "testdb-file-tests", event: "pull_request", changed: []string{"internal/testdb/testdb.go"}, want: "yes"},
		{name: "similar-prefix-skips", event: "pull_request", changed: []string{"internal/storefront/x.go", "internal/store.go"}, want: "no"},
		{name: "moved-out-of-store-tests", event: "pull_request", removed: []string{"internal/store/old.go"}, changed: []string{"internal/api/old.go"}, want: "yes"},
		{name: "path-with-space-tests", event: "pull_request", changed: []string{"internal/store/a b.go"}, want: "yes"},
		{name: "empty-base-tests", event: "pull_request", changed: []string{"NOTES.md"}, base: "empty", want: "yes"},
		{name: "unknown-base-tests", event: "pull_request", changed: []string{"NOTES.md"}, base: "0123456789012345678901234567890123456789", want: "yes"},
		{name: "push-skips", event: "push", changed: []string{"internal/store/task.go"}, want: "no"},
		{name: "dispatch-skips", event: "workflow_dispatch", changed: []string{"internal/store/task.go"}, want: "no"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			git(t, dir, "init", "-q", "-b", "main")
			write(t, dir, "internal/store/old.go")
			write(t, dir, "README.md")
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-q", "-m", "base")
			base := git(t, dir, "rev-parse", "HEAD")
			for _, f := range c.removed {
				if err := os.Remove(filepath.Join(dir, f)); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range c.changed {
				write(t, dir, f)
			}
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-q", "-m", "change")
			switch c.base {
			case "":
			case "empty":
				base = ""
			default:
				base = c.base
			}

			out := filepath.Join(dir, "..", strings.ReplaceAll(c.name, "/", "_")+".out")
			cmd := exec.Command("bash", "-eo", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "EVENT=" + c.event, "BASE_SHA=" + base, "GITHUB_OUTPUT=" + out}
			log, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("decision step failed: %v\n%s", err, log)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != "run="+c.want {
				t.Fatalf("GITHUB_OUTPUT = %q, want run=%s\nlog:\n%s", got, c.want, log)
			}
		})
	}
}

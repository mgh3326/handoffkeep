package ciworkflow_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// release.yml ci-green decides publication from ci.yml's ci-ok (hk 3023).
// The latest check run's verdict wins; a ci-any-ref status counts only when
// the check run has none, so a stale success status never hides a failed
// main run (review on #151). Runs the step's own script with a fake gh.
func TestReleaseCIGreenVerdicts(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatal(err)
	}
	job, ok := wf.Jobs["ci-green"]
	if !ok || len(job.Steps) != 1 {
		t.Fatal("release.yml must have a one-step ci-green job")
	}
	script := strings.Replace(job.Steps[0].Run, "sleep 30", "exit 7 # pending: the test stops instead of waiting", 1)
	for _, tc := range []struct {
		name, check, status string
		code                int
		green               string
	}{
		{"failed check beats stale success status", "completed failure", "success", 1, ""},
		{"timed out check beats success status", "completed timed_out", "success", 1, ""},
		{"check success publishes", "completed success", "none", 0, "true"},
		{"superseded check, any-ref success publishes", "completed cancelled", "success", 0, "true"},
		{"superseded check, any-ref failure blocks", "completed skipped", "error", 1, ""},
		{"superseded check, no status skips publication", "completed cancelled", "none", 0, "false"},
		{"no check run, any-ref success publishes", "none ", "success", 0, "true"},
		{"no check run, any-ref failure blocks", "none ", "failure", 1, ""},
		{"running check waits even with success status", "in_progress ", "success", 7, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gh := "#!/bin/sh\ncase \"$*\" in *check-runs*) printf '%s\\n' \"$FAKE_CHECK\" ;; *statuses*) printf '%s\\n' \"$FAKE_STATUS\" ;; esac\n"
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "out")
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GITHUB_OUTPUT="+out, "GITHUB_SHA=abc", "GITHUB_REPOSITORY=o/r", "FAKE_CHECK="+tc.check, "FAKE_STATUS="+tc.status)
			logs, err := cmd.CombinedOutput()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(out)
			green := strings.TrimPrefix(strings.TrimSpace(string(got)), "green=")
			if code != tc.code || green != tc.green {
				t.Fatalf("exit=%d green=%q, want exit=%d green=%q\n%s", code, green, tc.code, tc.green, logs)
			}
		})
	}
}

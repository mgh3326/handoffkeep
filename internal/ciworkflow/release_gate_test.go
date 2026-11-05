package ciworkflow_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// release.yml ci-green decides publication from ci.yml's ci-ok (hk 3023).
// The latest check run's verdict wins; a ci-any-ref status counts only once
// main CI is known to be superseded, so a stale success status never hides a
// failed or not-yet-started main run (reviews on #151). Runs the step's own script with a fake gh.
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
		name, check, status, main string
		code                      int
		green                     string
	}{
		{"failed check beats stale success status", "completed failure", "success", "", 1, ""},
		{"timed out check beats success status", "completed timed_out", "success", "", 1, ""},
		{"check success publishes", "completed success", "none", "", 0, "true"},
		{"superseded check, any-ref success publishes", "completed cancelled", "success", "", 0, "true"},
		{"superseded check, any-ref failure blocks", "completed skipped", "error", "", 1, ""},
		{"superseded check, no status skips publication", "completed cancelled", "none", "", 0, "false"},
		{"no check run yet, main run queued: stale success waits", "none ", "success", "queued ", 7, ""},
		{"no check run yet, no main run: stale success waits", "none ", "success", "none ", 7, ""},
		{"no check run yet, main in progress: any-ref failure waits", "none ", "failure", "in_progress ", 7, ""},
		{"main run cancelled before ci-ok, any-ref success publishes", "none ", "success", "completed cancelled", 0, "true"},
		{"main run cancelled before ci-ok, any-ref failure blocks", "none ", "failure", "completed cancelled", 1, ""},
		{"main run cancelled before ci-ok, no status skips publication", "none ", "none", "completed cancelled", 0, "false"},
		{"main run failed without ci-ok blocks despite success status", "none ", "success", "completed startup_failure", 1, ""},
		{"running check waits even with success status", "in_progress ", "success", "", 7, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gh := "#!/bin/sh\ncase \"$*\" in *check-runs*) printf '%s\\n' \"$FAKE_CHECK\" ;; *statuses*) printf '%s\\n' \"$FAKE_STATUS\" ;; *workflows/ci.yml/runs*) printf '%s\\n' \"$FAKE_MAIN\" ;; esac\n"
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "out")
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GITHUB_OUTPUT="+out, "GITHUB_SHA=abc", "GITHUB_REPOSITORY=o/r", "FAKE_CHECK="+tc.check, "FAKE_STATUS="+tc.status, "FAKE_MAIN="+tc.main)
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

// The release gate waits for main CI, so its deadline must outlast the
// slowest ci.yml job plus queue time, and its job timeout must outlast the
// deadline (review on #151, release.yml:125).
func TestReleaseGateOutlastsCI(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var ci struct {
		Jobs map[string]struct {
			Timeout int `yaml:"timeout-minutes"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(read("ci.yml"), &ci); err != nil {
		t.Fatal(err)
	}
	slowest := 0
	for _, j := range ci.Jobs {
		if j.Timeout > slowest {
			slowest = j.Timeout
		}
	}
	var rel struct {
		Jobs map[string]struct {
			Timeout int    `yaml:"timeout-minutes"`
			Steps   []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(read("release.yml"), &rel); err != nil {
		t.Fatal(err)
	}
	gate := rel.Jobs["ci-green"]
	m := regexp.MustCompile(`deadline=\$\(\( \$\(date \+%s\) \+ (\d+)\*60 \)\)`).FindStringSubmatch(gate.Steps[0].Run)
	if m == nil {
		t.Fatal("ci-green deadline not found")
	}
	deadline, _ := strconv.Atoi(m[1])
	if slowest == 0 || deadline < slowest+60 || gate.Timeout <= deadline {
		t.Fatalf("slowest ci.yml job %d min, gate deadline %d min, gate job timeout %d min: want deadline >= slowest+60 and timeout > deadline", slowest, deadline, gate.Timeout)
	}
}

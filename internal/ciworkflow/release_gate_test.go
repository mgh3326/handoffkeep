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
// The ci-ok job of ci.yml's main push run is the verdict, so another run of
// the same SHA cannot override it; a ci-any-ref status counts only once that
// run is known superseded; a failed API call waits (reviews on #151). Runs the step's own script with a fake gh.
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
		name, main, job, status, fail string
		code                          int
		green                         string
	}{
		{"main ci-ok success publishes", "9 completed success", "success", "none", "", 0, "true"},
		{"main ci-ok failure beats stale success status", "9 completed failure", "failure", "success", "", 1, ""},
		{"main ci-ok timed out blocks", "9 completed failure", "timed_out", "success", "", 1, ""},
		{"main run failed before ci-ok blocks despite success status", "9 completed startup_failure", "none", "success", "", 1, ""},
		{"main run failed with ci-ok skipped blocks", "9 completed failure", "skipped", "success", "", 1, ""},
		{"no main run yet: stale success waits", "0 none ", "", "success", "", 7, ""},
		{"main run queued: stale success waits", "9 queued ", "", "success", "", 7, ""},
		{"main run in progress: any-ref failure waits", "9 in_progress ", "", "failure", "", 7, ""},
		{"superseded, ci-ok skipped, any-ref success publishes", "9 completed cancelled", "skipped", "success", "", 0, "true"},
		{"superseded before ci-ok, any-ref success publishes", "9 completed cancelled", "none", "success", "", 0, "true"},
		{"superseded, any-ref failure blocks", "9 completed cancelled", "skipped", "error", "", 1, ""},
		{"superseded, any-ref pending waits", "9 completed cancelled", "skipped", "pending", "", 7, ""},
		{"superseded, no status skips publication", "9 completed cancelled", "cancelled", "none", "", 0, "false"},
		{"ci-ok failure in a cancelled run blocks", "9 completed cancelled", "failure", "success", "", 1, ""},
		{"runs API failure waits", "9 completed success", "success", "none", "workflows/ci.yml/runs", 7, ""},
		{"jobs API failure waits", "9 completed success", "success", "none", "/jobs", 7, ""},
		{"statuses API failure waits", "9 completed cancelled", "skipped", "success", "statuses", 7, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// The fake answers each endpoint from the case and rejects any
			// other query, so the verdict must come from the main push run's
			// latest ci-ok job. FAKE_FAIL names an endpoint that fails, as a
			// transient API error would.
			gh := `#!/bin/sh
case "$*" in *"$FAKE_FAIL"*) [ -n "$FAKE_FAIL" ] && exit 1 ;; esac
case "$*" in
  *'actions/workflows/ci.yml/runs?head_sha=abc&event=push&branch=main&'*) printf '%s\n' "$FAKE_MAIN" ;;
  *'actions/runs/9/jobs?filter=latest&'*) printf '%s\n' "$FAKE_JOB" ;;
  *statuses*) printf '%s\n' "$FAKE_STATUS" ;;
  *) echo "unexpected gh $*" >&2; exit 2 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "out")
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GITHUB_OUTPUT="+out, "GITHUB_SHA=abc", "GITHUB_REPOSITORY=o/r", "FAKE_MAIN="+tc.main, "FAKE_JOB="+tc.job, "FAKE_STATUS="+tc.status, "FAKE_FAIL="+tc.fail)
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

package ciworkflow_test

import (
	"fmt"
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
			Env   map[string]string `yaml:"env"`
			Steps []step            `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatal(err)
	}
	job, ok := wf.Jobs["ci-green"]
	if !ok || len(job.Steps) != 1 {
		t.Fatal("release.yml must have a one-step ci-green job")
	}
	// The gate is one job-level env value; the shipped default must stay ci-ok
	// until mergewake reads deploy_checks (decision §2, §4-3, hk 3440).
	if got := job.Env["GATE"]; got != "ci-ok" {
		t.Fatalf("ci-green GATE = %q, want ci-ok (the switch to deploy-ok waits for mergewake)", got)
	}
	script := strings.ReplaceAll(job.Steps[0].Run, "sleep 30", "exit 7 # pending: the test stops instead of waiting")
	for _, tc := range []struct {
		name, gate, main, job, status, newest, fail string
		code                                        int
		green                                       string
	}{
		{"main ci-ok success publishes", "ci-ok", "9 completed 2026-11-05T10:00:00Z success", "success", "none", "def 2026-11-05T10:05:00Z", "", 0, "true"},
		{"main ci-ok failure beats stale success status", "ci-ok", "9 completed 2026-11-05T10:00:00Z failure", "failure", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"main ci-ok timed out blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z failure", "timed_out", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"main run failed before ci-ok blocks despite success status", "ci-ok", "9 completed 2026-11-05T10:00:00Z startup_failure", "none", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"main run failed with ci-ok skipped blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z failure", "skipped", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"no main run yet: stale success waits", "ci-ok", "0 none none ", "", "success", "def 2026-11-05T10:05:00Z", "", 7, ""},
		{"main run queued: stale success waits", "ci-ok", "9 queued 2026-11-05T10:00:00Z ", "", "success", "def 2026-11-05T10:05:00Z", "", 7, ""},
		{"main run in progress: any-ref failure waits", "ci-ok", "9 in_progress 2026-11-05T10:00:00Z ", "", "failure", "def 2026-11-05T10:05:00Z", "", 7, ""},
		{"superseded, ci-ok skipped, any-ref success publishes", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "success", "def 2026-11-05T10:05:00Z", "", 0, "true"},
		{"superseded before ci-ok, any-ref success publishes", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "none", "success", "def 2026-11-05T10:05:00Z", "", 0, "true"},
		{"superseded, any-ref failure blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "error", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"superseded, any-ref pending waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "pending", "def 2026-11-05T10:05:00Z", "", 7, ""},
		{"superseded, no status skips publication", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "cancelled", "none", "def 2026-11-05T10:05:00Z", "", 0, "false"},
		{"ci-ok failure in a cancelled run blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "failure", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"runs API failure waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z success", "success", "none", "def 2026-11-05T10:05:00Z", "workflows/ci.yml/runs", 7, ""},
		{"jobs API failure waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z success", "success", "none", "def 2026-11-05T10:05:00Z", "/jobs", 7, ""},
		{"manual cancel, no newer main push, blocks despite success status", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "none", "success", "abc 2026-11-05T10:00:00Z", "", 1, ""},
		{"cancelled, newest main run is older, blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "success", "def 2026-11-05T09:00:00Z", "", 1, ""},
		{"newest-run API failure waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "success", "def 2026-11-05T10:05:00Z", "runs?event=push", 7, ""},
		{"statuses API failure waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "success", "def 2026-11-05T10:05:00Z", "statuses", 7, ""},
		// GATE=deploy-ok: the switch decision §4-3 will make. The verdict is
		// the deploy-ok status alone; no main-run lookup decides it.
		{"deploy-ok success publishes", "deploy-ok", "", "", "success", "", "", 0, "true"},
		{"deploy-ok failure blocks", "deploy-ok", "", "", "failure", "", "", 1, ""},
		{"deploy-ok error blocks", "deploy-ok", "", "", "error", "", "", 1, ""},
		{"deploy-ok pending waits", "deploy-ok", "", "", "pending", "", "", 7, ""},
		{"deploy-ok absent waits", "deploy-ok", "", "", "none", "", "", 7, ""},
		{"deploy-ok statuses API failure waits", "deploy-ok", "", "", "success", "", "statuses", 7, ""},
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
  *'actions/workflows/ci.yml/runs?event=push&branch=main&per_page=1'*) printf '%s\n' "$FAKE_NEWEST" ;;
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
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GITHUB_OUTPUT="+out, "GITHUB_SHA=abc", "GITHUB_REPOSITORY=o/r", "GATE="+tc.gate, "FAKE_MAIN="+tc.main, "FAKE_JOB="+tc.job, "FAKE_STATUS="+tc.status, "FAKE_NEWEST="+tc.newest, "FAKE_FAIL="+tc.fail)
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

// deploy.yml posts deploy-ok on the head its successful deploy tier checked,
// so GATE=deploy-ok needs only the statuses API. TestReleaseDeployGateMayReadDeployOK
// keeps that path honest: the same script under GATE=deploy-ok must not call
// the run or jobs endpoints (deploy.yml posts for every completed run, so no
// superseded-run fallback exists on that path).
func TestReleaseDeployGateUsesStatusesOnly(t *testing.T) {
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
	script := strings.ReplaceAll(wf.Jobs["ci-green"].Steps[0].Run, "sleep 30", "exit 7")
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	gh := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  *statuses*) printf 'success\n' ;;
  *) exit 2 ;;
esac
`, calls)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GITHUB_OUTPUT="+out, "GITHUB_SHA=abc", "GITHUB_REPOSITORY=o/r", "GATE=deploy-ok")
	if logs, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy-ok gate failed: %v\n%s", err, logs)
	}
	got, _ := os.ReadFile(calls)
	if !strings.Contains(string(got), "commits/abc/statuses") {
		t.Fatalf("deploy-ok gate did not read the commit statuses: %s", got)
	}
	if strings.Contains(string(got), "actions/workflows") || strings.Contains(string(got), "/jobs") {
		t.Fatalf("deploy-ok gate must not look up ci.yml runs or jobs: %s", got)
	}
	if v := strings.TrimSpace(string(mustRead(t, out))); v != "green=true" {
		t.Fatalf("GITHUB_OUTPUT = %q, want green=true", v)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
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

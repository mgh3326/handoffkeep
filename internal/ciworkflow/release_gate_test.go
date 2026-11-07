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
	// deploy is the deploy-ok status beside the ci-ok one: the shipped gate
	// requires both (review on #228, high), so a ci-ok success alone no longer
	// publishes.
	for _, tc := range []struct {
		name, gate, main, job, status, deploy, newest, fail string
		code                                                int
		green                                               string
	}{
		// Shipped GATE=ci-ok also requires deploy-ok: a green main tier without
		// deploy-tier evidence does not publish (review on #228, high).
		{"main ci-ok success, deploy-ok failure blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z success", "success", "none", "failure", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"main ci-ok success, deploy-ok absent waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z success", "success", "none", "none", "def 2026-11-05T10:05:00Z", "", 7, ""},
		{"main ci-ok success publishes", "ci-ok", "9 completed 2026-11-05T10:00:00Z success", "success", "none", "success", "def 2026-11-05T10:05:00Z", "", 0, "true"},
		{"main ci-ok failure beats stale success status", "ci-ok", "9 completed 2026-11-05T10:00:00Z failure", "failure", "success", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"main ci-ok timed out blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z failure", "timed_out", "success", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"main run failed before ci-ok blocks despite success status", "ci-ok", "9 completed 2026-11-05T10:00:00Z startup_failure", "none", "success", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"main run failed with ci-ok skipped blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z failure", "skipped", "success", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"no main run yet: stale success waits", "ci-ok", "0 none none ", "", "success", "success", "def 2026-11-05T10:05:00Z", "", 7, ""},
		{"main run queued: stale success waits", "ci-ok", "9 queued 2026-11-05T10:00:00Z ", "", "success", "success", "def 2026-11-05T10:05:00Z", "", 7, ""},
		{"main run in progress: any-ref failure waits", "ci-ok", "9 in_progress 2026-11-05T10:00:00Z ", "", "failure", "success", "def 2026-11-05T10:05:00Z", "", 7, ""},
		{"superseded, ci-ok skipped, any-ref success publishes", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "success", "success", "def 2026-11-05T10:05:00Z", "", 0, "true"},
		{"superseded before ci-ok, any-ref success publishes", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "none", "success", "success", "def 2026-11-05T10:05:00Z", "", 0, "true"},
		{"superseded, any-ref failure blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "error", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"superseded, any-ref pending waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "pending", "success", "def 2026-11-05T10:05:00Z", "", 7, ""},
		{"superseded, no status skips publication", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "cancelled", "none", "success", "def 2026-11-05T10:05:00Z", "", 0, "false"},
		{"ci-ok failure in a cancelled run blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "failure", "success", "success", "def 2026-11-05T10:05:00Z", "", 1, ""},
		{"runs API failure waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z success", "success", "none", "success", "def 2026-11-05T10:05:00Z", "workflows/ci.yml/runs", 7, ""},
		{"jobs API failure waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z success", "success", "none", "success", "def 2026-11-05T10:05:00Z", "/jobs", 7, ""},
		{"manual cancel, no newer main push, blocks despite success status", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "none", "success", "success", "abc 2026-11-05T10:00:00Z", "", 1, ""},
		{"cancelled, newest main run is older, blocks", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "success", "success", "def 2026-11-05T09:00:00Z", "", 1, ""},
		{"newest-run API failure waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "success", "success", "def 2026-11-05T10:05:00Z", "runs?event=push", 7, ""},
		{"statuses API failure waits", "ci-ok", "9 completed 2026-11-05T10:00:00Z cancelled", "skipped", "success", "success", "def 2026-11-05T10:05:00Z", "statuses", 7, ""},
		// GATE=deploy-ok: the switch decision §4-3 will make. The verdict is
		// the deploy-ok status alone; no main-run lookup decides it.
		{"deploy-ok success publishes", "deploy-ok", "", "", "none", "success", "", "", 0, "true"},
		{"deploy-ok failure blocks", "deploy-ok", "", "", "none", "failure", "", "", 1, ""},
		{"deploy-ok error blocks", "deploy-ok", "", "", "none", "error", "", "", 1, ""},
		{"deploy-ok pending waits", "deploy-ok", "", "", "none", "pending", "", "", 7, ""},
		{"deploy-ok absent waits", "deploy-ok", "", "", "none", "none", "", "", 7, ""},
		{"deploy-ok statuses API failure waits", "deploy-ok", "", "", "none", "success", "", "statuses", 7, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// The fake answers each endpoint from the case and rejects any
			// other query, so the verdict must come from the main push run's
			// latest ci-ok job and the combined commit status. It answers the
			// combined status endpoint (commits/<sha>/status), whose statuses
			// list is the latest state per context, not the paged list: the
			// plain list is newest-30 only (review on #228, medium).
			// FAKE_FAIL names an endpoint that fails, as a transient API error
			// would.
			gh := `#!/bin/sh
case "$*" in *"$FAKE_FAIL"*) [ -n "$FAKE_FAIL" ] && exit 1 ;; esac
case "$*" in
  *'actions/workflows/ci.yml/runs?head_sha=abc&event=push&branch=main&'*) printf '%s\n' "$FAKE_MAIN" ;;
  *'actions/workflows/ci.yml/runs?event=push&branch=main&per_page=1'*) printf '%s\n' "$FAKE_NEWEST" ;;
  *'actions/runs/9/jobs?filter=latest&'*) printf '%s\n' "$FAKE_JOB" ;;
  *'select(.context == "deploy-ok")'*) printf '%s\n' "$FAKE_DEPLOY" ;;
  *'select(.context == "ci-ok")'*) printf '%s\n' "$FAKE_STATUS" ;;
  *) echo "unexpected gh $*" >&2; exit 2 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "out")
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GITHUB_OUTPUT="+out, "GITHUB_SHA=abc", "GITHUB_REPOSITORY=o/r", "GATE="+tc.gate, "FAKE_DEPLOY="+tc.deploy, "FAKE_MAIN="+tc.main, "FAKE_JOB="+tc.job, "FAKE_STATUS="+tc.status, "FAKE_NEWEST="+tc.newest, "FAKE_FAIL="+tc.fail)
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

// TestReleaseDeployOKShape runs ci-green's real deploy_ok_state against
// combined-status response shapes. Only a well-formed body with an array
// .statuses may answer "none"; anything else must answer "unknown" so the gate
// retries instead of treating a malformed 2xx response as "no deploy-ok yet"
// (review on #228, high).
func TestReleaseDeployOKShape(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
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
	script := wf.Jobs["ci-green"].Steps[0].Run
	// Extract deploy_ok_state so the test cannot drift from the real function.
	start := strings.Index(script, "deploy_ok_state() {")
	if start < 0 {
		t.Fatal("ci-green has no deploy_ok_state function")
	}
	end := strings.Index(script[start:], "\n}\n")
	if end < 0 {
		t.Fatal("deploy_ok_state function is not closed")
	}
	fn := script[start : start+end+len("\n}")]

	for _, c := range []struct {
		name, response, want string
	}{
		{"deploy-ok-success", `{"statuses":[{"context":"deploy-ok","state":"success"}]}`, "success"},
		{"deploy-ok-failure", `{"statuses":[{"context":"deploy-ok","state":"failure"}]}`, "failure"},
		{"deploy-ok-pending", `{"statuses":[{"context":"deploy-ok","state":"pending"}]}`, "pending"},
		{"no-deploy-ok-among-statuses", `{"statuses":[{"context":"ci-ok","state":"success"}]}`, "none"},
		{"empty-statuses", `{"statuses":[]}`, "none"},
		{"missing-statuses-key", `{"state":"success"}`, "unknown"},
		{"null-statuses", `{"statuses":null}`, "unknown"},
		{"statuses-not-an-array", `{"statuses":{}}`, "unknown"},
		{"null-body", `null`, "unknown"},
		{"array-body", `[]`, "unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			// Fake gh api: prints the body, and applies a -q jq filter the way
			// the real CLI does (including a non-zero exit on a jq error).
			gh := fmt.Sprintf(`#!/bin/sh
resp=%q
expr=""
seen=0
for a in "$@"; do
  if [ "$seen" = 1 ]; then expr=$a; break; fi
  [ "$a" = "-q" ] && seen=1
done
if [ -n "$expr" ]; then
  printf '%%s' "$resp" | jq -r "$expr"
else
  printf '%%s\n' "$resp"
fi
`, c.response)
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", fn+"\ndeploy_ok_state")
			cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "GITHUB_REPOSITORY=o/r", "GITHUB_SHA=abc"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("deploy_ok_state failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != c.want {
				t.Fatalf("deploy_ok_state(%s) = %q, want %q", c.response, got, c.want)
			}
		})
	}
}

// deploy.yml posts deploy-ok on the head its successful deploy tier checked,
// so proving the deploy tier needs only the statuses API: under GATE=deploy-ok
// the script must not call the run or jobs endpoints at all (deploy.yml posts
// for every completed run, so no superseded-run fallback exists).
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
  *'select(.context == "deploy-ok")'*) printf 'success\n' ;;
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
	// The combined status endpoint, not the newest-30 paged list (review on
	// #228, medium): the latter can drop an existing deploy-ok.
	if !strings.Contains(string(got), "commits/abc/status ") {
		t.Fatalf("deploy-ok gate did not read the combined commit status: %s", got)
	}
	if strings.Contains(string(got), "commits/abc/statuses") {
		t.Fatalf("deploy-ok gate must not read the paged statuses list: %s", got)
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

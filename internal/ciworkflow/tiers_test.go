package ciworkflow_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// CI tiers (hk 2935, hk:doc decision/2026-11-05/fleet-ci-three-tiers).
// ci.yml splits into T-PR, the jobs a pull request runs, and T-POST, the
// post-* jobs guarded by postGuard that every other event runs (main push,
// dispatch, and the ci-any-ref call). These checks keep the PR tier light and
// keep PR code off self-hosted runners, since the repository is public.

const postGuard = "github.event_name != 'pull_request'"

// ciOkIf is the hk 2924 condition: a run whose only non-success is cancelled
// skips ci-ok (unknown) instead of failing it.
const ciOkIf = "always() && (contains(needs.*.result, 'failure') || !contains(needs.*.result, 'cancelled'))"

type tierJob struct {
	If       string         `yaml:"if"`
	RunsOn   any            `yaml:"runs-on"`
	Services map[string]any `yaml:"services"`
	Needs    any            `yaml:"needs"`
	Uses     string         `yaml:"uses"`
	Steps    []step         `yaml:"steps"`
}

type tierWorkflow struct {
	On          map[string]any     `yaml:"on"`
	Concurrency map[string]any     `yaml:"concurrency"`
	Jobs        map[string]tierJob `yaml:"jobs"`
}

func runsOnText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		parts := make([]string, len(x))
		for i, p := range x {
			parts[i] = fmt.Sprint(p)
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(v)
}

func needsList(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, len(x))
		for i, p := range x {
			out[i] = fmt.Sprint(p)
		}
		return out
	}
	return nil
}

func hasRace(j tierJob) bool {
	for _, s := range j.Steps {
		if strings.Contains(s.Run, "-race") {
			return true
		}
	}
	return false
}

// prRunnerProblems reports jobs a pull request reaches that could run on a
// self-hosted runner.
func prRunnerProblems(file string, wf tierWorkflow) []string {
	if _, ok := wf.On["pull_request"]; !ok {
		return nil
	}
	var out []string
	for name, j := range wf.Jobs {
		if strings.Contains(j.If, postGuard) {
			continue
		}
		ro := runsOnText(j.RunsOn)
		if strings.Contains(ro, "vars.") || strings.Contains(ro, "self-hosted") {
			out = append(out, fmt.Sprintf("%s job %s: pull requests reach it, runs-on %q may be self-hosted", file, name, ro))
		}
	}
	return out
}

// tierProblems checks ci.yml and ci-any-ref.yml against the tier contract.
func tierProblems(ci, anyRef []byte) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }
	var wf tierWorkflow
	if err := yaml.Unmarshal(ci, &wf); err != nil {
		return []string{err.Error()}
	}
	for _, ev := range []string{"pull_request", "push", "workflow_call"} {
		if _, ok := wf.On[ev]; !ok {
			add("ci.yml must trigger on %s", ev)
		}
	}
	// Only the newest main head runs: the main push group is keyed on the ref
	// and a newer push cancels the older run.
	if wf.Concurrency["cancel-in-progress"] != true || !strings.Contains(fmt.Sprint(wf.Concurrency["group"]), "github.ref") {
		add("ci.yml concurrency must key on github.ref with cancel-in-progress: true")
	}

	var prJobs, postJobs []string
	for name, j := range wf.Jobs {
		if strings.Contains(j.If, postGuard) {
			postJobs = append(postJobs, name)
			if !strings.HasPrefix(name, "post-") {
				add("job %s: post-tier jobs are named post-*", name)
			}
			continue
		}
		prJobs = append(prJobs, name)
		if strings.HasPrefix(name, "post-") {
			add("job %s: a post-* job must be guarded by %q", name, postGuard)
		}
		if hasRace(j) {
			add("job %s: pull requests reach it and it runs -race", name)
		}
		if len(j.Services) > 0 {
			add("job %s: pull requests reach it and it starts service containers", name)
		}
		if ro := runsOnText(j.RunsOn); ro != "ubuntu-latest" {
			add("job %s: pull requests reach it and runs-on is %q, want hosted ubuntu-latest", name, ro)
		}
	}
	sort.Strings(prJobs)
	sort.Strings(postJobs)

	race, postgres, macos := false, false, false
	for _, name := range postJobs {
		j := wf.Jobs[name]
		race = race || hasRace(j)
		if _, ok := j.Services["postgres"]; ok {
			postgres = true
		}
		ro := runsOnText(j.RunsOn)
		if strings.Contains(ro, "macos-latest") {
			macos = true
		}
		if !strings.Contains(ro, "vars.HANDOFFKEEP_POST_") || !strings.Contains(ro, "|| '") {
			add("job %s: runs-on %q must read a HANDOFFKEEP_POST_* variable with a hosted fallback", name, ro)
		}
	}
	if !race || !postgres || !macos {
		add("post tier must run -race (%v), PostgreSQL (%v) and macOS (%v)", race, postgres, macos)
	}

	ok, found := wf.Jobs["ci-ok"]
	switch {
	case !found:
		add("ci.yml must have a ci-ok aggregate job")
	default:
		if strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(ok.If), "${{"), "}}")) != ciOkIf {
			add("ci-ok if must be %q (hk 2924), got %q", ciOkIf, ok.If)
		}
		needs := map[string]bool{}
		for _, n := range needsList(ok.Needs) {
			needs[n] = true
		}
		for _, name := range append(append([]string{}, prJobs...), postJobs...) {
			if name != "ci-ok" && !needs[name] {
				add("ci-ok must need %s", name)
			}
		}
	}

	var ar tierWorkflow
	if err := yaml.Unmarshal(anyRef, &ar); err != nil {
		return append(out, err.Error())
	}
	// A dispatch reaches the post tier only because its event is not
	// pull_request; mergewake bisects with it, so it must run the full suite.
	if _, ok := ar.On["pull_request"]; ok {
		add("ci-any-ref.yml must not trigger on pull_request")
	}
	calls := false
	for _, j := range ar.Jobs {
		calls = calls || j.Uses == "./.github/workflows/ci.yml"
	}
	if !calls {
		add("ci-any-ref.yml must call ci.yml")
	}
	return out
}

func readWorkflow(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCITiers(t *testing.T) {
	for _, p := range tierProblems(readWorkflow(t, "ci.yml"), readWorkflow(t, "ci-any-ref.yml")) {
		t.Error(p)
	}
}

// No job a pull request reaches, in any workflow, may name a runner variable
// or a self-hosted label.
func TestPullRequestsStayOnHostedRunners(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "workflows")
	for _, name := range workflowFiles(t, dir) {
		var wf tierWorkflow
		if err := yaml.Unmarshal(readWorkflow(t, name), &wf); err != nil {
			t.Fatal(err)
		}
		for _, p := range prRunnerProblems(name, wf) {
			t.Error(p)
		}
	}
}

// Each mutant breaks one rule; the checks must catch every one.
func TestCITierMutantsTurnRed(t *testing.T) {
	ci := string(readWorkflow(t, "ci.yml"))
	anyRef := readWorkflow(t, "ci-any-ref.yml")
	for _, m := range []struct{ name, old, new string }{
		{"race-in-pr-test", "run: go test -timeout 10m ./...", "run: go test -race -timeout 10m ./..."},
		{"self-hosted-pr-test", "    runs-on: ubuntu-latest\n    timeout-minutes: 15\n", "    runs-on: [self-hosted, linux]\n    timeout-minutes: 15\n"},
		{"runner-variable-pr-test", "    runs-on: ubuntu-latest\n    timeout-minutes: 15\n", "    runs-on: ${{ vars.HANDOFFKEEP_POST_LINUX_RUNNER || 'ubuntu-latest' }}\n    timeout-minutes: 15\n"},
		{"no-concurrency", "concurrency: {group:", "x-concurrency: {group:"},
		{"no-cancel-in-progress", "cancel-in-progress: true}", "cancel-in-progress: false}"},
		{"post-linux-unguarded", "if: ${{ github.event_name != 'pull_request' && contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}\n    name: 'post: race + postgres'", "if: ${{ contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}\n    name: 'post: race + postgres'"},
		{"ci-ok-always", "if: ${{ " + ciOkIf + " }}", "if: ${{ always() }}"},
		{"no-macos", "vars.HANDOFFKEEP_POST_MAC_RUNNER || 'macos-latest'", "vars.HANDOFFKEEP_POST_MAC_RUNNER || 'ubuntu-latest'"},
	} {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(ci, m.old) != 1 {
				t.Fatalf("mutant anchor %q matches %d times", m.old, strings.Count(ci, m.old))
			}
			mutant := strings.Replace(ci, m.old, m.new, 1)
			problems := tierProblems([]byte(mutant), anyRef)
			var wf tierWorkflow
			if err := yaml.Unmarshal([]byte(mutant), &wf); err == nil {
				problems = append(problems, prRunnerProblems("ci.yml", wf)...)
			}
			if len(problems) == 0 {
				t.Fatal("mutant passed the tier checks")
			}
		})
	}
}

package ciworkflow_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// mainPush gates every runner variable: only reviewed, merged code on a main
// push may reach a self-hosted runner. A dispatch (ci-any-ref) checks out an
// arbitrary ref, so it stays hosted.
const mainPush = "github.event_name == 'push' && github.ref == 'refs/heads/main'"

// ciOkIf is the hk 2924 condition: a run whose only non-success is cancelled
// or abandoned (no runner acquired, run 42061203904, hk 3155) skips ci-ok
// (unknown) instead of failing it.
const ciOkIf = "always() && (contains(needs.*.result, 'failure') || !(contains(needs.*.result, 'cancelled') || contains(needs.*.result, 'abandoned')))"

// ciOkIfCancelledOnly is the hk 2924 guard before hk 3155; it fails ci-ok on
// an abandoned job.
const ciOkIfCancelledOnly = "always() && (contains(needs.*.result, 'failure') || !contains(needs.*.result, 'cancelled'))"

type tierJob struct {
	Name        string         `yaml:"name"`
	If          string         `yaml:"if"`
	Env         map[string]any `yaml:"env"`
	RunsOn      any            `yaml:"runs-on"`
	Services    map[string]any `yaml:"services"`
	Needs       any            `yaml:"needs"`
	Uses        string         `yaml:"uses"`
	Steps       []step         `yaml:"steps"`
	Concurrency map[string]any `yaml:"concurrency"`
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

// selfHostedProblems reports, in any workflow, a job whose runner may be
// self-hosted without the main-push gate in its runs-on expression.
func selfHostedProblems(file string, wf tierWorkflow) []string {
	var out []string
	for name, j := range wf.Jobs {
		ro := runsOnText(j.RunsOn)
		if (strings.Contains(ro, "vars.") || strings.Contains(ro, "self-hosted")) && !strings.Contains(ro, "("+mainPush+" && vars.") {
			out = append(out, fmt.Sprintf("%s job %s: runs-on %q may be self-hosted outside a main push", file, name, ro))
		}
	}
	return out
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

// gitleaksScopeProblems pins hk 3145: off a pull request gitleaks-action runs
// `git log --all` over a fetch-depth 0 checkout, so every other branch and
// mergewake review ref in it is scanned too, and an old false positive there
// fails an unrelated run. The test job must delete every ref but HEAD, off
// pull requests only, in a step before the gitleaks step.
func gitleaksScopeProblems(wf tierWorkflow) []string {
	steps := wf.Jobs["test"].Steps
	scan, prune := -1, -1
	for i, s := range steps {
		if hasAction(s.Uses, "gitleaks/gitleaks-action") {
			scan = i
		}
		if strings.Contains(s.Run, "git update-ref") && strings.Contains(s.Run, "for-each-ref") && s.If == "${{ "+postGuard+" }}" {
			prune = i
		}
	}
	switch {
	case scan < 0:
		return []string{"test job must run gitleaks-action"}
	case prune < 0:
		return []string{"test job must delete every ref but HEAD, if " + postGuard + ", before gitleaks (hk 3145)"}
	case prune > scan:
		return []string{"test job: the ref-pruning step must come before the gitleaks step (hk 3145)"}
	}
	return nil
}

// prStoreDB is the one PR-tier job that starts PostgreSQL (hk 3193): it tests
// ./internal/store/... when a pull request changes internal/store or
// internal/testdb. It has no event guard, so it always reports a conclusion
// and can be a required check; only the suite selector may skip it.
const (
	prStoreDB      = "pr-store-db"
	prStoreDBName  = "pr: store + postgres"
	prStoreDBSuite = "${{ contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}"
)

// prStoreDBProblems checks the pr-store-db job in wf and its entry in the
// mergewake policy's required_checks.
func prStoreDBProblems(wf tierWorkflow, policy []byte) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf("pr-store-db: "+f, a...)) }
	j, ok := wf.Jobs[prStoreDB]
	if !ok {
		return []string{"ci.yml must have a pr-store-db job"}
	}
	if j.Name != prStoreDBName {
		add("name is %q, want %q", j.Name, prStoreDBName)
	}
	// (a) No event guard: only the suite selector may skip the job.
	if j.If != prStoreDBSuite {
		add("job-level if is %q, want only the suite selector %q", j.If, prStoreDBSuite)
	}
	if ro := runsOnText(j.RunsOn); ro != "ubuntu-latest" {
		add("runs-on is %q, want ubuntu-latest", ro)
	}
	// (b) PostgreSQL service, with the DB URL and without the no-DB switch.
	if _, ok := j.Services["postgres"]; !ok {
		add("needs a postgres service")
	}
	if _, ok := j.Env["HANDOFFKEEP_TEST_NO_DB"]; ok {
		add("job sets HANDOFFKEEP_TEST_NO_DB")
	}
	var testStep, decideStep *step
	for i, s := range j.Steps {
		if _, ok := s.Env["HANDOFFKEEP_TEST_NO_DB"]; ok {
			add("step %d sets HANDOFFKEEP_TEST_NO_DB", i)
		}
		if strings.Contains(s.Run, "go test") {
			testStep = &j.Steps[i]
		}
		if strings.Contains(s.Run, "GITHUB_OUTPUT") {
			decideStep = &j.Steps[i]
		}
		// (e) Event data reaches the shell only through env:.
		if strings.Contains(s.Run, "${{") {
			add("step %d: run contains an expression; pass it through env:", i)
		}
	}
	if testStep == nil {
		add("needs a step that runs go test")
	} else {
		if fmt.Sprint(testStep.Env["HANDOFFKEEP_TEST_DB_URL"]) == "" || testStep.Env["HANDOFFKEEP_TEST_DB_URL"] == nil {
			add("the go test step must set HANDOFFKEEP_TEST_DB_URL")
		}
		// (c) Store package only, no -race.
		if !strings.Contains(testStep.Run, "./internal/store/...") {
			add("go test must name ./internal/store/...")
		}
		if strings.Contains(testStep.Run, "-race") {
			add("go test must not use -race")
		}
	}
	// (d) The decision step matches both directories.
	if decideStep == nil {
		add("needs a decision step that writes to GITHUB_OUTPUT")
	} else {
		for _, pat := range []string{"internal/store/*", "internal/testdb/*"} {
			if !strings.Contains(decideStep.Run, pat) {
				add("decision step must match %s", pat)
			}
		}
	}
	// (g) The policy requires the check by its name.
	var pol struct {
		RequiredChecks []string `yaml:"required_checks"`
	}
	if err := yaml.Unmarshal(policy, &pol); err != nil {
		add("policy.yaml: %v", err)
	} else if !slices.Contains(pol.RequiredChecks, prStoreDBName) {
		add("policy.yaml required_checks must include %q", prStoreDBName)
	}
	return out
}

// tierProblems checks ci.yml and ci-any-ref.yml against the tier contract,
// and ci.yml's pr-store-db job against the mergewake policy.
func tierProblems(ci, anyRef, policy []byte) []string {
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
	// The group is keyed on the ref. Only a pull request cancels its older
	// run; a main push lets the running T-POST finish (hk 3103), so main
	// always gets a finished ci-ok even when merges outpace CI.
	if wf.Concurrency["cancel-in-progress"] != "${{ github.event_name == 'pull_request' }}" || !strings.Contains(fmt.Sprint(wf.Concurrency["group"]), "github.ref") {
		add("ci.yml concurrency must key on github.ref and cancel in progress only for pull_request")
	}

	out = append(out, gitleaksScopeProblems(wf)...)
	out = append(out, prStoreDBProblems(wf, policy)...)

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
		if len(j.Services) > 0 && name != prStoreDB {
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
		if !strings.Contains(ro, "("+mainPush+" && vars.HANDOFFKEEP_POST_") || !strings.Contains(ro, ") || '") {
			add("job %s: runs-on %q must read a HANDOFFKEEP_POST_* variable on a main push only, with a hosted fallback", name, ro)
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
	// pull_request; an empty packages input retains the full suite for bisection.
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
	out = append(out, packageProblems(wf, ar)...)
	return out
}

func packageProblems(ci, anyRef tierWorkflow) []string {
	var out []string
	for _, item := range []struct {
		name, event string
		wf          tierWorkflow
	}{
		{"ci.yml", "workflow_call", ci}, {"ci-any-ref.yml", "workflow_dispatch", anyRef},
	} {
		event, _ := item.wf.On[item.event].(map[string]any)
		inputs, _ := event["inputs"].(map[string]any)
		input, _ := inputs["packages"].(map[string]any)
		if input == nil || input["type"] != "string" || input["default"] != "" {
			out = append(out, item.name+": packages must be a string with empty default")
		}
		for job, spec := range item.wf.Jobs {
			for _, step := range spec.Steps {
				if strings.Contains(step.Run, "inputs.packages") {
					out = append(out, item.name+" job "+job+": packages expression in run")
				}
			}
		}
	}
	full := false
	for _, step := range ci.Jobs["post-linux"].Steps {
		if strings.TrimSpace(step.Run) == "go test -v -race -timeout 30m ./..." && step.If == "${{ inputs.packages == '' }}" {
			full = true
		}
	}
	if !full {
		out = append(out, "post-linux must run the full race suite when packages is empty")
	}
	for _, job := range []string{"ci", "status"} {
		if !strings.Contains(fmt.Sprint(anyRef.Jobs[job].Concurrency["group"]), "inputs.packages") {
			out = append(out, "ci-any-ref job "+job+": concurrency must include packages")
		}
	}
	status := false
	for _, step := range anyRef.Jobs["status"].Steps {
		if strings.Contains(step.Run, `if [[ -n "$PACKAGES" ]]; then CONTEXT=ci-ok-test-packages;`) {
			status = true
		}
	}
	if !status {
		out = append(out, "packages status must use ci-ok-test-packages")
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

func readPolicy(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".mergewake", "policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCITiers(t *testing.T) {
	for _, p := range tierProblems(readWorkflow(t, "ci.yml"), readWorkflow(t, "ci-any-ref.yml"), readPolicy(t)) {
		t.Error(p)
	}
}

// No job a pull request reaches, in any workflow, may name a runner variable
// or a self-hosted label, and no job anywhere may reach one outside a main
// push.
func TestPullRequestsStayOnHostedRunners(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "workflows")
	for _, name := range workflowFiles(t, dir) {
		var wf tierWorkflow
		if err := yaml.Unmarshal(readWorkflow(t, name), &wf); err != nil {
			t.Fatal(err)
		}
		for job, spec := range wf.Jobs {
			for _, step := range spec.Steps {
				if strings.Contains(step.Run, "inputs.packages") {
					t.Errorf("%s job %s: packages expression in run", name, job)
				}
			}
		}
		for _, p := range append(prRunnerProblems(name, wf), selfHostedProblems(name, wf)...) {
			t.Error(p)
		}
	}
}

// Each mutant breaks one rule; the checks must catch every one.
func TestCITierMutantsTurnRed(t *testing.T) {
	ci := string(readWorkflow(t, "ci.yml"))
	anyRef := readWorkflow(t, "ci-any-ref.yml")
	policy := readPolicy(t)
	const storeIf = "    if: ${{ contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}\n    name: 'pr: store + postgres'"
	const storeEnv = "        env:\n          HANDOFFKEEP_TEST_DB_URL: postgres://handoffkeep:handoffkeep@localhost:5432/handoffkeep?sslmode=disable\n        run: go test -timeout 10m -count=1 ./internal/store/..."
	for _, m := range []struct{ name, old, new string }{
		{"race-in-pr-test", "run: go test -timeout 10m ./...", "run: go test -race -timeout 10m ./..."},
		{"self-hosted-pr-test", "'all') }}\n    runs-on: ubuntu-latest\n    timeout-minutes: 15\n", "'all') }}\n    runs-on: [self-hosted, linux]\n    timeout-minutes: 15\n"},
		{"runner-variable-pr-test", "'all') }}\n    runs-on: ubuntu-latest\n    timeout-minutes: 15\n", "'all') }}\n    runs-on: ${{ vars.HANDOFFKEEP_POST_LINUX_RUNNER || 'ubuntu-latest' }}\n    timeout-minutes: 15\n"},
		{"no-concurrency", "concurrency: {group:", "x-concurrency: {group:"},
		{"no-cancel-in-progress", `cancel-in-progress: "${{ github.event_name == 'pull_request' }}"}`, "cancel-in-progress: false}"},
		{"main-push-cancels", `cancel-in-progress: "${{ github.event_name == 'pull_request' }}"}`, "cancel-in-progress: true}"},
		{"post-linux-unguarded", "if: ${{ github.event_name != 'pull_request' && contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}\n    name: 'post: race + postgres'", "if: ${{ contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}\n    name: 'post: race + postgres'"},
		{"ci-ok-always", "if: ${{ " + ciOkIf + " }}", "if: ${{ always() }}"},
		{"ci-ok-ignores-abandoned", "if: ${{ " + ciOkIf + " }}", "if: ${{ " + ciOkIfCancelledOnly + " }}"},
		{"no-macos", "vars.HANDOFFKEEP_POST_MAC_RUNNER) || 'macos-latest'", "vars.HANDOFFKEEP_POST_MAC_RUNNER) || 'ubuntu-latest'"},
		{"self-hosted-on-dispatch", "runs-on: ${{ (" + mainPush + " && vars.HANDOFFKEEP_POST_LINUX_RUNNER) || 'ubuntu-latest' }}", "runs-on: ${{ vars.HANDOFFKEEP_POST_LINUX_RUNNER || 'ubuntu-latest' }}"},
		{"no-ref-pruning", "          git for-each-ref --format='delete %(refname)' | git update-ref --no-deref --stdin\n", "          true\n"},
		{"prune-on-pull-request-too", "        if: ${{ github.event_name != 'pull_request' }}\n        run: |\n          git checkout --detach", "        run: |\n          git checkout --detach"},
		{"pr-store-db-pr-only-if", storeIf, "    if: ${{ github.event_name == 'pull_request' && contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}\n    name: 'pr: store + postgres'"},
		{"pr-store-db-no-db-url", storeEnv, "        run: go test -timeout 10m -count=1 ./internal/store/..."},
		{"pr-store-db-race", "run: go test -timeout 10m -count=1 ./internal/store/...", "run: go test -race -timeout 10m -count=1 ./internal/store/..."},
		{"pr-store-db-drops-testdb-path", "internal/store/*|internal/testdb/*)", "internal/store/*)"},
		{"pr-store-db-expr-in-run", "run: go test -timeout 10m -count=1 ./internal/store/...", "run: go test -timeout 10m -count=1 ./internal/store/... -run '${{ github.head_ref }}'"},
		{"pr-store-db-not-in-ci-ok", "needs: [test, pr-store-db, vitest,", "needs: [test, vitest,"},
	} {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(ci, m.old) != 1 {
				t.Fatalf("mutant anchor %q matches %d times", m.old, strings.Count(ci, m.old))
			}
			mutant := strings.Replace(ci, m.old, m.new, 1)
			problems := tierProblems([]byte(mutant), anyRef, policy)
			var wf tierWorkflow
			if err := yaml.Unmarshal([]byte(mutant), &wf); err == nil {
				problems = append(problems, prRunnerProblems("ci.yml", wf)...)
				problems = append(problems, selfHostedProblems("ci.yml", wf)...)
			}
			if len(problems) == 0 {
				t.Fatal("mutant passed the tier checks")
			}
		})
	}
	for _, m := range []struct {
		name, old, new string
		targetCI       bool
	}{
		{"packages-in-run", "          go test -v -race -timeout 30m $PACKAGES", "          go test -v -race -timeout 30m ${{ inputs.packages }}", true},
		{"packages-posts-ci-ok-test", "then CONTEXT=ci-ok-test-packages;", "then CONTEXT=ci-ok-test;", false},
		{"packages-concurrency-shared", "-${{ inputs.packages }}", "", false},
		{"packages-skips-full-suite", "        if: ${{ inputs.packages == '' }}\n        run: go test -v -race -timeout 30m ./...", "        if: ${{ inputs.packages != '' }}\n        run: go test -v -race -timeout 30m ./...", true},
		{"packages-default-nonempty", "        default: ''\n        type: string\n# Two tiers", "        default: './internal/store'\n        type: string\n# Two tiers", true},
	} {
		t.Run(m.name, func(t *testing.T) {
			a, b := ci, string(anyRef)
			target := &b
			if m.targetCI {
				target = &a
			}
			if !strings.Contains(*target, m.old) {
				t.Fatalf("mutant anchor %q missing", m.old)
			}
			*target = strings.Replace(*target, m.old, m.new, 1)
			if len(tierProblems([]byte(a), []byte(b), readPolicy(t))) == 0 {
				t.Fatal("mutant passed tier checks")
			}
		})
	}

	// The policy must list the check by its name.
	t.Run("pr-store-db-not-required", func(t *testing.T) {
		old := "  - 'pr: store + postgres'\n"
		if strings.Count(string(policy), old) != 1 {
			t.Fatalf("mutant anchor %q matches %d times", old, strings.Count(string(policy), old))
		}
		mutant := strings.Replace(string(policy), old, "", 1)
		if len(tierProblems([]byte(ci), anyRef, []byte(mutant))) == 0 {
			t.Fatal("mutant passed the tier checks")
		}
	})
}

// The pruning step must come before gitleaks: moved behind it, the scan has
// already read every ref.
func TestCITierMutantPruneAfterGitleaksTurnsRed(t *testing.T) {
	ci := string(readWorkflow(t, "ci.yml"))
	start := strings.Index(ci, "      # Scan only the history reachable")
	scan := strings.Index(ci, "      - uses: gitleaks/gitleaks-action@")
	end := strings.Index(ci, "  vitest:\n")
	if start < 0 || scan < start || end < scan {
		t.Fatalf("mutant anchors out of order: %d %d %d", start, scan, end)
	}
	mutant := ci[:start] + ci[scan:end] + ci[start:scan] + ci[end:]
	if len(tierProblems([]byte(mutant), readWorkflow(t, "ci-any-ref.yml"), readPolicy(t))) == 0 {
		t.Fatal("mutant passed the tier checks")
	}
}

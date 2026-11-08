package ciworkflow_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// CI tiers (hk 2935, hk 3440,
// hk:doc decision/2026-11-07/fleet-ci-three-tiers-r2).
//
//	pr      a pull request: light jobs, hosted runners, no -race, PostgreSQL
//	        only in pr-store-db. The merge gate.
//	main    every other event: the light jobs plus main-linux, the whole Go
//	        suite against PostgreSQL WITHOUT -race, the migration smoke, the
//	        mutant suites and bundle freshness. Target under 10 minutes. Its
//	        aggregate is ci-ok.
//	deploy  suite=deploy only, off a pull request: deploy-linux (-race with
//	        PostgreSQL), deploy-macos (-race without a database) and
//	        deploy-release (the release-build dry run). deploy.yml starts it
//	        from a green main run and publishes deploy-ok.
//
// These checks keep the PR tier light, keep PR code off self-hosted runners
// (the repository is public), keep -race/macOS/release out of the main tier,
// and keep the deploy tier off our machines.

// postGuard is the event half of every non-PR tier guard.
const postGuard = "github.event_name != 'pull_request'"

// mainGuard selects the main tier: every non-PR event, for the all/test suites.
const mainGuard = "github.event_name != 'pull_request' && contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all')"

// deployGuard selects the deploy tier: a suite=deploy dispatch or a call, never
// a pull request.
const deployGuard = "github.event_name != 'pull_request' && inputs.suite == 'deploy'"

// mainPush gates every self-hosted runner variable: only reviewed, merged code
// on a main push may reach one. A dispatch checks out an arbitrary ref, so it
// stays hosted.
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
	With        map[string]any `yaml:"with"`
	Steps       []step         `yaml:"steps"`
	Concurrency map[string]any `yaml:"concurrency"`
}

type tierWorkflow struct {
	On          map[string]any     `yaml:"on"`
	Concurrency map[string]any     `yaml:"concurrency"`
	Permissions map[string]any     `yaml:"permissions"`
	Jobs        map[string]tierJob `yaml:"jobs"`
}

// unwrap strips the ${{ }} around a job-level if expression.
func unwrap(cond string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(cond), "${{"), "}}"))
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

func stepRuns(j tierJob) string {
	var b strings.Builder
	for _, s := range j.Steps {
		b.WriteString(s.Run)
		b.WriteString("\n")
	}
	return b.String()
}

// tierOf classifies a ci.yml job: "deploy", "main" (non-PR only) or "pr".
// A PR-tier job may also run on other events; it is the light half either way.
func tierOf(name string, j tierJob) string {
	switch {
	case strings.Contains(j.If, deployGuard):
		return "deploy"
	case name == prStoreDB:
		return "pr"
	case strings.Contains(j.If, postGuard):
		return "main"
	}
	return "pr"
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

// prStoreDB is the one PR-tier job that starts PostgreSQL (hk 3193): on a pull
// request it tests every changed Go package with DB-backed tests, and
// ./internal/store/... when internal/store or internal/testdb changed
// (hk 3528). It has no event guard, so it always reports a conclusion
// and can be a required check; only the suite selector may skip it.
const (
	prStoreDB      = "pr-store-db"
	prStoreDBName  = "pr: store + postgres"
	prStoreDBSuite = "${{ contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}"
	// prStoreDBPackages feeds the decision step's selection to go test.
	prStoreDBPackages = "${{ steps.decide.outputs.packages }}"
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
		// (c) Exactly the packages the decision step selected, no -race.
		if testStep.Env["PACKAGES"] != prStoreDBPackages {
			add("the go test step must take PACKAGES from %s", prStoreDBPackages)
		}
		if !strings.Contains(testStep.Run, "$PACKAGES") || strings.Contains(testStep.Run, "./") {
			add("go test must run the selected $PACKAGES and name no package itself")
		}
		if strings.Contains(testStep.Run, "-race") {
			add("go test must not use -race")
		}
	}
	// (d) The decision step matches both directories, keeps the store
	// pattern and finds DB-backed packages by their internal/testdb import.
	if decideStep == nil {
		add("needs a decision step that writes to GITHUB_OUTPUT")
	} else {
		for _, pat := range []string{"internal/store/*", "internal/testdb/*", "./internal/store/...", `/internal/testdb"'`, "packages="} {
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

// mainTierProblems checks main-linux: the non-race PostgreSQL suite, the
// migration smoke, the mutants and the release-build dry run run there, and
// neither -race nor macOS do. The release build stays in the main tier for the
// transition (hk 3440): release.yml gates on ci-ok, so while that holds a
// green ci-ok must keep proving the release assets build.
func mainTierProblems(wf tierWorkflow) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf("main tier: "+f, a...)) }
	var mainJobs []string
	for name, j := range wf.Jobs {
		if tierOf(name, j) == "main" {
			mainJobs = append(mainJobs, name)
		}
	}
	sort.Strings(mainJobs)
	if !slices.Equal(mainJobs, []string{"main-linux"}) {
		return []string{fmt.Sprintf("main tier must be exactly [main-linux], got %v", mainJobs)}
	}
	j := wf.Jobs["main-linux"]
	if j.Name != "main: postgres" {
		add("main-linux name is %q, want %q", j.Name, "main: postgres")
	}
	if unwrap(j.If) != mainGuard {
		add("main-linux if is %q, want %q", j.If, mainGuard)
	}
	if _, ok := j.Services["postgres"]; !ok {
		add("main-linux needs a postgres service")
	}
	if fmt.Sprint(j.Env["HANDOFFKEEP_TEST_DB_URL"]) == "" {
		add("main-linux must set HANDOFFKEEP_TEST_DB_URL")
	}
	if hasRace(j) {
		add("main-linux must not run -race (deploy tier owns it)")
	}
	ro := runsOnText(j.RunsOn)
	if !strings.Contains(ro, "("+mainPush+" && vars.HANDOFFKEEP_POST_LINUX_RUNNER)") || !strings.Contains(ro, ") || '") {
		add("main-linux runs-on %q must read HANDOFFKEEP_POST_LINUX_RUNNER on a main push only, with a hosted fallback", ro)
	}
	runs := stepRuns(j)
	for _, want := range []struct{ needle, what string }{
		{"scripts/test-task-expected-migration.sh", "the migration and startup smoke"},
		{"scripts/task-reopen-mutants.py", "the reopen mutants"},
		{"scripts/quota-mutants.py", "the quota mutants"},
		{"scripts/test-build-release.sh", "the release-build dry run (release still gates on ci-ok)"},
	} {
		if !strings.Contains(runs, want.needle) {
			add("main-linux must run %s", want.what)
		}
	}
	full := false
	for _, s := range j.Steps {
		if strings.TrimSpace(s.Run) == "go test -v -timeout 30m ./..." && s.If == "${{ inputs.packages == '' }}" {
			full = true
		}
	}
	if !full {
		add("main-linux must run the full suite without -race when packages is empty")
	}
	return out
}

// deployTierProblems checks the deploy tier in ci.yml: exactly the three
// deploy-* jobs, all hosted, the heavy checks among them, and none of them
// reachable from a pull request.
func deployTierProblems(wf tierWorkflow) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf("deploy tier: "+f, a...)) }
	var names []string
	for name, j := range wf.Jobs {
		if tierOf(name, j) != "deploy" {
			continue
		}
		names = append(names, name)
		if !strings.HasPrefix(name, "deploy-") {
			add("job %s: deploy-tier jobs are named deploy-*", name)
		}
		ro := runsOnText(j.RunsOn)
		if strings.Contains(ro, "vars.") || strings.Contains(ro, "self-hosted") {
			add("job %s: runs-on %q must be hosted; a dispatch can name any sha", name, ro)
		}
	}
	sort.Strings(names)
	if !slices.Equal(names, []string{"deploy-linux", "deploy-macos", "deploy-release"}) {
		return append(out, fmt.Sprintf("deploy tier must be exactly [deploy-linux deploy-macos deploy-release], got %v", names))
	}

	linux := wf.Jobs["deploy-linux"]
	if unwrap(linux.If) != deployGuard {
		add("deploy-linux if is %q, want %q", linux.If, deployGuard)
	}
	if _, ok := linux.Services["postgres"]; !ok {
		add("deploy-linux needs a postgres service")
	}
	if fmt.Sprint(linux.Env["HANDOFFKEEP_TEST_DB_URL"]) == "" {
		add("deploy-linux must set HANDOFFKEEP_TEST_DB_URL")
	}
	if !strings.Contains(stepRuns(linux), "go test -v -race -timeout 30m ./...") {
		add("deploy-linux must run the full -race suite against PostgreSQL")
	}

	macos := wf.Jobs["deploy-macos"]
	if unwrap(macos.If) != deployGuard {
		add("deploy-macos if is %q, want %q", macos.If, deployGuard)
	}
	if ro := runsOnText(macos.RunsOn); !strings.Contains(ro, "macos-") {
		add("deploy-macos runs-on is %q, want a macOS runner", ro)
	}
	if !hasRace(macos) {
		add("deploy-macos must run -race")
	}
	if _, ok := macos.Env["HANDOFFKEEP_TEST_NO_DB"]; !ok {
		add("deploy-macos must skip DB-backed tests (no service containers on macOS)")
	}

	rel := wf.Jobs["deploy-release"]
	if unwrap(rel.If) != deployGuard {
		add("deploy-release if is %q, want %q", rel.If, deployGuard)
	}
	if !strings.Contains(stepRuns(rel), "scripts/test-build-release.sh") {
		add("deploy-release must run the release-build dry run")
	}
	return out
}

// deployWorkflowProblems checks deploy.yml: what starts it, that it never
// cancels, its exact permissions, and that its one status writer publishes
// deploy-ok on the resolved target sha.
func deployWorkflowProblems(raw []byte) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf("deploy.yml: "+f, a...)) }
	var wf tierWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return []string{fmt.Sprintf("deploy.yml: %v", err)}
	}
	run, ok := wf.On["workflow_run"].(map[string]any)
	if !ok {
		add("must trigger on workflow_run")
	} else {
		if got := fmt.Sprint(run["workflows"]); got != "[ci]" {
			add("workflow_run.workflows is %q, want [ci]", got)
		}
		if got := fmt.Sprint(run["types"]); got != "[completed]" {
			add("workflow_run.types is %q, want [completed]", got)
		}
		if got := fmt.Sprint(run["branches"]); got != "[main]" {
			add("workflow_run.branches is %q, want [main]", got)
		}
	}
	if _, ok := wf.On["workflow_dispatch"]; !ok {
		add("must trigger on workflow_dispatch(sha)")
	}
	if fmt.Sprint(wf.Concurrency["cancel-in-progress"]) != "false" {
		add("concurrency.cancel-in-progress is %q, want false", wf.Concurrency["cancel-in-progress"])
	}
	if g := fmt.Sprint(wf.Concurrency["group"]); g != "deploy" {
		add("concurrency.group is %q, want one group per repository (\"deploy\")", g)
	}
	want := map[string]string{"contents": "read", "statuses": "write"}
	for k, v := range want {
		if fmt.Sprint(wf.Permissions[k]) != v {
			add("permissions.%s is %q, want %q", k, wf.Permissions[k], v)
		}
	}
	for k, v := range wf.Permissions {
		if _, ok := want[k]; !ok {
			add("permissions.%s=%v is wider than the deploy tier needs", k, v)
		}
	}

	call, ok := wf.Jobs["ci"]
	switch {
	case !ok:
		add("must call ci.yml from a ci job")
	case call.Uses != "./.github/workflows/ci.yml":
		add("ci job uses %q, want ./.github/workflows/ci.yml", call.Uses)
	case fmt.Sprint(call.With["suite"]) != "deploy":
		add("ci call must pass suite: deploy, got %q", call.With["suite"])
	}

	compat, ok := wf.Jobs["schema-compat"]
	if !ok {
		add("must have a schema-compat job (previous release on the target schema)")
	} else {
		if got := fmt.Sprint(needsList(compat.Needs)); got != "[target]" {
			add("schema-compat must need only target, got %s", got)
		}
		if !strings.Contains(stepRuns(compat), "scripts/test-previous-release-schema.sh") {
			add("schema-compat must run scripts/test-previous-release-schema.sh")
		}
		if v, ok := compat.Env["HANDOFFKEEP_TEST_DB_URL"]; !ok || fmt.Sprint(v) == "" {
			add("schema-compat must set HANDOFFKEEP_TEST_DB_URL")
		}
		if _, ok := compat.Services["postgres"]; !ok {
			add("schema-compat needs a postgres service")
		}
		if ro := runsOnText(compat.RunsOn); ro != "ubuntu-latest" {
			add("schema-compat runs-on is %q, want hosted ubuntu-latest", ro)
		}
		// The checkout must pin the workflow revision itself: a dispatch or a
		// re-run can name an old ref that predates the script, and the job has
		// to keep running (reviews on #237). github.workflow_sha is the commit
		// of the workflow file. The target reaches the script as TARGET_SHA,
		// which it builds from that commit.
		fullHistory, pinned := false, false
		for _, s := range compat.Steps {
			if !hasAction(s.Uses, "actions/checkout") {
				continue
			}
			if v, _ := s.With["ref"].(string); v != "" {
				if v != "${{ github.workflow_sha }}" {
					add("schema-compat checkout ref is %q, want ${{ github.workflow_sha }} so an old dispatch ref cannot drop the script", v)
				} else {
					pinned = true
				}
			}
			if fmt.Sprint(s.With["fetch-depth"]) == "0" {
				fullHistory = true
			}
		}
		if !pinned {
			add("schema-compat checkout must pin ref: ${{ github.workflow_sha }}")
		}
		if !fullHistory {
			add("schema-compat checkout needs fetch-depth: 0 so the target and base commits resolve")
		}
		// The script step itself must carry the target, and a step before it
		// must fetch the requested commits: fetch-depth 0 covers branch and
		// tag refs, not a commit no ref points at (review on #237).
		scriptTarget, fetched := false, false
		for _, s := range compat.Steps {
			if strings.Contains(s.Run, "scripts/test-previous-release-schema.sh") {
				scriptTarget = fmt.Sprint(s.Env["TARGET_SHA"]) == "${{ needs.target.outputs.sha }}"
			}
			if strings.Contains(s.Run, `git fetch --no-tags origin "$TARGET_SHA"`) {
				fetched = true
			}
		}
		if !scriptTarget {
			add("the schema-compat script step must pass TARGET_SHA=${{ needs.target.outputs.sha }}")
		}
		if !fetched {
			add("schema-compat must fetch TARGET_SHA explicitly (fetch-depth 0 does not cover an unreferenced commit)")
		}
	}

	status, ok := wf.Jobs["status"]
	if !ok {
		add("must have a status job")
		return out
	}
	if !strings.Contains(status.If, "always()") {
		add("the status job must run with always() so a failed tier reports failure")
	}
	needs := fmt.Sprint(needsList(status.Needs))
	if !strings.Contains(needs, "target") || !strings.Contains(needs, "ci") || !strings.Contains(needs, "schema-compat") {
		add("the status job must need target, ci and schema-compat, got %s", needs)
	}
	runs := stepRuns(status)
	if strings.Count(runs, "context=deploy-ok") != 1 {
		add("the status job must be the one writer of context=deploy-ok")
	}
	if strings.Contains(runs, "context=\"$CONTEXT\"") || strings.Contains(runs, "context=ci-ok") {
		add("the status job must publish only deploy-ok")
	}
	if !strings.Contains(runs, "statuses/${TARGET_SHA}") {
		add("the status must be posted to statuses/<target sha>")
	}
	if got := fmt.Sprint(status.Env["TARGET_SHA"]); got != "${{ needs.target.outputs.sha }}" {
		add("TARGET_SHA is %q, want the resolved target %q", got, "${{ needs.target.outputs.sha }}")
	}
	return out
}

// tierProblems checks ci.yml and ci-any-ref.yml against the tier contract,
// deploy.yml against the deploy-tier contract, and ci.yml's pr-store-db job
// against the mergewake policy.
func tierProblems(ci, anyRef, deploy, policy []byte) []string {
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
	// The group is keyed on the ref and a newer push cancels the older run,
	// on a pull request and on main (operator 2026-11-06): the newest main
	// head ships, and a failure there is bisected and reverted by watch.
	if fmt.Sprint(wf.Concurrency["cancel-in-progress"]) != "true" || !strings.Contains(fmt.Sprint(wf.Concurrency["group"]), "github.ref") {
		add("ci.yml concurrency must key on github.ref and cancel the older run in progress")
	}

	out = append(out, gitleaksScopeProblems(wf)...)
	out = append(out, prStoreDBProblems(wf, policy)...)
	out = append(out, mainTierProblems(wf)...)
	out = append(out, deployTierProblems(wf)...)
	out = append(out, deployWorkflowProblems(deploy)...)

	for name, j := range wf.Jobs {
		if tierOf(name, j) != "pr" {
			continue
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
		for name := range wf.Jobs {
			if name != "ci-ok" && !needs[name] {
				add("ci-ok must need %s", name)
			}
		}
	}

	var ar tierWorkflow
	if err := yaml.Unmarshal(anyRef, &ar); err != nil {
		return append(out, err.Error())
	}
	// A dispatch reaches the main tier only because its event is not
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
	// suite=deploy is a diagnostic: only deploy.yml may publish deploy-ok.
	if strings.Contains(string(anyRef), "deploy-ok") {
		add("ci-any-ref.yml must not publish deploy-ok")
	}
	if !strings.Contains(string(anyRef), "\n          - deploy\n") {
		add("ci-any-ref.yml suite options must include deploy")
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

func allWorkflows(t *testing.T) []byte {
	t.Helper()
	return readWorkflow(t, "ci.yml")
}

func TestCITiers(t *testing.T) {
	for _, p := range tierProblems(allWorkflows(t), readWorkflow(t, "ci-any-ref.yml"), readWorkflow(t, "deploy.yml"), readPolicy(t)) {
		t.Error(p)
	}
}

// TestCITierMatrix pins which jobs run for each event and suite. It evaluates
// the real job-level if: expressions, so a job that silently moves between
// tiers (or out of all of them) fails here, not in production. ci-ok is judged
// by TestCIOkAggregate instead: its guard is about needs results.
func TestCITierMatrix(t *testing.T) {
	var wf tierWorkflow
	if err := yaml.Unmarshal(allWorkflows(t), &wf); err != nil {
		t.Fatal(err)
	}
	light := []string{"auto-tag-test", "build", "pr-store-db", "test", "vitest"}
	// suite=test drops the vitest and build jobs (their own suite selectors).
	testOnly := []string{"auto-tag-test", "pr-store-db", "test"}
	mainTier := append(append([]string{}, light...), "main-linux")
	mainTest := append(append([]string{}, testOnly...), "main-linux")
	deploy := []string{"deploy-linux", "deploy-macos", "deploy-release"}
	for _, c := range []struct {
		name, event, suite string
		want               []string
	}{
		{"pull-request", "pull_request", "all", light},
		{"pull-request-test", "pull_request", "test", testOnly},
		{"main-push", "push", "all", mainTier},
		{"main-push-test", "push", "test", mainTest},
		{"dispatch-all", "workflow_dispatch", "all", mainTier},
		{"any-ref-deploy", "workflow_dispatch", "deploy", deploy},
		{"deploy-workflow-run", "workflow_run", "deploy", deploy},
		{"deploy-called", "workflow_call", "deploy", deploy},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for name, j := range wf.Jobs {
				if name == "ci-ok" {
					continue
				}
				cond := unwrap(j.If)
				if cond == "" {
					got = append(got, name)
					continue
				}
				run, err := evalGuard(cond, c.event, c.suite)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if run {
					got = append(got, name)
				}
			}
			sort.Strings(got)
			want := append([]string{}, c.want...)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Fatalf("%s/%s runs %v, want %v", c.event, c.suite, got, want)
			}
		})
	}
}

// evalGuard evaluates the subset of the Actions expression language the job
// guards use, with github.event_name and inputs.suite substituted.
func evalGuard(expr, event, suite string) (bool, error) {
	atoms := []struct {
		text string
		val  bool
	}{
		{"github.event_name != 'pull_request'", event != "pull_request"},
		{"github.ref == 'refs/heads/main'", false},
		{"inputs.suite == 'deploy'", suite == "deploy"},
		{"inputs.suite != 'deploy'", suite != "deploy"},
		{`contains(fromJSON('["all","test"]'), inputs.suite || 'all')`, suite == "all" || suite == "test"},
		{`contains(fromJSON('["all","vitest"]'), inputs.suite || 'all')`, suite == "all" || suite == "vitest"},
		{`contains(fromJSON('["all","build"]'), inputs.suite || 'all')`, suite == "all" || suite == "build"},
	}
	for _, a := range atoms {
		expr = strings.ReplaceAll(expr, a.text, fmt.Sprint(a.val))
	}
	p := &boolParser{s: expr}
	v, err := p.or()
	if err != nil {
		return false, err
	}
	p.space()
	if p.i != len(p.s) {
		return false, fmt.Errorf("unsupported guard text at %d in %q", p.i, expr)
	}
	return v, nil
}

type boolParser struct {
	s string
	i int
}

func (p *boolParser) space() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *boolParser) eat(tok string) bool {
	p.space()
	if strings.HasPrefix(p.s[p.i:], tok) {
		p.i += len(tok)
		return true
	}
	return false
}

func (p *boolParser) or() (bool, error) {
	v, err := p.and()
	for err == nil && p.eat("||") {
		var r bool
		r, err = p.and()
		v = v || r
	}
	return v, err
}

func (p *boolParser) and() (bool, error) {
	v, err := p.unary()
	for err == nil && p.eat("&&") {
		var r bool
		r, err = p.unary()
		v = v && r
	}
	return v, err
}

func (p *boolParser) unary() (bool, error) {
	switch {
	case p.eat("!"):
		v, err := p.unary()
		return !v, err
	case p.eat("("):
		v, err := p.or()
		if err == nil && !p.eat(")") {
			err = fmt.Errorf("missing ) at %d in %q", p.i, p.s)
		}
		return v, err
	case p.eat("true"):
		return true, nil
	case p.eat("false"):
		return false, nil
	}
	return false, fmt.Errorf("unsupported expression at %d in %q", p.i, p.s)
}

// TestDeployStatusVerdicts runs deploy.yml's status step against a fake gh:
// success and failure post deploy-ok on the target sha, a cancelled or
// abandoned tier posts nothing (the head reads unknown), and the context is
// never anything else. The previous-release schema check is part of the tier,
// so its result fails deploy-ok too.
func TestDeployStatusVerdicts(t *testing.T) {
	var wf tierWorkflow
	if err := yaml.Unmarshal(readWorkflow(t, "deploy.yml"), &wf); err != nil {
		t.Fatal(err)
	}
	steps := wf.Jobs["status"].Steps
	if len(steps) != 1 {
		t.Fatalf("deploy.yml status job has %d steps, want one", len(steps))
	}
	for _, c := range []struct {
		name, result, compat, state string // state "" means no status call at all
	}{
		{name: "success", result: "success", compat: "success", state: "success"},
		{name: "failure", result: "failure", compat: "success", state: "failure"},
		{name: "cancelled", result: "cancelled", compat: "success", state: ""},
		{name: "abandoned", result: "abandoned", compat: "success", state: ""},
		{name: "schema-compat-failure", result: "success", compat: "failure", state: "failure"},
		{name: "schema-compat-cancelled", result: "success", compat: "cancelled", state: ""},
		{name: "failure-beats-cancelled", result: "failure", compat: "cancelled", state: "failure"},
		// GitHub refuses a description over 140 characters, so an overlong
		// result must still post a short description.
		{name: "overlong-result", result: strings.Repeat("x", 300), compat: "success", state: "failure"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			call := filepath.Join(dir, "call")
			// One argument per line, so the description is read back whole.
			gh := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do printf '%%s\n' "$a"; done > %q
`, call)
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-euo", "pipefail", "-c", steps[0].Run)
			cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "GH_TOKEN=x", "GITHUB_REPOSITORY=o/r",
				"TARGET_SHA=abc123", "RESULT=" + c.result, "SCHEMA_COMPAT=" + c.compat,
				"RUN_URL=https://example/run"}
			if logs, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("status step failed: %v\n%s", err, logs)
			}
			b, err := os.ReadFile(call)
			if c.state == "" {
				if err == nil {
					t.Fatalf("cancelled/abandoned posted a status: %s", b)
				}
				return
			}
			if err != nil {
				t.Fatalf("no status posted for %s: %v", c.name, err)
			}
			got := string(b)
			for _, want := range []string{"statuses/abc123", "state=" + c.state, "context=deploy-ok"} {
				if !strings.Contains(got, want) {
					t.Errorf("status call %q lacks %q", got, want)
				}
			}
			start := strings.Index(got, "description=")
			if start < 0 {
				t.Fatalf("status call %q has no description", got)
			}
			desc := got[start+len("description="):]
			if i := strings.IndexByte(desc, '\n'); i >= 0 {
				desc = desc[:i]
			}
			if len(desc) > 140 {
				t.Errorf("status description is %d characters, GitHub allows 140: %q", len(desc), desc)
			}
		})
	}
}

// TestCITierJobInventory pins every job in each workflow by name: the
// before/after table in the pull request cannot promise that no check
// disappeared unless the inventory itself is checked.
func TestCITierJobInventory(t *testing.T) {
	for _, c := range []struct {
		file string
		want []string
	}{
		{"ci.yml", []string{"auto-tag-test", "build", "ci-ok", "deploy-linux", "deploy-macos", "deploy-release", "main-linux", "pr-store-db", "test", "vitest"}},
		{"deploy.yml", []string{"ci", "schema-compat", "status", "target"}},
	} {
		var wf tierWorkflow
		if err := yaml.Unmarshal(readWorkflow(t, c.file), &wf); err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		var got []string
		for name := range wf.Jobs {
			got = append(got, name)
		}
		sort.Strings(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s jobs = %v, want %v", c.file, got, c.want)
		}
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
			t.Errorf("%s: %v", name, err)
			continue
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
	ci := string(allWorkflows(t))
	anyRef := readWorkflow(t, "ci-any-ref.yml")
	deploy := readWorkflow(t, "deploy.yml")
	policy := readPolicy(t)
	const storeIf = "    if: ${{ contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}\n    name: 'pr: store + postgres'"
	const storeEnv = "        env:\n          HANDOFFKEEP_TEST_DB_URL: postgres://handoffkeep:handoffkeep@localhost:5432/handoffkeep?sslmode=disable\n          PACKAGES: ${{ steps.decide.outputs.packages }}\n"
	for _, m := range []struct{ name, old, new string }{
		{"race-in-pr-test", "run: go test -timeout 10m ./...", "run: go test -race -timeout 10m ./..."},
		{"race-in-main-linux", "run: go test -v -timeout 30m ./...", "run: go test -v -race -timeout 30m ./..."},
		{"main-linux-race-only", "        run: go test -v -timeout 30m ./...\n", "        run: go test -v -race -timeout 30m ./...\n"},
		{"self-hosted-pr-test", "'all') }}\n    runs-on: ubuntu-latest\n    timeout-minutes: 15\n", "'all') }}\n    runs-on: [self-hosted, linux]\n    timeout-minutes: 15\n"},
		{"runner-variable-pr-test", "'all') }}\n    runs-on: ubuntu-latest\n    timeout-minutes: 15\n", "'all') }}\n    runs-on: ${{ vars.HANDOFFKEEP_POST_LINUX_RUNNER || 'ubuntu-latest' }}\n    timeout-minutes: 15\n"},
		{"no-concurrency", "concurrency: {group:", "x-concurrency: {group:"},
		{"no-cancel-in-progress", "cancel-in-progress: true}", "cancel-in-progress: false}"},
		{"main-push-keeps-running", "cancel-in-progress: true}", `cancel-in-progress: "${{ github.event_name == 'pull_request' }}"}`},
		{"main-linux-unguarded", "    if: ${{ " + mainGuard + " }}\n    name: 'main: postgres'", "    if: ${{ contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}\n    name: 'main: postgres'"},
		{"deploy-linux-unguarded", "    if: ${{ " + deployGuard + " }}\n    name: 'deploy: race + postgres'", "    if: ${{ contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}\n    name: 'deploy: race + postgres'"},
		{"deploy-linux-race-removed", "go test -v -race -timeout 30m ./...", "go test -v -timeout 30m ./..."},
		{"deploy-macos-on-linux", "'deploy: macos'\n    runs-on: macos-latest", "'deploy: macos'\n    runs-on: ubuntu-latest"},
		{"deploy-macos-no-db-switch", "      HANDOFFKEEP_TEST_NO_DB: \"1\"\n    steps:\n      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0\n        with:\n          ref: ${{ inputs.ref }}\n      - uses: actions/setup-go", "    steps:\n      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0\n        with:\n          ref: ${{ inputs.ref }}\n      - uses: actions/setup-go"},
		{"deploy-release-dropped", "      - run: bash scripts/test-build-release.sh", "      - run: true"},
		// The main tier keeps the release build during the transition. Two
		// distinct anchors: ci.yml has it in both main-linux and deploy-release.
		{"main-linux-release-dropped", "        run: bash scripts/test-build-release.sh\n", "        run: true\n"},
		{"main-linux-migration-dropped", "        run: bash scripts/test-task-expected-migration.sh", "        run: true"},
		{"main-linux-mutants-dropped", "        run: python3 scripts/task-reopen-mutants.py", "        run: true"},
		{"ci-ok-always", "if: ${{ " + ciOkIf + " }}", "if: ${{ always() }}"},
		{"ci-ok-ignores-abandoned", "if: ${{ " + ciOkIf + " }}", "if: ${{ " + ciOkIfCancelledOnly + " }}"},
		{"self-hosted-on-dispatch", "runs-on: ${{ (" + mainPush + " && vars.HANDOFFKEEP_POST_LINUX_RUNNER) || 'ubuntu-latest' }}", "runs-on: ${{ vars.HANDOFFKEEP_POST_LINUX_RUNNER || 'ubuntu-latest' }}"},
		{"no-ref-pruning", "          git for-each-ref --format='delete %(refname)' | git update-ref --no-deref --stdin\n", "          true\n"},
		{"prune-on-pull-request-too", "        if: ${{ github.event_name != 'pull_request' }}\n        run: |\n          git checkout --detach", "        run: |\n          git checkout --detach"},
		{"pr-store-db-pr-only-if", storeIf, "    if: ${{ github.event_name == 'pull_request' && contains(fromJSON('[\"all\",\"test\"]'), inputs.suite || 'all') }}\n    name: 'pr: store + postgres'"},
		{"pr-store-db-no-db-url", storeEnv, "        env:\n          PACKAGES: ${{ steps.decide.outputs.packages }}\n"},
		{"pr-store-db-race", `go test -timeout 10m -count=1 -v "${pkgs[@]}"`, `go test -race -timeout 10m -count=1 -v "${pkgs[@]}"`},
		{"pr-store-db-drops-testdb-path", "internal/store/*|internal/testdb/*)", "internal/store/*)"},
		{"pr-store-db-expr-in-run", `go test -timeout 10m -count=1 -v "${pkgs[@]}"`, `go test -timeout 10m -count=1 -v "${pkgs[@]}" -run '${{ github.head_ref }}'`},
		{"pr-store-db-ignores-selection", `go test -timeout 10m -count=1 -v "${pkgs[@]}"`, `go test -timeout 10m -count=1 -v ./internal/store/...`},
		{"pr-store-db-packages-not-from-decision", "          PACKAGES: ${{ steps.decide.outputs.packages }}\n", "          PACKAGES: ./internal/store/...\n"},
		{"pr-store-db-not-in-ci-ok", "needs: [test, pr-store-db, vitest,", "needs: [test, vitest,"},
	} {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(ci, m.old) != 1 {
				t.Fatalf("mutant anchor %q matches %d times", m.old, strings.Count(ci, m.old))
			}
			mutant := strings.Replace(ci, m.old, m.new, 1)
			problems := tierProblems([]byte(mutant), anyRef, deploy, policy)
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
		{"packages-in-run", "          go test -v -timeout 30m $PACKAGES", "          go test -v -timeout 30m ${{ inputs.packages }}", true},
		{"packages-posts-ci-ok-test", "then CONTEXT=ci-ok-test-packages;", "then CONTEXT=ci-ok-test;", false},
		{"packages-concurrency-shared", "-${{ inputs.packages }}", "", false},
		{"packages-skips-full-suite", "        if: ${{ inputs.packages == '' }}\n        run: go test -v -timeout 30m ./...", "        if: ${{ inputs.packages != '' }}\n        run: go test -v -timeout 30m ./...", true},
		{"packages-default-nonempty", "        default: ''\n        type: string\n# Three tiers", "        default: './internal/store'\n        type: string\n# Three tiers", true},
		{"deploy-ok-from-any-ref", `-f context="$CONTEXT"`, `-f context="$CONTEXT" -f context=deploy-ok`, false},
		{"deploy-suite-not-an-option", "\n          - deploy\n", "\n", false},
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
			if len(tierProblems([]byte(a), []byte(b), deploy, readPolicy(t))) == 0 {
				t.Fatal("mutant passed tier checks")
			}
		})
	}

	// deploy.yml mutants: each breaks one deploy-tier rule.
	for _, m := range []struct{ name, old, new string }{
		{"deploy-cancels", "cancel-in-progress: false", "cancel-in-progress: true"},
		{"deploy-one-group-per-run", "group: deploy", "group: deploy-${{ github.run_id }}"},
		{"deploy-widens-permissions", "  statuses: write", "  statuses: write\n  contents: write"},
		{"deploy-drops-contents-read", "  contents: read\n  statuses: write", "  statuses: write"},
		{"deploy-no-workflow-run", "  workflow_run:\n    # ci.yml is by name, the only workflow it may follow. branches: [main] drops\n    # every pull request and any-ref call at the trigger, before a run exists;\n    # the target job repeats the main-push test so a manually edited trigger\n    # cannot deploy a PR head.\n    workflows: [ci]\n    types: [completed]\n    branches: [main]\n", ""},
		{"deploy-follows-any-branch", "    branches: [main]\n", "    branches: [main, develop]\n"},
		{"deploy-unpinned-context", "context=deploy-ok", "context=ci-ok"},
		{"deploy-status-on-dispatch-sha", "      TARGET_SHA: ${{ needs.target.outputs.sha }}\n      RESULT:", "      TARGET_SHA: ${{ github.sha }}\n      RESULT:"},
		{"deploy-status-without-always", "if: ${{ always() && needs.target.result == 'success' }}", "if: ${{ needs.target.result == 'success' }}"},
		{"deploy-call-without-suite", "      suite: deploy", "      suite: all"},
		{"deploy-schema-compat-dropped", "            bash scripts/test-previous-release-schema.sh \"$BASE\"\n          else\n            bash scripts/test-previous-release-schema.sh\n", "            true\n          else\n            true\n"},
		{"deploy-schema-compat-not-in-status", "needs: [target, ci, schema-compat]", "needs: [target, ci]"},
		{"deploy-schema-compat-no-db", "      HANDOFFKEEP_TEST_DB_URL: postgres://handoffkeep:handoffkeep@localhost:5432/handoffkeep?sslmode=disable\n    steps:\n      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0\n        with:", "    steps:\n      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0\n        with:"},
		{"deploy-schema-compat-pins-target-ref", "          ref: ${{ github.workflow_sha }}", "          ref: ${{ needs.target.outputs.sha }}"},
		{"deploy-schema-compat-unpinned", "          ref: ${{ github.workflow_sha }}\n", ""},
		{"deploy-schema-compat-no-target-sha", "          # v* tag before the target.\n          TARGET_SHA: ${{ needs.target.outputs.sha }}\n", "          # v* tag before the target.\n"},
		{"deploy-schema-compat-no-fetch", "          git fetch --no-tags origin \"$TARGET_SHA\"\n", ""},
	} {
		t.Run(m.name, func(t *testing.T) {
			src := string(deploy)
			if strings.Count(src, m.old) != 1 {
				t.Fatalf("mutant anchor %q matches %d times", m.old, strings.Count(src, m.old))
			}
			mutant := strings.Replace(src, m.old, m.new, 1)
			if len(tierProblems(allWorkflows(t), anyRef, []byte(mutant), policy)) == 0 {
				t.Fatal("mutant passed the deploy.yml checks")
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
		if len(tierProblems([]byte(ci), anyRef, deploy, []byte(mutant))) == 0 {
			t.Fatal("mutant passed the tier checks")
		}
	})
}

// The pruning step must come before gitleaks: moved behind it, the scan has
// already read every ref.
func TestCITierMutantPruneAfterGitleaksTurnsRed(t *testing.T) {
	ci := string(allWorkflows(t))
	start := strings.Index(ci, "      # Scan only the history reachable")
	scan := strings.Index(ci, "      - uses: gitleaks/gitleaks-action@")
	end := strings.Index(ci, "  vitest:\n")
	if start < 0 || scan < start || end < scan {
		t.Fatalf("mutant anchors out of order: %d %d %d", start, scan, end)
	}
	mutant := ci[:start] + ci[scan:end] + ci[start:scan] + ci[end:]
	if len(tierProblems([]byte(mutant), readWorkflow(t, "ci-any-ref.yml"), readWorkflow(t, "deploy.yml"), readPolicy(t))) == 0 {
		t.Fatal("mutant passed the tier checks")
	}
}

// TestImageCancelsSupersededMainRuns pins the fleet main-CI rule on image.yml
// (policy/fleet-ci-main-cancel): its concurrency is keyed on the ref and a
// newer push cancels the older run; a mutant without cancellation turns red.
func TestImageCancelsSupersededMainRuns(t *testing.T) {
	check := func(src []byte) []string {
		var wf tierWorkflow
		if err := yaml.Unmarshal(src, &wf); err != nil {
			t.Fatal(err)
		}
		var out []string
		if fmt.Sprint(wf.Concurrency["cancel-in-progress"]) != "true" || !strings.Contains(fmt.Sprint(wf.Concurrency["group"]), "github.ref") {
			out = append(out, "image.yml concurrency must key on github.ref and cancel the older run in progress")
		}
		return out
	}
	src := readWorkflow(t, "image.yml")
	if p := check(src); len(p) != 0 {
		t.Fatal(p)
	}
	mutant := strings.Replace(string(src), "cancel-in-progress: true}", "cancel-in-progress: false}", 1)
	if mutant == string(src) {
		t.Fatal("mutant did not apply")
	}
	if len(check([]byte(mutant))) == 0 {
		t.Fatal("image-keeps-running mutant stayed green")
	}
}

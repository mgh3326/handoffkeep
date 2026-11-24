package ciworkflow_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The long suites are sharded (hk 6156). A shard script deals its list
// round-robin, so each shard set is a partition of the list. These tests prove
// the partition for every shard count and that the workflow runs every shard.

// fakeGo answers the two go invocations the shard scripts make, from canned
// lists in the environment, so the partition is checked without compiling:
// `go list ./...` and `go test [-tags=mysql] -list . ./internal/store`.
const fakeGo = `#!/usr/bin/env bash
case "$1" in
list)
  printf '%s\n' $FAKE_PACKAGES
  ;;
test)
  if [ "$2" = -tags=mysql ]; then
    printf '%s\n' $FAKE_STORE_TESTS $FAKE_MYSQL_TESTS
  else
    printf '%s\n' $FAKE_STORE_TESTS
  fi
  echo "ok	example.test/internal/store	0.01s"
  ;;
*)
  echo "unexpected go $*" >&2
  exit 2
  ;;
esac
`

// shardLists returns the canned package and test lists. They mix Test,
// Example and Fuzz names, add a few store tests that exist only under the
// mysql tag, and include nested and store-like package paths.
func shardLists() (pkgs, store, mysql []string) {
	pkgs = []string{
		"example.test/cmd/handoffkeep", "example.test/client", "example.test/internal/api",
		"example.test/internal/store", "example.test/internal/store/sub", "example.test/internal/ui",
		"example.test/internal/guard", "example.test/tests", "example.test/internal/testdb",
		"example.test/internal/hktransport", "example.test/internal/remote", "example.test/internal/ciworkflow",
	}
	for i := 0; i < 137; i++ {
		store = append(store, fmt.Sprintf("TestStore%03d", i))
	}
	store = append(store, "ExampleStoreRows", "FuzzStoreKey")
	mysql = []string{"TestMySQLOnlyDialect", "TestMySQLOnlyDrift", "TestMySQLOnlyJSON"}
	return pkgs, store, mysql
}

// fakeEnv writes the fake go into a fresh directory and returns the
// environment that puts it first on PATH, with the canned lists.
func fakeEnv(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	pkgs, store, mysql := shardLists()
	return []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_PACKAGES=" + strings.Join(pkgs, " "),
		"FAKE_STORE_TESTS=" + strings.Join(store, " "),
		"FAKE_MYSQL_TESTS=" + strings.Join(mysql, " "),
	}
}

// runShard runs one shard script under env and returns its stdout.
func runShard(env []string, script string, args ...string) (string, error) {
	cmd := exec.Command("bash", append([]string{filepath.Join("..", "..", "scripts", script)}, args...)...)
	cmd.Env = env
	out, err := cmd.Output()
	return string(out), err
}

// regexNames returns the test names an anchored shard regex selects.
func regexNames(t *testing.T, regex string) []string {
	t.Helper()
	body, ok := strings.CutPrefix(strings.TrimSpace(regex), "^(")
	if !ok {
		t.Fatalf("shard regex %q is not anchored", regex)
	}
	body, ok = strings.CutSuffix(body, ")$")
	if !ok {
		t.Fatalf("shard regex %q is not anchored", regex)
	}
	return strings.Split(body, "|")
}

// assertPartition fails unless got lists each want entry exactly once.
func assertPartition(t *testing.T, what string, got, want []string) {
	t.Helper()
	sorted := append([]string{}, got...)
	sort.Strings(sorted)
	if !slices.Equal(sorted, want) {
		t.Fatalf("%s: shards cover %d entries, want the %d-entry list exactly once\ngot:  %v\nwant: %v", what, len(got), len(want), sorted, want)
	}
}

// TestCIShardsPartitionTheSuites proves that for every shard count the store
// tests (plain and mysql-tagged) and the packages each form a partition: a
// shard runs nothing another shard runs, and the union is the whole list. A
// test or package a shard dropped would never run in CI.
func TestCIShardsPartitionTheSuites(t *testing.T) {
	env := fakeEnv(t)
	pkgs, store, mysql := shardLists()
	wantStore := append([]string{}, store...)
	wantMySQL := append(append([]string{}, store...), mysql...)
	sort.Strings(wantStore)
	sort.Strings(wantMySQL)
	var wantPkgs []string
	for _, p := range pkgs {
		if !strings.HasSuffix(p, "/internal/store") {
			wantPkgs = append(wantPkgs, p)
		}
	}
	sort.Strings(wantPkgs)

	for count := 1; count <= 7; count++ {
		var gotStore, gotMySQL, gotPkgs []string
		for index := 1; index <= count; index++ {
			idx, n := strconv.Itoa(index), strconv.Itoa(count)

			out, err := runShard(env, "store-test-shard.sh", idx, n)
			if err != nil {
				t.Fatalf("store shard %s of %d: %v", idx, count, err)
			}
			gotStore = append(gotStore, regexNames(t, out)...)

			out, err = runShard(env, "store-test-shard.sh", idx, n, "mysql")
			if err != nil {
				t.Fatalf("mysql store shard %s of %d: %v", idx, count, err)
			}
			gotMySQL = append(gotMySQL, regexNames(t, out)...)

			out, err = runShard(env, "package-test-shard.sh", idx, n)
			if err != nil {
				t.Fatalf("package shard %s of %d: %v", idx, count, err)
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if line != "" {
					gotPkgs = append(gotPkgs, line)
				}
			}
		}
		assertPartition(t, fmt.Sprintf("store tests, %d shards", count), gotStore, wantStore)
		assertPartition(t, fmt.Sprintf("mysql store tests, %d shards", count), gotMySQL, wantMySQL)
		assertPartition(t, fmt.Sprintf("packages, %d shards", count), gotPkgs, wantPkgs)
	}
}

// TestCIShardScriptsRefuseBadArguments: an index outside 1..count, a
// non-numeric count or an unknown tag is an error, so a workflow typo cannot
// silently run nothing.
func TestCIShardScriptsRefuseBadArguments(t *testing.T) {
	env := fakeEnv(t)
	for _, args := range [][]string{{}, {"1"}, {"0", "3"}, {"4", "3"}, {"1", "x"}, {"", "3"}} {
		if _, err := runShard(env, "store-test-shard.sh", args...); err == nil {
			t.Errorf("store-test-shard.sh %q succeeded, want an error", args)
		}
		if _, err := runShard(env, "package-test-shard.sh", args...); err == nil {
			t.Errorf("package-test-shard.sh %q succeeded, want an error", args)
		}
	}
	if _, err := runShard(env, "store-test-shard.sh", "1", "3", "postgres"); err == nil {
		t.Error("store-test-shard.sh with an unknown tag succeeded, want an error")
	}
}

// shardJobProblems checks a sharded job: its matrix shard list is 1..SHARDS
// with SHARDS set in its env, fail-fast is off so every leg reports, and a
// step hands each leg its own shard.
func shardJobProblems(name string, j tierJob) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(name+": "+f, a...)) }
	n, err := strconv.Atoi(fmt.Sprint(j.Env["SHARDS"]))
	if err != nil || n < 1 {
		add("needs env SHARDS set to the matrix size, got %v", j.Env["SHARDS"])
		return out
	}
	m, _ := j.Strategy["matrix"].(map[string]any)
	legs, _ := m["shard"].([]any)
	var got, want []string
	for _, l := range legs {
		got = append(got, fmt.Sprint(l))
	}
	for i := 1; i <= n; i++ {
		want = append(want, strconv.Itoa(i))
	}
	if !slices.Equal(got, want) {
		add("matrix shard is %v, want 1..%d (SHARDS=%d) so every shard has a leg", got, n, n)
	}
	if fmt.Sprint(j.Strategy["fail-fast"]) != "false" {
		add("strategy.fail-fast must be false so each leg reports on its own")
	}
	handed := false
	for _, s := range j.Steps {
		if fmt.Sprint(s.Env["SHARD"]) == "${{ matrix.shard }}" {
			handed = true
		}
	}
	if !handed {
		add("a step must take SHARD from ${{ matrix.shard }}")
	}
	return out
}

// TestCIShardMatricesCoverEveryLeg checks the sharded jobs in ci.yml and the
// wiring around them: the required check's exact name, its aggregate's
// dependence on every leg, and ci-ok reading the legs (not the aggregate).
func TestCIShardMatricesCoverEveryLeg(t *testing.T) {
	var wf tierWorkflow
	if err := yaml.Unmarshal(readWorkflow(t, "ci.yml"), &wf); err != nil {
		t.Fatal(err)
	}
	var problems []string
	for _, name := range []string{"mysql-store-shard", "deploy-linux"} {
		j, ok := wf.Jobs[name]
		if !ok {
			problems = append(problems, "ci.yml must have a "+name+" job")
			continue
		}
		problems = append(problems, shardJobProblems(name, j)...)
	}

	// The required check keeps its exact name (.mergewake/policy.yaml) and
	// passes only when every MySQL leg succeeded.
	agg, ok := wf.Jobs["mysql-store"]
	switch {
	case !ok:
		problems = append(problems, "ci.yml must keep the mysql-store aggregate (the required check)")
	case agg.Name != "store: mysql and schema drift":
		problems = append(problems, fmt.Sprintf("mysql-store aggregate name is %q, want the required check's name", agg.Name))
	default:
		if got := needsList(agg.Needs); !slices.Equal(got, []string{"mysql-store-shard"}) {
			problems = append(problems, fmt.Sprintf("mysql-store must need mysql-store-shard, got %v", got))
		}
		if !strings.Contains(unwrap(agg.If), "always()") {
			problems = append(problems, "mysql-store must run with always() so a failed leg fails the check, not skips it")
		}
		if !strings.Contains(stepRuns(agg), `"$RESULT" != success`) {
			problems = append(problems, "mysql-store must fail unless every leg succeeded")
		}
	}
	var pol struct {
		RequiredChecks []string `yaml:"required_checks"`
	}
	if err := yaml.Unmarshal(readPolicy(t), &pol); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pol.RequiredChecks, "store: mysql and schema drift") {
		problems = append(problems, "policy.yaml must still require 'store: mysql and schema drift'")
	}

	// ci-ok reads the legs directly, so a cancelled leg reads unknown.
	ciOk, found := wf.Jobs["ci-ok"]
	if !found {
		problems = append(problems, "ci.yml must have a ci-ok job")
	} else {
		needs := needsList(ciOk.Needs)
		for _, want := range []string{"mysql-store-shard", "deploy-linux"} {
			if !slices.Contains(needs, want) {
				problems = append(problems, "ci-ok must need "+want)
			}
		}
		if slices.Contains(needs, "mysql-store") {
			problems = append(problems, "ci-ok must read the legs, not the mysql-store aggregate: a cancelled leg would fail ci-ok instead of reading unknown")
		}
	}

	for _, p := range problems {
		t.Error(p)
	}
}

// TestDeployPromoteWaitsForMainTier pins the hk 6156 ordering: the deploy tier
// starts with the push, so the promote job waits for the main tier's run on
// the target before it reads the gate's ci-ok verdict.
func TestDeployPromoteWaitsForMainTier(t *testing.T) {
	var wf tierWorkflow
	if err := yaml.Unmarshal(readWorkflow(t, "deploy.yml"), &wf); err != nil {
		t.Fatal(err)
	}
	p, ok := wf.Jobs["promote"]
	if !ok {
		t.Fatal("deploy.yml must have a promote job")
	}
	var waits bool
	for _, s := range p.Steps {
		if strings.Contains(s.Run, "ci.yml/runs?head_sha=") && strings.Contains(s.Run, "completed") {
			waits = true
		}
	}
	if !waits {
		t.Fatal("promote must wait for the main tier's ci.yml run on the target before the gate reads ci-ok (hk 6156)")
	}
}

// TestShardSelectorsComeFromWorkflowRevision pins the hk 6156 review fix: the
// selector scripts are checked out from github.workflow_sha, not taken from
// the ref under test. A ci-any-ref dispatch can name an older commit whose
// scripts predate the leg count, and the legs would then fail or run the old
// halves. The scripts must also never cd into their own checkout, since they
// read the module from the working directory.
func TestShardSelectorsComeFromWorkflowRevision(t *testing.T) {
	var wf tierWorkflow
	if err := yaml.Unmarshal(readWorkflow(t, "ci.yml"), &wf); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mysql-store-shard", "deploy-linux"} {
		j, ok := wf.Jobs[name]
		if !ok {
			t.Fatalf("ci.yml must have a %s job", name)
		}
		var pinned bool
		for _, s := range j.Steps {
			if !hasAction(s.Uses, "actions/checkout") || fmt.Sprint(s.With["path"]) != "shard-scripts" {
				continue
			}
			sparse := fmt.Sprint(s.With["sparse-checkout"])
			pinned = fmt.Sprint(s.With["ref"]) == "${{ github.workflow_sha }}" &&
				strings.Contains(sparse, "scripts/store-test-shard.sh") &&
				strings.Contains(sparse, "scripts/package-test-shard.sh")
		}
		if !pinned {
			t.Errorf("%s must check out the selector scripts from github.workflow_sha into shard-scripts (hk 6156 review)", name)
		}
		run := stepRuns(j)
		if strings.Contains(run, "bash scripts/store-test-shard.sh") || strings.Contains(run, "bash scripts/package-test-shard.sh") {
			t.Errorf("%s runs a selector from the ref under test; it must run shard-scripts/scripts/ (hk 6156 review)", name)
		}
		if !strings.Contains(run, "shard-scripts/scripts/store-test-shard.sh") {
			t.Errorf("%s must select its store tests with shard-scripts/scripts/store-test-shard.sh", name)
		}
	}
	for _, f := range []string{"store-test-shard.sh", "package-test-shard.sh"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "scripts", f))
		if err != nil {
			t.Fatal(err)
		}
		src := string(body)
		if strings.Contains(src, "cd \"$here\"") {
			t.Errorf("scripts/%s must not cd into its own checkout: it runs from the module root", f)
		}
	}
}

// TestDeployReRunOfMainCIDeploys pins the hk 6156 review fix: a rerun of a
// main push's ci run that turns green fires workflow_run, and the target job
// accepts exactly that case (a successful rerun of a push to main), so the
// promote held on a failed or pending ci-ok recovers without a manual dispatch.
func TestDeployReRunOfMainCIDeploys(t *testing.T) {
	var wf tierWorkflow
	if err := yaml.Unmarshal(readWorkflow(t, "deploy.yml"), &wf); err != nil {
		t.Fatal(err)
	}
	if _, ok := wf.On["workflow_run"]; !ok {
		t.Fatal("deploy.yml must trigger on workflow_run of ci, so a rerun that turns main green deploys")
	}
	target := unwrap(wf.Jobs["target"].If)
	for _, want := range []string{
		"github.event_name == 'workflow_run'",
		"github.event.workflow_run.event == 'push'",
		"github.event.workflow_run.head_branch == 'main'",
		"github.event.workflow_run.conclusion == 'success'",
		"github.event.workflow_run.run_attempt > 1",
	} {
		if !strings.Contains(target, want) {
			t.Errorf("target job must accept a workflow_run only with %q, got %q", want, target)
		}
	}
	if !strings.Contains(stepRuns(wf.Jobs["target"]), "SHA=\"$RUN_SHA\"") {
		t.Error("target must resolve a workflow_run to its run head sha")
	}
}

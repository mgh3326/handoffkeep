package ciworkflow_test

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestCIOkAggregate runs ci-ok's real guard and script against the needs
// results of concrete runs (hk 2924, hk 3155): the guard decides whether the
// job runs at all, the script whether it passes.

type ciOkCase struct {
	name    string
	env     map[string]string // EVENT, SUITE and the needs results
	skipped bool              // the guard must skip the job
	exit    int               // script exit code when the job runs
	output  string            // substring the script must print
}

func greenEnv(suite string, overrides ...string) map[string]string {
	env := map[string]string{
		"EVENT": "workflow_dispatch", "SUITE": suite,
		"TEST": "success", "PR_STORE_DB": "success", "VITEST": "skipped", "BUILD": "skipped",
		"AUTO_TAG": "success", "POST_LINUX": "success", "POST_MACOS": "success",
	}
	if suite == "all" {
		env["VITEST"], env["BUILD"] = "success", "success"
	}
	for i := 0; i+1 < len(overrides); i += 2 {
		env[overrides[i]] = overrides[i+1]
	}
	return env
}

var ciOkCases = []ciOkCase{
	// The ci-ok (test) step env of run 42061203904: macOS never got a runner.
	{name: "run-42061203904-abandoned-macos-skips", env: greenEnv("test", "POST_MACOS", "abandoned"), skipped: true},
	{name: "suite-test-green-passes", env: greenEnv("test")},
	{name: "suite-test-vitest-ran-fails", env: greenEnv("test", "VITEST", "success"), exit: 1, output: "vitest ran outside its suite"},
	{name: "failure-with-abandoned-fails", env: greenEnv("test", "TEST", "failure", "POST_MACOS", "abandoned"), exit: 1, output: "test did not succeed: failure"},
	{name: "suite-all-green-passes", env: greenEnv("all")},
	// pr-store-db always reports: success on every event that runs the test suite.
	{name: "pull-request-green-passes", env: greenEnv("all", "EVENT", "pull_request", "POST_LINUX", "skipped", "POST_MACOS", "skipped")},
	{name: "pr-store-db-failure-fails", env: greenEnv("test", "PR_STORE_DB", "failure"), exit: 1, output: "pr-store-db did not succeed: failure"},
	{name: "pr-store-db-skipped-in-test-suite-fails", env: greenEnv("test", "PR_STORE_DB", "skipped"), exit: 1, output: "pr-store-db did not succeed: skipped"},
	{name: "pr-store-db-ran-outside-suite-fails", env: greenEnv("vitest", "VITEST", "success", "TEST", "skipped", "PR_STORE_DB", "success", "POST_LINUX", "skipped", "POST_MACOS", "skipped"), exit: 1, output: "pr-store-db ran outside its suite or tier: success"},
}

var ciOkNeeds = []string{"TEST", "PR_STORE_DB", "VITEST", "BUILD", "AUTO_TAG", "POST_LINUX", "POST_MACOS"}

// ciOkJob returns ci-ok's if: expression (without ${{ }}) and its script.
func ciOkJob(ci []byte) (string, string, error) {
	var wf tierWorkflow
	if err := yaml.Unmarshal(ci, &wf); err != nil {
		return "", "", err
	}
	job, ok := wf.Jobs["ci-ok"]
	if !ok || len(job.Steps) != 1 {
		return "", "", fmt.Errorf("ci-ok job with one step not found")
	}
	cond := strings.TrimSpace(job.If)
	cond = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(cond, "${{"), "}}"))
	return cond, job.Steps[0].Run, nil
}

// checkCIOkCase returns nil when ci-ok in ci behaves as c expects.
func checkCIOkCase(ci []byte, c ciOkCase) error {
	cond, script, err := ciOkJob(ci)
	if err != nil {
		return err
	}
	results := make([]string, len(ciOkNeeds))
	for i, k := range ciOkNeeds {
		results[i] = c.env[k]
	}
	runs, err := evalNeedsExpr(cond, results)
	if err != nil {
		return err
	}
	if runs == c.skipped {
		return fmt.Errorf("guard %q with needs %v: runs=%v, want skipped=%v", cond, results, runs, c.skipped)
	}
	if !runs {
		return nil
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range c.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		return err
	}
	if code != c.exit {
		return fmt.Errorf("script exit %d, want %d; output:\n%s", code, c.exit, out)
	}
	if !strings.Contains(string(out), c.output) {
		return fmt.Errorf("script output lacks %q:\n%s", c.output, out)
	}
	return nil
}

func TestCIOkAggregate(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	for _, c := range ciOkCases {
		t.Run(c.name, func(t *testing.T) {
			if err := checkCIOkCase(ci, c); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The pre-hk 3155 guard fails ci-ok on an abandoned job; the aggregate check
// must catch it.
func TestCIOkAggregateMutantTurnsRed(t *testing.T) {
	ci := string(readWorkflow(t, "ci.yml"))
	old := "if: ${{ " + ciOkIf + " }}"
	if strings.Count(ci, old) != 1 {
		t.Fatalf("mutant anchor %q matches %d times", old, strings.Count(ci, old))
	}
	mutant := []byte(strings.Replace(ci, old, "if: ${{ "+ciOkIfCancelledOnly+" }}", 1))
	for _, c := range ciOkCases {
		if checkCIOkCase(mutant, c) != nil {
			return
		}
	}
	t.Fatal("ci-ok-ignores-abandoned mutant passed TestCIOkAggregate")
}

// evalNeedsExpr evaluates the subset of the Actions expression language that
// ci-ok's guard uses: always(), contains(needs.*.result, '<v>'), !, &&, ||
// and parentheses. results are the needs results.
func evalNeedsExpr(expr string, results []string) (bool, error) {
	p := &needsParser{s: expr, results: results}
	v, err := p.or()
	if err != nil {
		return false, err
	}
	p.space()
	if p.i != len(p.s) {
		return false, fmt.Errorf("unexpected %q in %q", p.s[p.i:], expr)
	}
	return v, nil
}

type needsParser struct {
	s       string
	i       int
	results []string
}

func (p *needsParser) space() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *needsParser) eat(tok string) bool {
	p.space()
	if strings.HasPrefix(p.s[p.i:], tok) {
		p.i += len(tok)
		return true
	}
	return false
}

func (p *needsParser) or() (bool, error) {
	v, err := p.and()
	for err == nil && p.eat("||") {
		var r bool
		r, err = p.and()
		v = v || r
	}
	return v, err
}

func (p *needsParser) and() (bool, error) {
	v, err := p.unary()
	for err == nil && p.eat("&&") {
		var r bool
		r, err = p.unary()
		v = v && r
	}
	return v, err
}

func (p *needsParser) unary() (bool, error) {
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
	case p.eat("always()"):
		return true, nil
	case p.eat("contains(needs.*.result, '"):
		end := strings.Index(p.s[p.i:], "')")
		if end < 0 {
			return false, fmt.Errorf("unterminated contains in %q", p.s)
		}
		want := p.s[p.i : p.i+end]
		p.i += end + 2
		return slices.Contains(p.results, want), nil
	}
	return false, fmt.Errorf("unsupported expression at %d in %q", p.i, p.s)
}

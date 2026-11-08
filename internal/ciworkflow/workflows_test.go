package ciworkflow_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Static guards for CI artifact uploads (task #2037). The account artifact
// quota filled because every CI run uploaded binaries, the gitleaks SARIF and
// docker build records with the default 90-day retention, and no consumer
// reads any of them. These assertions keep the workflow files from growing
// the artifact store again:
//   - every actions/upload-artifact step must set with.retention-days to a
//     literal integer from 1 to 7 (strings and expressions are rejected)
//   - every gitleaks/gitleaks-action step must set env
//     GITLEAKS_ENABLE_UPLOAD_ARTIFACT to "false"
//   - every docker/build-push-action step must set env
//     DOCKER_BUILD_RECORD_UPLOAD to "false"
// Each of the three kinds must appear at least once so the test cannot pass
// by matching nothing.

type step struct {
	If               string         `yaml:"if"`
	Run              string         `yaml:"run"`
	WorkingDirectory string         `yaml:"working-directory"`
	Name             string         `yaml:"name"`
	Uses             string         `yaml:"uses"`
	With             map[string]any `yaml:"with"`
	Env              map[string]any `yaml:"env"`
}

type workflowFile struct {
	Jobs map[string]struct {
		Uses  string `yaml:"uses"`
		Steps []step `yaml:"steps"`
	} `yaml:"jobs"`
}

// workflowFiles returns the sorted *.yml/*.yaml names in the workflow dir.
func workflowFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".yml") || strings.HasSuffix(e.Name(), ".yaml") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		t.Fatalf("%s has no workflow files", dir)
	}
	sort.Strings(files)
	return files
}

func hasAction(uses, name string) bool {
	return uses == name || strings.HasPrefix(uses, name+"@")
}

func stepRef(file, job string, idx int, s step) string {
	if s.Name != "" {
		return fmt.Sprintf("%s job %q step %d (%q, uses %s)", file, job, idx+1, s.Name, s.Uses)
	}
	return fmt.Sprintf("%s job %q step %d (uses %s)", file, job, idx+1, s.Uses)
}

func envString(env map[string]any, key string) (string, bool) {
	v, ok := env[key]
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	default:
		return fmt.Sprint(t), true
	}
}

func TestWorkflowArtifactUploads(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "workflows")
	files := workflowFiles(t, dir)

	var uploads, gitleaks, buildpush int

	for _, fname := range files {
		body, err := os.ReadFile(filepath.Join(dir, fname))
		if err != nil {
			t.Fatalf("read %s: %v", fname, err)
		}
		var wf workflowFile
		if err := yaml.Unmarshal(body, &wf); err != nil {
			t.Fatalf("parse %s: %v", fname, err)
		}
		jobs := make([]string, 0, len(wf.Jobs))
		for j := range wf.Jobs {
			jobs = append(jobs, j)
		}
		sort.Strings(jobs)
		for _, job := range jobs {
			for i, s := range wf.Jobs[job].Steps {
				switch {
				case hasAction(s.Uses, "actions/upload-artifact"):
					uploads++
					v, ok := s.With["retention-days"]
					if !ok {
						t.Errorf("%s: missing with.retention-days", stepRef(fname, job, i, s))
						continue
					}
					days, ok := v.(int)
					if !ok {
						t.Errorf("%s: with.retention-days must be a literal integer, got %T %v",
							stepRef(fname, job, i, s), v, v)
						continue
					}
					if days < 1 || days > 7 {
						t.Errorf("%s: with.retention-days=%d, want an integer from 1 to 7",
							stepRef(fname, job, i, s), days)
					}
				case hasAction(s.Uses, "gitleaks/gitleaks-action"):
					gitleaks++
					v, ok := envString(s.Env, "GITLEAKS_ENABLE_UPLOAD_ARTIFACT")
					if !ok {
						t.Errorf("%s: missing env GITLEAKS_ENABLE_UPLOAD_ARTIFACT",
							stepRef(fname, job, i, s))
						continue
					}
					if v != "false" {
						t.Errorf("%s: env GITLEAKS_ENABLE_UPLOAD_ARTIFACT=%q, want \"false\"",
							stepRef(fname, job, i, s), v)
					}
				case hasAction(s.Uses, "docker/build-push-action"):
					buildpush++
					v, ok := envString(s.Env, "DOCKER_BUILD_RECORD_UPLOAD")
					if !ok {
						t.Errorf("%s: missing env DOCKER_BUILD_RECORD_UPLOAD",
							stepRef(fname, job, i, s))
						continue
					}
					if v != "false" {
						t.Errorf("%s: env DOCKER_BUILD_RECORD_UPLOAD=%q, want \"false\"",
							stepRef(fname, job, i, s), v)
					}
				}
			}
		}
	}

	for _, kind := range []struct {
		name  string
		found int
	}{
		{"actions/upload-artifact", uploads},
		{"gitleaks/gitleaks-action", gitleaks},
		{"docker/build-push-action", buildpush},
	} {
		if kind.found == 0 {
			t.Errorf("no %s step found in %s; the guard must match at least one step", kind.name, dir)
		}
	}
}

var pinnedUses = regexp.MustCompile(`^([^@]+)@([0-9a-f]{40})$`)

// TestWorkflowPinning keeps CI reproducible: every external action is pinned
// to a full commit SHA, each action resolves to the same SHA in every
// workflow, and the Go and Node toolchains come from the repo's version files
// (go.mod, web/console/.nvmrc) rather than floating labels like "stable".
func TestWorkflowPinning(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "workflows")
	shas := map[string]string{} // action -> "sha (file)"
	for _, fname := range workflowFiles(t, dir) {
		body, err := os.ReadFile(filepath.Join(dir, fname))
		if err != nil {
			t.Fatalf("read %s: %v", fname, err)
		}
		var wf workflowFile
		if err := yaml.Unmarshal(body, &wf); err != nil {
			t.Fatalf("parse %s: %v", fname, err)
		}
		jobs := make([]string, 0, len(wf.Jobs))
		for j := range wf.Jobs {
			jobs = append(jobs, j)
		}
		sort.Strings(jobs)
		for _, job := range jobs {
			uses := []string{wf.Jobs[job].Uses}
			for _, s := range wf.Jobs[job].Steps {
				uses = append(uses, s.Uses)
				switch {
				case hasAction(s.Uses, "actions/setup-go"):
					t.Errorf("%s job %q: call ./.github/actions/setup-go-cached instead of actions/setup-go, so the toolchain cache and download retry apply", fname, job)
				case hasAction(s.Uses, "actions/setup-node"):
					if v, _ := s.With["node-version-file"].(string); v != "web/console/.nvmrc" || s.With["node-version"] != nil {
						t.Errorf("%s job %q: setup-node must use node-version-file: web/console/.nvmrc, got %v", fname, job, s.With)
					}
				}
			}
			for _, u := range uses {
				if u == "" || strings.HasPrefix(u, "./") || strings.HasPrefix(u, "docker://") {
					continue
				}
				m := pinnedUses.FindStringSubmatch(u)
				if m == nil {
					t.Errorf("%s job %q: action %q is not pinned to a full commit SHA", fname, job, u)
					continue
				}
				where := m[2] + " (" + fname + ")"
				if prev, ok := shas[m[1]]; ok && !strings.HasPrefix(prev, m[2]) {
					t.Errorf("%s pinned to %s and %s; use one version everywhere", m[1], prev, where)
				}
				shas[m[1]] = where
			}
		}
	}
	// The composite action holds the only actions/setup-go and actions/cache
	// steps, so its pins follow the same one-version rule.
	for _, s := range readSetupGoCachedAction(t).Runs.Steps {
		if s.Uses == "" {
			continue
		}
		m := pinnedUses.FindStringSubmatch(s.Uses)
		if m == nil {
			t.Errorf("%s: action %q is not pinned to a full commit SHA", setupGoCachedActionFile, s.Uses)
			continue
		}
		where := m[2] + " (" + setupGoCachedActionFile + ")"
		if prev, ok := shas[m[1]]; ok && !strings.HasPrefix(prev, m[2]) {
			t.Errorf("%s pinned to %s and %s; use one version everywhere", m[1], prev, where)
		}
		shas[m[1]] = where
	}
}

// setupGoCachedActionFile is the one local composite action every job uses
// for Go, relative to the repository root.
const setupGoCachedActionFile = ".github/actions/setup-go-cached/action.yml"

type compositeAction struct {
	Runs struct {
		Steps []step `yaml:"steps"`
	} `yaml:"runs"`
}

func readSetupGoCachedAction(t *testing.T) compositeAction {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(setupGoCachedActionFile)))
	if err != nil {
		t.Fatalf("read %s: %v", setupGoCachedActionFile, err)
	}
	var a compositeAction
	if err := yaml.Unmarshal(body, &a); err != nil {
		t.Fatalf("parse %s: %v", setupGoCachedActionFile, err)
	}
	if len(a.Runs.Steps) == 0 {
		t.Fatalf("%s has no steps", setupGoCachedActionFile)
	}
	return a
}

// TestSetupGoCachedAction checks the toolchain cache and the download retry in
// the composite action. The cache must restore before actions/setup-go runs,
// its key must include the Go version and runner OS and arch, and setup-go must
// get three attempts that all read go.mod.
func TestSetupGoCachedAction(t *testing.T) {
	a := readSetupGoCachedAction(t)
	cacheIdx, setupIdx, setupAttempts := -1, -1, 0
	for i, s := range a.Runs.Steps {
		switch {
		case hasAction(s.Uses, "actions/cache"):
			if cacheIdx < 0 {
				cacheIdx = i
			}
			path, _ := s.With["path"].(string)
			if !strings.HasPrefix(path, "${{ runner.tool_cache }}/go/") {
				t.Errorf("cache path must be the tool cache's go/<version> directory, got %q", path)
			}
			key, _ := s.With["key"].(string)
			for _, want := range []string{"runner.os", "runner.arch", "steps.version.outputs.version"} {
				if !strings.Contains(key, want) {
					t.Errorf("cache key %q must include %s so toolchains do not leak across versions or platforms", key, want)
				}
			}
		case hasAction(s.Uses, "actions/setup-go"):
			setupAttempts++
			if setupIdx < 0 {
				setupIdx = i
			}
			if v, _ := s.With["go-version-file"].(string); v != "go.mod" || s.With["go-version"] != nil {
				t.Errorf("setup-go must use go-version-file: go.mod, got %v", s.With)
			}
		}
	}
	if cacheIdx < 0 || setupIdx < 0 || cacheIdx > setupIdx {
		t.Errorf("actions/cache must restore before actions/setup-go (cache step %d, setup-go step %d)", cacheIdx, setupIdx)
	}
	if setupAttempts != 3 {
		t.Errorf("setup-go must run three bounded attempts on a cache miss, got %d", setupAttempts)
	}
}

// Full Go suites execute console bundle rebuild and drift tests, so their job
// must provision the same locked frontend toolchain before invoking them.
func TestFullGoSuitesProvisionConsole(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "workflows")
	suites := 0
	for _, fname := range workflowFiles(t, dir) {
		body, err := os.ReadFile(filepath.Join(dir, fname))
		if err != nil {
			t.Fatal(err)
		}
		var wf workflowFile
		if err := yaml.Unmarshal(body, &wf); err != nil {
			t.Fatal(err)
		}
		for job, spec := range wf.Jobs {
			node, dependencies, build, drift, clean := false, false, false, false, false
			for _, s := range spec.Steps {
				if hasAction(s.Uses, "actions/setup-node") {
					node = true
				}
				if s.WorkingDirectory == "web/console" && strings.TrimSpace(s.Run) == "npm ci" {
					dependencies = node
				}
				if s.WorkingDirectory == "web/console" && strings.TrimSpace(s.Run) == "npm run build" {
					build = dependencies
				}
				if strings.TrimSpace(s.Run) == "git diff --exit-code -- internal/ui/static/console" {
					drift = build
				}
				if strings.Contains(s.Run, "git status --porcelain --untracked-files=all -- internal/ui/static/console") {
					clean = drift
				}
				if fullGoSuite(s.Run) {
					suites++
					if !node || !dependencies || !build || !drift || !clean {
						t.Errorf("%s job %s: full Go suite requires setup-node, npm ci, console build and tracked/untracked bundle checks first", fname, job)
					}
				}
			}
		}
	}
	if suites < 2 {
		t.Fatal("expected CI and release full Go suites")
	}
}

func fullGoSuite(run string) bool {
	for _, line := range strings.Split(run, "\n") {
		args := strings.Fields(line)
		if len(args) < 3 || args[0] != "go" || args[1] != "test" {
			continue
		}
		for _, arg := range args[2:] {
			if arg == "./..." {
				return true
			}
		}
	}
	return false
}

func TestFullGoSuiteCommand(t *testing.T) {
	for _, tc := range []struct {
		run  string
		want bool
	}{
		{"go test -race -timeout 30m ./...", true},
		{"go vet ./...\ngo test ./internal/ciworkflow", false},
		{"go vet ./...\ngo test -v -race ./...", true},
		{"go test ./internal/...", false},
	} {
		if got := fullGoSuite(tc.run); got != tc.want {
			t.Errorf("%q full=%v want=%v", tc.run, got, tc.want)
		}
	}
}

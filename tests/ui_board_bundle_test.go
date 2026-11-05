package tests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// readTreeDir maps every file under root to its bytes, keyed by slash
// relative path. The console artifact comparison is recursive and exact:
// neither the emitted build nor the committed directory may hide drift in a
// file nobody named.
func readTreeDir(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = body
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s has no files", root)
	}
	return out
}

// consoleTreeError reports the first difference between the committed
// artifact tree and a build's emitted tree: an extra file, a missing file, or
// changed bytes. Exact file-set plus byte equality means stale, truncated, or
// extra output can never pass as a fresh build.
func consoleTreeError(committed, emitted map[string][]byte) error {
	names := make([]string, 0, len(committed)+len(emitted))
	seen := map[string]bool{}
	for name := range committed {
		names = append(names, name)
		seen[name] = true
	}
	for name := range emitted {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		want, inCommitted := committed[name]
		got, inEmitted := emitted[name]
		switch {
		case !inCommitted:
			return fmt.Errorf("build emitted extra file %s", name)
		case !inEmitted:
			return fmt.Errorf("build output missing %s", name)
		case !bytes.Equal(want, got):
			return fmt.Errorf("build output %s differs from the committed artifact", name)
		}
	}
	return nil
}

// consoleContractsError checks the entry outputs a correct multi-entry build
// must produce: the shell, server and deploys entries, their styles, and the
// shared chunk, plus the API/security contracts each bundle carries.
func consoleContractsError(tree map[string][]byte) error {
	for _, name := range []string{"shell.js", "shell.css", "server.js", "server.css", "tokens.css", "shared-client.js", "shared-DocView.js", "shared-DocMarkdown.js"} {
		body, ok := tree[name]
		if !ok {
			return fmt.Errorf("console artifact %s missing", name)
		}
		if len(body) == 0 {
			return fmt.Errorf("console artifact %s is empty", name)
		}
	}
	shell := string(tree["shell.js"])
	// The shell entry mounts into the template's root, reaches the document
	// viewer through a lazy chunk, and makes no API read itself.
	for _, want := range []string{"shell-root", "shared-DocView.js"} {
		if !strings.Contains(shell, want) {
			return fmt.Errorf("shell.js missing %q", want)
		}
	}
	// The viewer reads one document through the console's JSON route.
	if !strings.Contains(string(tree["shared-DocView.js"]), "/ui/api/board/doc") {
		return errors.New("shared-DocView.js lost the /ui/api/board/doc contract")
	}
	if !strings.Contains(string(tree["shell.js"]), "/ui/api/deploy-pending") {
		return errors.New("shell.js lost the /ui/api/deploy-pending contract")
	}
	for name, body := range tree {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		if strings.Contains(string(body), "/v1/nodes") || strings.Contains(string(body), "/v1/jobs") || strings.Contains(string(body), fleetTestSecret) {
			return fmt.Errorf("%s contains a hub path or secret", name)
		}
	}
	return nil
}

// consoleSiblingEntriesError checks that a build kept every pre-existing
// console entry output. Adding or repointing an entry must never delete a
// sibling's emitted file.
func consoleSiblingEntriesError(tree map[string][]byte) error {
	for _, name := range []string{"shell.js", "shell.css", "server.js"} {
		if len(tree[name]) == 0 {
			return fmt.Errorf("sibling entry output %s missing", name)
		}
	}
	shared := 0
	for name, body := range tree {
		if strings.HasPrefix(name, "shared-") && strings.HasSuffix(name, ".js") && len(body) > 0 {
			shared++
		}
	}
	if shared == 0 {
		return errors.New("build emitted no shared-*.js chunk")
	}
	return nil
}

func consoleProjectDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller path unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "web", "console")
}

func requireNPM(t *testing.T) string {
	t.Helper()
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm unavailable; CI runs this check where node is installed")
	}
	return npm
}

// viteBuild runs the console build into an explicit outDir so the committed
// artifacts are never touched by verification. config is a filename inside
// the project directory; empty selects the real vite.config.ts.
func viteBuild(t *testing.T, npm, projectDir, config, outDir string) {
	t.Helper()
	args := []string{"run", "build", "--"}
	if config != "" {
		args = append(args, "--config", config)
	}
	args = append(args, "--outDir", outDir)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, npm, args...)
	cmd.Dir = projectDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vite build (config=%q) failed: %v\n%s", config, err, output)
	}
}

// writeMutantConfig applies a mutation to the real vite config and writes it
// inside the project directory so relative entry resolution matches the real
// build. It returns the filename to pass via --config.
func writeMutantConfig(t *testing.T, projectDir, name string, mutate func(string) string) string {
	t.Helper()
	path := filepath.Join(projectDir, "vite.config.ts")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutated := mutate(string(source))
	if mutated == string(source) {
		t.Fatalf("%s mutant did not change vite.config.ts", name)
	}
	filename := "vite.config.mutant-" + name + ".ts"
	if err := os.WriteFile(filepath.Join(projectDir, filename), []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(projectDir, filename)) })
	return filename
}

func TestConsoleMultiEntryArtifacts(t *testing.T) {
	committed := readTreeDir(t, consoleDir(t))
	if err := consoleContractsError(committed); err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(shellPagePath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="shell-root"`, "/ui/static/console/shell.js", "/ui/static/console/shell.css"} {
		if !strings.Contains(string(page), want) {
			t.Fatalf("console.html missing %q", want)
		}
	}

}

// One clean build into a scratch outDir must emit exactly the committed file
// set — same names, same bytes — including the shared chunk. A stale file
// seeded into outDir proves the real emptyOutDir policy is applied
// functionally: if the setting is lost, the stale file survives the build and
// the comparison fails. The committed directory is never written.
func TestConsoleCleanBuildMatchesCommitted(t *testing.T) {
	npm := requireNPM(t)
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "stale-legacy.js"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	viteBuild(t, npm, consoleProjectDir(t), "", outDir)
	emitted := readTreeDir(t, outDir)
	if _, ok := emitted["stale-legacy.js"]; ok {
		t.Fatal("build kept a stale file in outDir: the emptyOutDir policy is not applied")
	}
	committed := readTreeDir(t, consoleDir(t))
	if err := consoleTreeError(committed, emitted); err != nil {
		t.Fatalf("committed artifacts drifted from a clean build: %v", err)
	}
	if err := consoleContractsError(emitted); err != nil {
		t.Fatalf("clean multi-entry build lost an entry contract: %v", err)
	}
	if err := consoleSiblingEntriesError(emitted); err != nil {
		t.Fatalf("clean build dropped a sibling entry: %v", err)
	}
}

// Each destructive mutant must turn the drift gate RED. Tree mutants check
// the comparison itself; config mutants run real builds so the failure is
// produced by the build, not by matching a token in the config source.
func TestConsoleBuildDriftMutantsTurnRed(t *testing.T) {
	committed := readTreeDir(t, consoleDir(t))
	for name, mutate := range map[string]func(map[string][]byte){
		"missing-shell.js":         func(tree map[string][]byte) { delete(tree, "shell.js") },
		"missing-shared-client.js": func(tree map[string][]byte) { delete(tree, "shared-client.js") },
		"extra-stale.js":           func(tree map[string][]byte) { tree["stale-legacy.js"] = []byte("stale") },
		"stale-shell.js":           func(tree map[string][]byte) { tree["shell.js"] = append([]byte("drift:"), tree["shell.js"]...) },
		"empty-shell.js":           func(tree map[string][]byte) { tree["shell.js"] = nil },
	} {
		t.Run("tree-"+name, func(t *testing.T) {
			mutated := make(map[string][]byte, len(committed))
			for k, v := range committed {
				mutated[k] = v
			}
			mutate(mutated)
			if err := consoleTreeError(committed, mutated); err == nil {
				t.Fatalf("tree compare stayed green on %s", name)
			}
		})
	}

	npm := requireNPM(t)
	projectDir := consoleProjectDir(t)

	// Deleting the actual emptyOutDir setting must fail even though the word
	// still appears in the config comment: outDir is outside the project
	// root, so without the setting vite leaves the seeded stale file in
	// place and it lands in the emitted set.
	t.Run("config-no-emptyOutDir", func(t *testing.T) {
		config := writeMutantConfig(t, projectDir, "no-empty-outdir", func(source string) string {
			return strings.Replace(source, "\n    emptyOutDir: true,", "", 1)
		})
		outDir := filepath.Join(t.TempDir(), "out")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "stale-legacy.js"), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		viteBuild(t, npm, projectDir, config, outDir)
		emitted := readTreeDir(t, outDir)
		if err := consoleTreeError(committed, emitted); err == nil {
			t.Fatal("tree compare stayed green after deleting the emptyOutDir setting")
		}
	})

	// A build without the deploys entry emits no deploys output; the emitted
	// set must not silently shrink.
	t.Run("config-single-entry", func(t *testing.T) {
		config := writeMutantConfig(t, projectDir, "single-entry", func(source string) string {
			return strings.Replace(source, "\n        deploys: resolve(root, \"src/deploys/main.tsx\"),", "", 1)
		})
		outDir := filepath.Join(t.TempDir(), "out")
		viteBuild(t, npm, projectDir, config, outDir)
		emitted := readTreeDir(t, outDir)
		if err := consoleTreeError(committed, emitted); err == nil {
			t.Fatal("tree compare stayed green on a single-entry build")
		}
	})
}

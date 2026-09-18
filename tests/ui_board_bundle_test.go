package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// consoleArtifactsError is the drift gate for the multi-entry console build.
// It reports the first missing or malformed artifact instead of aborting on
// the bundle list so a mutant regression produces a readable failure.
func consoleArtifactsError(dir string) error {
	required := []string{"fleet.js", "fleet.css", "board.js", "board.css"}
	for _, name := range required {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("console artifact %s missing: %w", name, err)
		}
		if info.Size() == 0 {
			return fmt.Errorf("console artifact %s is empty", name)
		}
	}
	fleet, err := os.ReadFile(filepath.Join(dir, "fleet.js"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(fleet), "/ui/api/fleet") {
		return errors.New("fleet.js lost the /ui/api/fleet contract")
	}
	board, err := os.ReadFile(filepath.Join(dir, "board.js"))
	if err != nil {
		return err
	}
	for _, want := range []string{"/ui/api/board/tasks", "/ui/api/policy/active"} {
		if !strings.Contains(string(board), want) {
			return fmt.Errorf("board.js missing %q", want)
		}
	}
	for name, body := range map[string]string{"fleet.js": string(fleet), "board.js": string(board)} {
		if strings.Contains(body, "/v1/nodes") || strings.Contains(body, "/v1/jobs") || strings.Contains(body, fleetTestSecret) {
			return fmt.Errorf("%s contains a hub path or secret", name)
		}
	}
	return nil
}

func boardTemplatePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller path unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "internal", "ui", "templates", "board.html")
}

func viteConfigPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller path unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "web", "console", "vite.config.ts")
}

// multiEntryConfigError verifies the vite config declares both entries in one
// build. A single-entry regression — or emptyOutDir wiping a sibling output —
// is what this check exists to catch.
func multiEntryConfigError(source string) error {
	for _, entry := range []string{"fleet", "board"} {
		if !strings.Contains(source, entry+":") {
			return fmt.Errorf("vite config lost the %q entry", entry)
		}
	}
	if !strings.Contains(source, "emptyOutDir") {
		return errors.New("vite config lost the explicit emptyOutDir policy")
	}
	return nil
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConsoleMultiEntryArtifacts(t *testing.T) {
	if err := consoleArtifactsError(consoleDir(t)); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(viteConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := multiEntryConfigError(string(config)); err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(boardTemplatePath(t))
	if err != nil {
		t.Fatal(err)
	}
	body := string(page)
	for _, want := range []string{`id="board-root"`, "/ui/static/console/board.js", "/ui/static/console/board.css"} {
		if !strings.Contains(body, want) {
			t.Fatalf("board.html missing %q", want)
		}
	}
	fleet, err := os.ReadFile(fleetPagePath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="fleet-root"`, "/ui/static/console/fleet.js", "/ui/static/console/fleet.css"} {
		if !strings.Contains(string(fleet), want) {
			t.Fatalf("fleet.html missing %q", want)
		}
	}
}

// A single-entry or emptied output directory is the regression this test
// guards: the artifact check must turn RED on each destructive mutant.
func TestConsoleArtifactMutantsTurnRed(t *testing.T) {
	src := consoleDir(t)
	for _, name := range []string{"fleet.js", "board.js", "fleet.css", "board.css"} {
		t.Run("missing-"+name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "console")
			copyDir(t, src, dst)
			if err := os.Remove(filepath.Join(dst, name)); err != nil {
				t.Fatal(err)
			}
			if err := consoleArtifactsError(dst); err == nil {
				t.Fatalf("artifact check stayed green without %s", name)
			}
		})
	}
	t.Run("emptied-board.js", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "console")
		copyDir(t, src, dst)
		if err := os.WriteFile(filepath.Join(dst, "board.js"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := consoleArtifactsError(dst); err == nil {
			t.Fatal("artifact check stayed green on an empty board.js")
		}
	})
	for name, mutant := range map[string]string{
		"single-entry":          `export default { build: { emptyOutDir: true, rollupOptions: { input: { fleet: "src/main.tsx" } } } }`,
		"no-emptyOutDir-policy": `export default { build: { rollupOptions: { input: { fleet: "src/main.tsx", board: "src/board.tsx" } } } }`,
	} {
		t.Run("config-"+name, func(t *testing.T) {
			if err := multiEntryConfigError(mutant); err == nil {
				t.Fatalf("config check stayed green on %s mutant", name)
			}
		})
	}
}

// One clean build must emit every entry: after a single `npm run build` both
// fleet and board outputs exist, proving emptyOutDir does not delete the
// sibling entry it did not write. Skipped only when node/npm are absent.
func TestConsoleCleanMultiEntryBuild(t *testing.T) {
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm unavailable; CI runs this check where node is installed")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller path unavailable")
	}
	consoleSrc := filepath.Join(filepath.Dir(file), "..", "web", "console")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, npm, "run", "build")
	cmd.Dir = consoleSrc
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("npm run build failed: %v\n%s", err, output)
	}
	if err := consoleArtifactsError(consoleDir(t)); err != nil {
		t.Fatalf("clean multi-entry build did not produce both entries: %v", err)
	}
}

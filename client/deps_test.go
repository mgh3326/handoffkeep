package client

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The fleet services import this package, so it must not drag hk's store,
// pgx or any other module dependency into their builds (hk:doc
// decision/2026-11-08/fleet-dependency-boundaries §1, §5). Its whole
// dependency closure is the standard library plus internal/buildinfo, which
// is itself stdlib-only.
func TestClientImportsOnlyStandardLibrary(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		gobin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	out, err := exec.Command(gobin, "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	allowed := map[string]bool{
		"github.com/mgh3326/handoffkeep/client":             true,
		"github.com/mgh3326/handoffkeep/internal/buildinfo": true,
	}
	self := false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, standard, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("unexpected go list line %q", line)
		}
		if path == "github.com/mgh3326/handoffkeep/client" {
			self = true
		}
		if standard == "true" {
			continue
		}
		if !allowed[path] {
			t.Errorf("client depends on %s, outside the standard library", path)
		}
	}
	if !self {
		t.Fatalf("go list -deps did not list the client package itself:\n%s", out)
	}
}

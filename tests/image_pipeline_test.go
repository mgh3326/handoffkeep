package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Static guards for the image pipeline (task #519). These assert the parts of
// the Dockerfile / .dockerignore / image workflow that must stay true for the
// deploy runbook's vcs-stamp verification and the public-repo supply-chain
// rules. Mutating them must fail here, not in production:
//   - adding -buildvcs=false (or GOFLAGS equivalent) to the Dockerfile
//   - excluding .git from the Docker build context
//   - copying anything but the built binary into the final image stage
//   - using a tag/branch-pinned (unpinned) third-party action

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

func TestDockerfileKeepsVCSStamping(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")
	var instructions []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			instructions = append(instructions, line)
		}
	}
	lower := strings.ToLower(strings.Join(instructions, "\n"))
	if strings.Contains(lower, "buildvcs=false") || strings.Contains(lower, "buildvcs = false") {
		t.Fatal("Dockerfile disables VCS stamping; the image binary must carry vcs.revision")
	}
	if !regexp.MustCompile(`(?m)^COPY \. \.$`).MatchString(body) {
		t.Fatal("Dockerfile build stage must `COPY . .` so .git reaches `go build` for VCS stamping")
	}
}

func TestDockerignoreKeepsGitInContext(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", ".dockerignore"))
	if os.IsNotExist(err) {
		return // no .dockerignore: context keeps everything, .git included
	}
	if err != nil {
		t.Fatalf("read .dockerignore: %v", err)
	}
	for i, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pat := strings.TrimPrefix(line, "!")
		switch {
		case pat == ".git" || pat == ".git/" || strings.HasPrefix(pat, ".git/"):
			t.Fatalf(".dockerignore:%d excludes %q — .git must stay in the build context for vcs.revision", i+1, line)
		case pat == ".*" || pat == "**" || pat == "/" || pat == "*":
			t.Fatalf(".dockerignore:%d pattern %q would exclude .git from the build context", i+1, line)
		}
	}
}

func TestDockerfileFinalStageCopiesOnlyBinary(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")
	lastFrom := -1
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "FROM ") {
			lastFrom = i
		}
	}
	if lastFrom < 0 {
		t.Fatal("Dockerfile has no FROM")
	}
	for i := lastFrom + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(strings.ToUpper(line), "COPY") && !strings.Contains(line, "--from=") {
			t.Fatalf("Dockerfile final stage line %d copies build context into the image: %q (only --from=<stage> binary copies allowed)", i+1, line)
		}
	}
}

func TestImageWorkflowPinsActionsBySHA(t *testing.T) {
	body := readRepoFile(t, filepath.Join(".github", "workflows", "image.yml"))
	uses := regexp.MustCompile(`(?m)uses:\s*([^\s#]+)`)
	for _, m := range uses.FindAllStringSubmatch(body, -1) {
		ref := m[1]
		at := strings.LastIndex(ref, "@")
		if at < 0 {
			t.Fatalf("workflow uses unpinned action %q", ref)
		}
		if ok, _ := regexp.MatchString(`^[0-9a-f]{40}$`, ref[at+1:]); !ok {
			t.Fatalf("workflow action %q is not pinned to a full commit SHA", ref)
		}
	}
}

func TestImageWorkflowContract(t *testing.T) {
	body := readRepoFile(t, filepath.Join(".github", "workflows", "image.yml"))
	for _, want := range []string{
		"contents: read",
		"packages: write",
		"verify-image-vcs.sh",
		"docker run --rm",
		"${{ github.sha }}",
		"${{ env.IMAGE }}:main",
		"linux/amd64",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("image workflow missing %q", want)
		}
	}
	// Push must be gated to pushes to main — PRs build only.
	if !strings.Contains(body, "github.event_name == 'push' && github.ref == 'refs/heads/main'") {
		t.Fatal("image workflow push step is not gated to push events on refs/heads/main")
	}
}

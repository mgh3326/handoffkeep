package tests

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// wrkMutant is one hk 1925 brief mutant, stated as the invariant it breaks:
// applied to a throwaway copy of the module, its named test must fail by an
// assertion (a "--- FAIL" with no panic and no build error).
type wrkMutant struct {
	name, file, old, new, pkg, test string
	db                              bool
}

var wrkMutants = []wrkMutant{
	{
		name: "check expected_seq before successful-key replay", file: "internal/wrk/engine.go",
		old: "\t// Successful-key replay precedes CAS, expiry, terminal state and rules.\n",
		new: "\tif req.ExpectedSeq != s.tail || req.ClaimSeq != s.claimSeq {\n\t\treturn WriteResult{}, Fail(SequenceConflict)\n\t}\n",
		pkg: "./internal/api", test: "^TestWrkReplayPrecedesCAS$",
	},
	{
		name: "let a host write a builder claim", file: "internal/wrk/engine.go",
		old: "} else if !e.Policy.Has(client, PermClaimWrite) {",
		new: "} else if !e.Policy.Has(client, PermClaimWrite) && !e.Policy.Has(client, PermReviewerClaimWrite) {",
		pkg: "./internal/api", test: "^TestWrkHostCannotMintBuilderClaim$",
	},
	{
		name: "let an extension be consumed before the cap", file: "internal/wrk/engine.go",
		old: "\t\tif ext != nil {\n\t\t\treturn nil, Fail(GrantBeforeCap)\n\t\t}\n",
		new: "",
		pkg: "./internal/api", test: "^TestWrkExtensionBeforeCapRefused$",
	},
	{
		name: "accept a verdict whose head differs from its open", file: "internal/wrk/engine.go",
		old: "if v.head != asserted || asserted != v.rd.head {",
		new: "if v.head != asserted {",
		pkg: "./internal/api", test: "^TestWrkVerdictHeadMustEqualOpen$",
	},
	{
		name: "let a void return a consumed grant's capacity", file: "internal/wrk/state.go",
		old: "\t\t\tif rd := s.rounds[no]; rd != nil {\n\t\t\t\trd.void = &rr\n\t\t\t}\n",
		new: "\t\t\tif rd := s.rounds[no]; rd != nil {\n\t\t\t\trd.void = &rr\n\t\t\t\tfor _, gs := range s.grants {\n\t\t\t\t\tif gs.consumedBy == rd.openSeq {\n\t\t\t\t\t\tgs.consumedBy = 0\n\t\t\t\t\t}\n\t\t\t\t}\n\t\t\t}\n",
		pkg: "./internal/api", test: "^TestWrkVoidNeverRefundsGrant$",
	},
	{
		name: "accept a joined with a pending open round", file: "internal/wrk/engine.go",
		old: "\trounds := s.nonvoidRounds()\n",
		new: "\trounds := s.nonvoidRounds()\n\t{\n\t\tvar kept []*round\n\t\tfor _, rd := range rounds {\n\t\t\tif rd.verdict != nil {\n\t\t\t\tkept = append(kept, rd)\n\t\t\t}\n\t\t}\n\t\trounds = kept\n\t}\n",
		pkg: "./internal/api", test: "^TestWrkJoinRefusesPendingRound$",
	},
	{
		name: "let a legacy relay job.completed through for a receipt_required job without a receipt", file: "internal/store/wrk_fence.go",
		old: "\tif x.WrkReceipt == nil {\n\t\treturn nil, ErrWrkReceiptRequired\n\t}\n",
		new: "\tif x.WrkReceipt == nil {\n\t\treturn nil, nil\n\t}\n",
		pkg: "./internal/api", test: "^TestO3PackStore$/^meta/", db: true,
	},
	{
		name: "follow one redirect in the wrk CLI client", file: "internal/remote/remote.go",
		old: "\thc := c.httpClientNoFollow()\n\tresp, err := hc.Do(r)\n\tif err != nil {\n\t\t// A failed dial",
		new: "\thc := c.httpClient()\n\tresp, err := hc.Do(r)\n\tif err != nil {\n\t\t// A failed dial",
		pkg: "./cmd/handoffkeep", test: "^TestWrkCLIRefusesEveryRedirect$",
	},
}

func copyModule(t *testing.T, root, dst string) {
	t.Helper()
	for _, name := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"cmd", "internal", "testdata", "docs"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			target := filepath.Join(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0o644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestWrkMutantsTurnRed applies each brief mutant to a copy of the module
// and requires its named test to turn RED. The store-backed mutant needs
// CI's PostgreSQL; in CI nothing may be skipped.
func TestWrkMutantsTurnRed(t *testing.T) {
	if testing.Short() {
		t.Skip("mutant runs compile the module once per mutant")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	ci := os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != ""
	for _, m := range wrkMutants {
		m := m
		t.Run(m.name, func(t *testing.T) {
			if m.db && os.Getenv("HANDOFFKEEP_TEST_DB_URL") == "" {
				if ci {
					t.Fatal("CI must provide HANDOFFKEEP_TEST_DB_URL for the store-backed mutant")
				}
				t.Skip("store-backed mutant runs in CI only")
			}
			work := t.TempDir()
			copyModule(t, root, work)
			path := filepath.Join(work, m.file)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(src), m.old); n != 1 {
				t.Fatalf("mutant anchor found %d times in %s", n, m.file)
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(src), m.old, m.new, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("go", "test", "-count=1", "-run", m.test, m.pkg)
			cmd.Dir = work
			out, err := cmd.CombinedOutput()
			text := string(out)
			if err == nil || !strings.Contains(text, "--- FAIL") || strings.Contains(text, "panic:") || strings.Contains(text, "[build failed]") || strings.Contains(text, "no tests to run") {
				t.Fatalf("mutant survived or broke the build (err=%v):\n%s", err, text)
			}
		})
	}
}

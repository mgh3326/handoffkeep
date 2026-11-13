package ciworkflow_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestHKFollowRecordsParse runs deploy/hk-follow's decision tests and parses
// every deploy record they write with the store's own deploy-record/v0 parser,
// the one the console deploy view and the hk.deploy event use. The shell fakes
// store bodies verbatim, so without this a record the readers reject (a
// string failed_step against store.DeployRecord.FailedStep *int, review on
// #349) would pass every shell test and vanish from the console as invalid.
func TestHKFollowRecordsParse(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "flock", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed: hk-follow targets Linux hosts", tool)
		}
	}
	out := t.TempDir()
	cmd := exec.Command("bash", filepath.Join("..", "..", "deploy", "hk-follow", "test-hk-follow.sh"))
	cmd.Env = append(os.Environ(), "HK_FOLLOW_RECORD_OUT="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("test-hk-follow.sh: %v\n%s", err, b)
	}
	keys, err := filepath.Glob(filepath.Join(out, "*", "*.key"))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) < 10 {
		t.Fatalf("only %d records written; the record path is not exercised", len(keys))
	}
	results := map[string]bool{}
	for _, kf := range keys {
		kb, err := os.ReadFile(kf)
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(strings.TrimSuffix(kf, ".key") + ".json")
		if err != nil {
			t.Fatal(err)
		}
		key := strings.TrimSpace(string(kb))
		parts := strings.Split(key, "/")
		if len(parts) != 3 || parts[0] != "deploy" {
			t.Errorf("%s: key %q is not deploy/<service>/<stamp>", kf, key)
			continue
		}
		rec, _, reasons := store.ClassifyDeployRecord(key, parts[1], string(body))
		if len(reasons) > 0 || rec == nil {
			t.Errorf("%s: record %s rejected %v:\n%s", kf, key, reasons, body)
			continue
		}
		results[rec.Result] = true
		if rec.Source != "hk-follow" {
			t.Errorf("%s: source %q, want hk-follow", key, rec.Source)
		}
		if (rec.Result == "success") != (rec.FailedStep == nil) {
			t.Errorf("%s: result %s with failed_step %v", key, rec.Result, rec.FailedStep)
		}
	}
	for _, r := range []string{"success", "failed", "rolled_back"} {
		if !results[r] {
			t.Errorf("no %s record parsed; the tests no longer cover it", r)
		}
	}
}

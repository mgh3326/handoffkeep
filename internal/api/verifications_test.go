package api

import (
	"fmt"
	"net/http"
	"testing"
)

const changeTestSHA2 = "2222222222222222222222222222222222222222"

func verificationBody(pr, head, level string) map[string]any {
	return map[string]any{"pr": pr, "head_sha": head, "level": level, "verifier": "lead", "family": "anthropic",
		"evidence": "https://github.com/example/app/actions/runs/1", "ci_result": "success", "source": "direct_merge"}
}

func TestChangesVerificationRoute(t *testing.T) {
	h, _ := changesServer(t)
	pr := fmt.Sprintf("https://github.com/%s/pull/1", changeRepoName("verify"))
	const path = "/v1/changes/verifications"

	status, body := changesCall(t, h, http.MethodPost, path, "peer-token", verificationBody(pr, changeTestSHA, "unit-test-verified"))
	if status != http.StatusCreated || body["recorded_by"] != "peer" || body["level"] != "unit-test-verified" || body["current"] != true {
		t.Fatalf("create: %d %v", status, body)
	}
	id := body["id"]
	if status, body = changesCall(t, h, http.MethodPost, path, "op-token", verificationBody(pr, changeTestSHA, "unit-test-verified")); status != http.StatusOK || body["id"] != id || body["recorded_by"] != "peer" {
		t.Fatalf("replay: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodPost, path, "peer-token", verificationBody(pr, changeTestSHA, "live-verified")); status != http.StatusConflict || body["error"] != "verification_exists" || len(body) != 1 {
		t.Fatalf("conflict: %d %v", status, body)
	}
	if status, _ = changesCall(t, h, http.MethodPost, path, "peer-token", verificationBody(pr, changeTestSHA2, "verifier-blocked")); status != http.StatusCreated {
		t.Fatalf("blocked: %d", status)
	}
	if status, body = changesCall(t, h, http.MethodPost, path, "peer-token", verificationBody(pr, changeTestSHA2, "live-verified")); status != http.StatusOK || body["level"] != "live-verified" {
		t.Fatalf("blocked replace: %d %v", status, body)
	}
	spoof := verificationBody(pr, changeTestSHA2, "live-verified")
	spoof["recorded_by"] = "operator"
	for name, b := range map[string]map[string]any{
		"ci-green":    verificationBody(pr, changeTestSHA, "ci-green"),
		"short head":  verificationBody(pr, "abc", "live-verified"),
		"not a PR":    verificationBody("https://example.com/x", changeTestSHA, "live-verified"),
		"recorded_by": spoof,
	} {
		if status, body = changesCall(t, h, http.MethodPost, path, "peer-token", b); status != http.StatusBadRequest || body["error"] != "invalid_context" {
			t.Errorf("%s: %d %v", name, status, body)
		}
	}
	if status, _ = changesCall(t, h, http.MethodPost, path, "wrong", verificationBody(pr, changeTestSHA, "live-verified")); status != http.StatusUnauthorized {
		t.Errorf("bad token: %d", status)
	}
}

func TestChangeVerificationCurrent(t *testing.T) {
	h, st := changesServer(t)
	x := relaneAPITask(t, st, relaneLane(t, "verify"), "verify target")
	pr := fmt.Sprintf("https://github.com/%s/pull/1", changeRepoName("current"))
	changeEvent(t, h, st, map[string]any{"source": "mergewake.pr", "source_ref": pr, "task_id": x.ID, "head_sha": changeTestSHA})
	if status, body := changesCall(t, h, http.MethodPost, "/v1/changes/verifications", "peer-token", verificationBody(pr, changeTestSHA, "live-verified")); status != http.StatusCreated {
		t.Fatalf("verdict A: %d %v", status, body)
	}
	read := func() map[string]any {
		t.Helper()
		status, body := changesCall(t, h, http.MethodGet, "/v1/changes?pr="+pr, "peer-token", nil)
		list, _ := body["changes"].([]any)
		if status != http.StatusOK || len(list) != 1 {
			t.Fatalf("read: %d %v", status, body)
		}
		return list[0].(map[string]any)
	}
	if c := read(); c["verification"] == nil || c["verification"].(map[string]any)["head_sha"] != changeTestSHA {
		t.Fatalf("on head A: %v", c)
	}

	changeEvent(t, h, st, map[string]any{"source": "mergewake.pr", "source_ref": pr, "task_id": x.ID, "head_sha": changeTestSHA2})
	c := read()
	vs, _ := c["verifications"].([]any)
	if c["verification"] != nil || len(vs) != 1 || vs[0].(map[string]any)["current"] != false || vs[0].(map[string]any)["head_sha"] != changeTestSHA {
		t.Fatalf("after head B: verification=%v verifications=%v", c["verification"], c["verifications"])
	}
	if _, ok := c["reviews"]; !ok {
		t.Fatalf("existing field reviews missing: %v", c)
	}

	if status, body := changesCall(t, h, http.MethodPost, "/v1/changes/verifications", "peer-token", verificationBody(pr, changeTestSHA2, "unit-test-verified")); status != http.StatusCreated {
		t.Fatalf("verdict B: %d %v", status, body)
	}
	c = read()
	vs, _ = c["verifications"].([]any)
	cur, _ := c["verification"].(map[string]any)
	if cur == nil || cur["head_sha"] != changeTestSHA2 || cur["current"] != true || len(vs) != 2 || vs[0].(map[string]any)["head_sha"] != changeTestSHA2 || vs[1].(map[string]any)["current"] != false {
		t.Fatalf("on head B: verification=%v verifications=%v", c["verification"], c["verifications"])
	}
}

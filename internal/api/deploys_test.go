package api

import (
	"fmt"
	"net/http"
	"testing"
)

func TestServicesRoutes(t *testing.T) {
	h, _ := changesServer(t)
	repo := changeRepoName("services")
	svc := map[string]any{"repo": repo, "path_patterns": []string{"internal/**"}, "deploy_mode": "manual", "targets": []string{"prod"}}
	for _, token := range []string{"op-token", "desk-token"} {
		if status, body := changesCall(t, h, http.MethodPut, "/v1/services/api-svc", token, svc); status != http.StatusOK || body["name"] != "api-svc" || body["repo"] != repo {
			t.Fatalf("%s put: %d %v", token, status, body)
		}
	}
	if status, body := changesCall(t, h, http.MethodPut, "/v1/services/api-svc", "peer-token", svc); status != http.StatusForbidden || body["error"] != "operator_required" {
		t.Fatalf("peer put: %d %v", status, body)
	}
	for name, bad := range map[string]map[string]any{
		"no pattern": {"repo": repo, "path_patterns": []string{}, "deploy_mode": "manual", "targets": []string{"prod"}},
		"no target":  {"repo": repo, "path_patterns": []string{"x/**"}, "deploy_mode": "auto", "targets": []string{}},
		"bad repo":   {"repo": "norepo", "path_patterns": []string{"x/**"}, "deploy_mode": "none"},
		"bad mode":   {"repo": repo, "path_patterns": []string{"x/**"}, "deploy_mode": "sometimes", "targets": []string{"prod"}},
		"unknown":    {"repo": repo, "path_patterns": []string{"x/**"}, "deploy_mode": "none", "extra": 1},
	} {
		if status, body := changesCall(t, h, http.MethodPut, "/v1/services/api-svc-bad", "op-token", bad); status != http.StatusBadRequest {
			t.Errorf("%s: %d %v", name, status, body)
		}
	}
	status, body := changesCall(t, h, http.MethodGet, "/v1/services", "peer-token", nil)
	if list, _ := body["services"].([]any); status != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["name"] != "api-svc" {
		t.Fatalf("list: %d %v", status, body)
	}
	status, body = changesCall(t, h, http.MethodGet, "/v1/services/api-svc/pending", "peer-token", nil)
	if list, ok := body["changes"].([]any); status != http.StatusOK || !ok || len(list) != 0 {
		t.Fatalf("pending: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodGet, "/v1/services/missing/pending", "peer-token", nil); status != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("unknown service: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodGet, "/v1/services/api-svc/pending?limit=0", "peer-token", nil); status != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("bad limit: %d %v", status, body)
	}
}

func TestDeploysRoutes(t *testing.T) {
	h, st := changesServer(t)
	x := relaneAPITask(t, st, relaneLane(t, "deploys"), "deploy target")
	repo := changeRepoName("deploys")
	pr := fmt.Sprintf("https://github.com/%s/pull/1", repo)
	changeEvent(t, h, st, map[string]any{"source": "mergewake.merge", "source_ref": pr, "task_id": x.ID})
	if status, body := changesCall(t, h, http.MethodPut, "/v1/services/deploy-svc", "op-token", map[string]any{"repo": repo, "path_patterns": []string{"internal/**"}, "deploy_mode": "auto", "targets": []string{"prod"}}); status != http.StatusOK {
		t.Fatalf("service: %d %v", status, body)
	}
	if status, body := changesCall(t, h, http.MethodGet, "/v1/services/deploy-svc/pending", "peer-token", nil); status != http.StatusOK || len(body["changes"].([]any)) != 1 {
		t.Fatalf("pending before: %d %v", status, body)
	}
	start := map[string]any{"attempt_id": "api-att-1", "service": "deploy-svc", "targets": []string{"prod"}, "action": "deploy", "deployed_ref": "abc", "source": "mergewake",
		"changes": []any{map[string]any{"pr": pr, "paths": []string{"internal/a.go"}}}}
	status, body := changesCall(t, h, http.MethodPost, "/v1/deploys", "peer-token", start)
	d, _ := body["deploy"].(map[string]any)
	if status != http.StatusCreated || body["created"] != true || d["state"] != "requested" {
		t.Fatalf("start: %d %v", status, body)
	}
	status, body = changesCall(t, h, http.MethodPost, "/v1/deploys", "peer-token", start)
	if again, _ := body["deploy"].(map[string]any); status != http.StatusOK || body["created"] != false || again["id"] != d["id"] {
		t.Fatalf("replay: %d %v", status, body)
	}
	start["deployed_ref"] = "def"
	if status, body = changesCall(t, h, http.MethodPost, "/v1/deploys", "peer-token", start); status != http.StatusConflict || body["error"] != "deploy_attempt_conflict" {
		t.Fatalf("conflict: %d %v", status, body)
	}
	start["service"], start["attempt_id"] = "missing", "api-att-2"
	if status, body = changesCall(t, h, http.MethodPost, "/v1/deploys", "peer-token", start); status != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("unknown service: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodPost, "/v1/deploys/api-att-1/finish", "peer-token", map[string]any{"state": "success"}); status != http.StatusOK || body["state"] != "success" {
		t.Fatalf("finish: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodPost, "/v1/deploys/api-att-1/finish", "peer-token", map[string]any{"state": "success"}); status != http.StatusOK {
		t.Fatalf("finish replay: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodPost, "/v1/deploys/api-att-1/finish", "peer-token", map[string]any{"state": "failed"}); status != http.StatusConflict || body["error"] != "deploy_attempt_finished" {
		t.Fatalf("finish other state: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodPost, "/v1/deploys/api-missing/finish", "peer-token", map[string]any{"state": "success"}); status != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("finish unknown: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodPost, "/v1/deploys/api-att-1/finish", "peer-token", map[string]any{"state": "running"}); status != http.StatusBadRequest {
		t.Fatalf("finish bad state: %d %v", status, body)
	}
	status, body = changesCall(t, h, http.MethodGet, fmt.Sprintf("/v1/tasks/%d", x.ID), "peer-token", nil)
	if status != http.StatusOK || body["state"] != "deployed" {
		t.Fatalf("task after deploy: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodGet, "/v1/services/deploy-svc/pending", "peer-token", nil); status != http.StatusOK || len(body["changes"].([]any)) != 0 {
		t.Fatalf("pending after: %d %v", status, body)
	}
}

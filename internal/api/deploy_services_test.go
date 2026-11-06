package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestDeployServicesRoutes(t *testing.T) {
	h, _ := changesServer(t)
	path := "/v1/deploy-services/zz-api"
	good := map[string]any{"repo": "owner/repo", "record_prefixes": []string{"deploy/zz-api/"}}
	if status, _ := changesCall(t, h, http.MethodPut, path, "peer-token", good); status != http.StatusForbidden {
		t.Fatalf("peer: %d", status)
	}
	if status, body := changesCall(t, h, http.MethodPut, path, "op-token", map[string]any{"repo": "bad", "record_prefixes": []string{"deploy/x/"}}); status != http.StatusBadRequest || body["error"] != "invalid_deploy_service" || !strings.Contains(body["reason"].(string), "repo") {
		t.Fatalf("validation: %d %v", status, body)
	}
	if status, body := changesCall(t, h, http.MethodPut, path, "op-token", good); status != http.StatusOK || body["active"] != true {
		t.Fatalf("put: %d %v", status, body)
	}
	status, body := changesCall(t, h, http.MethodGet, "/v1/deploy-services", "peer-token", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, body)
	}
	list := body["services"].([]any)
	for i := 1; i < len(list); i++ {
		if list[i-1].(map[string]any)["name"].(string) >= list[i].(map[string]any)["name"].(string) {
			t.Fatalf("order: %v", list)
		}
	}
	if status, body = changesCall(t, h, http.MethodPost, path+"/deactivate", "op-token", nil); status != http.StatusOK || body["active"] != false {
		t.Fatalf("deactivate: %d %v", status, body)
	}
}

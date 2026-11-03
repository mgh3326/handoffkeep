package main

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func clearLinearConfig(t *testing.T) {
	t.Helper()
	for _, name := range []string{"HANDOFFKEEP_LINEAR_ISSUE_LIMIT", "HANDOFFKEEP_LINEAR_ARCHIVE_HIGH_WATERMARK", "HANDOFFKEEP_LINEAR_ARCHIVE_LOW_WATERMARK", "HANDOFFKEEP_LINEAR_CAPACITY_SCOPE", "HANDOFFKEEP_LINEAR_WORK_BOARD", "HANDOFFKEEP_LINEAR_WORK_BOARD_BASE_URL", "HANDOFFKEEP_LINEAR_API_KEY", "HANDOFFKEEP_LINEAR_CLIENT_ID", "HANDOFFKEEP_LINEAR_CLIENT_SECRET", "HANDOFFKEEP_LINEAR_WEBHOOK_SECRET", "HANDOFFKEEP_LINEAR_OPERATOR_USER_ID", "HANDOFFKEEP_LINEAR_DECISION_STATES", "HANDOFFKEEP_LINEAR_DECISION_TEAM", "HANDOFFKEEP_DISCORD_DECISIONS_WEBHOOK_URL"} {
		t.Setenv(name, "")
	}
}

func TestWorkBoardConfigUsesSharedAppToken(t *testing.T) {
	clearLinearConfig(t)
	t.Setenv("HANDOFFKEEP_LINEAR_WEBHOOK_SECRET", "fake-signing-secret")
	t.Setenv("HANDOFFKEEP_LINEAR_WORK_BOARD", "on")
	t.Setenv("HANDOFFKEEP_LINEAR_CLIENT_ID", "test-app")
	t.Setenv("HANDOFFKEEP_LINEAR_CLIENT_SECRET", "test-secret")
	t.Setenv("HANDOFFKEEP_LINEAR_API_KEY", "test-personal-key")
	t.Setenv("HANDOFFKEEP_LINEAR_WORK_BOARD_BASE_URL", "https://console.example")
	st := &store.Store{}
	config := linearFromEnv(st)
	worker, ok := config.boardWorker.(api.LinearBoardWorker)
	if !ok || !st.LinearBoardEnabled() || config.webhook == nil {
		t.Fatalf("config=%+v", config)
	}
	client := worker.Linear.(api.LinearAPI)
	if client.App == nil || client.Key != "" || worker.TaskBaseURL != "https://console.example" {
		t.Fatalf("shared app client missing")
	}
	t.Setenv("HANDOFFKEEP_LINEAR_WEBHOOK_SECRET", "test-webhook")
	t.Setenv("HANDOFFKEEP_LINEAR_OPERATOR_USER_ID", "test-operator")
	config = linearFromEnv(st)
	boardClient := config.boardWorker.(api.LinearBoardWorker).Linear.(api.LinearAPI)
	decisionClient := config.decisions.Linear.(api.LinearAPI)
	if boardClient.App != decisionClient.App || boardClient.App == nil {
		t.Fatal("board and decisions do not share the token cache")
	}
}

func TestWorkBoardConfigOffAndNoCredential(t *testing.T) {
	clearLinearConfig(t)
	st := &store.Store{}
	if linearFromEnv(st).boardWorker != nil || st.LinearBoardEnabled() {
		t.Fatal("enabled by default")
	}
	t.Setenv("HANDOFFKEEP_LINEAR_WORK_BOARD", "on")
	if linearFromEnv(st).boardWorker != nil || st.LinearBoardEnabled() {
		t.Fatal("enabled without a credential")
	}
	t.Setenv("HANDOFFKEEP_LINEAR_API_KEY", "test-key")
	t.Setenv("HANDOFFKEEP_LINEAR_WEBHOOK_SECRET", "fake-signing-secret")
	if linearFromEnv(st).boardWorker == nil || !st.LinearBoardEnabled() {
		t.Fatal("key fallback disabled")
	}
}

func TestLinearAdmissionConfigRequiresVerifiedWorkspaceScope(t *testing.T) {
	clearLinearConfig(t)
	t.Setenv("HANDOFFKEEP_LINEAR_WEBHOOK_SECRET", "fake-signing-secret")
	t.Setenv("HANDOFFKEEP_LINEAR_WORK_BOARD", "on")
	t.Setenv("HANDOFFKEEP_LINEAR_API_KEY", "fake-key")
	st := &store.Store{}
	client := linearFromEnv(st).boardWorker.(api.LinearBoardWorker).Linear.(api.LinearAPI)
	if client.Admission != st || client.CapacityScopeVerified {
		t.Fatal("unverified scope admitted")
	}
	t.Setenv("HANDOFFKEEP_LINEAR_CAPACITY_SCOPE", "workspace")
	client = linearFromEnv(st).boardWorker.(api.LinearBoardWorker).Linear.(api.LinearAPI)
	if !client.CapacityScopeVerified {
		t.Fatal("verified scope ignored")
	}
}

func TestLinearCapacityConfigObservedLimitAndDowngrade(t *testing.T) {
	clearLinearConfig(t)
	t.Setenv("HANDOFFKEEP_LINEAR_WEBHOOK_SECRET", "fake-signing-secret")
	t.Setenv("HANDOFFKEEP_LINEAR_WORK_BOARD", "on")
	t.Setenv("HANDOFFKEEP_LINEAR_API_KEY", "fake-key")
	t.Setenv("HANDOFFKEEP_LINEAR_CAPACITY_SCOPE", "workspace")
	get := func() api.LinearAPI {
		return linearFromEnv(&store.Store{}).boardWorker.(api.LinearBoardWorker).Linear.(api.LinearAPI)
	}
	client := get()
	if client.CapacityPolicy.Limit() != 275 || client.CapacityPolicy.CreateCeiling() != 270 || !client.CapacityScopeVerified {
		t.Fatal(client.CapacityPolicy)
	}
	t.Setenv("HANDOFFKEEP_LINEAR_ISSUE_LIMIT", "250")
	client = get()
	if client.CapacityPolicy.Limit() != 250 || client.CapacityPolicy.CreateCeiling() != 245 || !client.CapacityScopeVerified {
		t.Fatal(client.CapacityPolicy)
	}
	t.Setenv("HANDOFFKEEP_LINEAR_ARCHIVE_HIGH_WATERMARK", "210")
	t.Setenv("HANDOFFKEEP_LINEAR_ARCHIVE_LOW_WATERMARK", "180")
	client = get()
	if client.CapacityPolicy.ArchiveHigh() != 210 || client.CapacityPolicy.ArchiveLow() != 180 {
		t.Fatal(client.CapacityPolicy)
	}
	t.Setenv("HANDOFFKEEP_LINEAR_ISSUE_LIMIT", "bad")
	if get().CapacityScopeVerified {
		t.Fatal("invalid limit enabled creation")
	}
}

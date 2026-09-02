package tests

import (
	"context"
	"os"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestPostgresMigrationAndCoreRoundTrip(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL integration tests")
	}
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := api.Service{Store: s}
	got, err := svc.Checkpoint(t.Context(), "test-client", store.Checkpoint{Session: "hk-test", Kind: "checkpoint", Title: "core", Body: "round trip"})
	if err != nil || got.CreatedBy != "test-client" {
		t.Fatalf("checkpoint=%+v err=%v", got, err)
	}
	items, err := svc.Recent(t.Context(), "hk-test", "", 3)
	if err != nil || len(items) == 0 {
		t.Fatalf("recent=%v err=%v", items, err)
	}
	if _, err := svc.Checkpoint(t.Context(), "test-client", store.Checkpoint{Session: "hk-test", Kind: "checkpoint", Title: "bad", Body: "sk-abcdefghijk"}); err == nil {
		t.Fatal("secret guard accepted content")
	}
}

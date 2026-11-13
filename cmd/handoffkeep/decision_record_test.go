package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestDecisionsRecordCLI(t *testing.T) {
	_, dbURL := testdb.Schema(t, "decision_record_cli")
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(api.Server{Service: api.Service{Store: st}, Tokens: api.Tokens{"operator": "cli-secret"}}.Handler())
	defer srv.Close()
	t.Setenv("HANDOFFKEEP_URL", srv.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "cli-secret")
	args := []string{"--quote", "exact operator words", "--ground", "hk:doc decision/one", "--lane", "design-lane", "--key", "cli-a1", "--decided-at", "2026-11-11T05:00:00Z", "--reading", "Keep the existing design."}
	var first strings.Builder
	if err := decisionRecordCmd(args, &first); err != nil {
		t.Fatal(err)
	}
	var out store.SessionDecisionResult
	if err := json.Unmarshal([]byte(first.String()), &out); err != nil {
		t.Fatal(err)
	}
	if out.Duplicate || out.Record.Answer != "exact operator words" || out.Record.Reading != "Keep the existing design." || out.Record.AnsweredBy != "operator" {
		t.Fatalf("CLI output=%+v", out)
	}
	var replay strings.Builder
	if err := decisionRecordCmd(args, &replay); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(replay.String()), &out); err != nil || !out.Duplicate {
		t.Fatalf("CLI replay=%+v err=%v", out, err)
	}
}

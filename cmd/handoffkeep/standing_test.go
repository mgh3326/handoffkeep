package main

import (
	"bytes"
	"encoding/json"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStandingCLIUsage(t *testing.T) {
	for _, args := range [][]string{{"standing", "put", "--lane", "x"}, {"standing", "put", "--lane", "x", "--project", "y"}} {
		if err := run(args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
func TestStandingCLIDeskDeployGrant(t *testing.T) {
	_, url := testdb.Schema(t, "standing_cli")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key := "decision/2026-10-29/desk-standing-deploy-grant"
	if _, _, err = st.PutDocument(t.Context(), store.Document{Key: key, Kind: "note", Body: "fake", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(api.Server{Service: api.Service{Store: st}, Tokens: api.Tokens{"operator-desk": "tok"}}.Handler())
	defer h.Close()
	var out bytes.Buffer
	args := []string{"standing", "put", "--lane", "operator-desk", "--topic", "deploy.grant", "--statement", "Desk may deploy fleet tooling with backup, rollback and verification; auto_trader production remains excluded.", "--source-doc", key, "--decided-by", "operator", "--decided-at", "2026-10-29T17:17:00+09:00", "--url", h.URL, "--token", "tok"}
	if err = run(args, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var order store.StandingOrder
	if err = json.Unmarshal(out.Bytes(), &order); err != nil || order.Scope != "operator-desk" || order.ScopeKind != "lane" || order.TopicKey != "deploy.grant" || order.SourceDoc != key || order.DecidedBy != "operator" {
		t.Fatalf("put: %s %v", out.String(), err)
	}
	out.Reset()
	if err = run([]string{"standing", "list", "--lane", "operator-desk", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Orders    []store.StandingOrder `json:"orders"`
		Truncated bool                  `json:"truncated"`
	}
	if err = json.Unmarshal(out.Bytes(), &list); err != nil || len(list.Orders) != 1 || list.Orders[0].ID != order.ID || list.Truncated {
		t.Fatalf("list: %s %v", out.String(), err)
	}
}

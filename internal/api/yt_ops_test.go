package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

func TestYTOpsResolveHTTPIsOperatorOnlyAndUsesProtocol(t *testing.T) {
	_, dbURL := testdb.Schema(t, "yt_ops_resolve_api")
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"operator": "op-token", "peer": "peer-token"}}.Handler())
	t.Cleanup(h.Close)

	now := time.Now().UTC()
	op, _, err := st.YTWriteLedger().Prepare(context.Background(), now, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend, Key: "resolve", PayloadSHA: "sha", Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "42", YTTargetID: "2-42", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := st.YTWriteLedger().Claim(context.Background(), now, op.ID); err != nil || outcome != ytwrite.ClaimClaimed {
		t.Fatalf("claim = %s, %v", outcome, err)
	}
	if _, err := st.YTWriteLedger().MarkUnknown(context.Background(), now, op.ID, now, ytwrite.ReasonTransport, "uncertain"); err != nil {
		t.Fatal(err)
	}
	post := func(token string, opID int64, body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/v1/yt/ops/%d/resolve", h.URL, opID), bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if status, body := post("peer-token", op.ID, map[string]any{"applied": true, "yt_id": "c-42", "by": "operator"}); status != http.StatusForbidden || body["error"] != "operator_required" {
		t.Fatalf("peer resolve = %d %v", status, body)
	}
	if status, body := post("op-token", op.ID, map[string]any{"applied": true, "yt_id": "c-42", "by": "operator"}); status != http.StatusOK || body["state"] != string(ytwrite.StateApplied) {
		t.Fatalf("operator resolve = %d %v", status, body)
	}
	resolved, err := st.YTWriteLedger().ByID(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != ytwrite.StateApplied || resolved.YTResultID != "c-42" {
		t.Fatalf("resolved op = %+v", resolved)
	}

	ledger := st.YTWriteLedger()
	owner, _, err := ledger.Prepare(context.Background(), now, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend, Key: "map-owner", PayloadSHA: "owner-sha", Payload: []byte(`{"body":"owner"}`), EntityKind: "task", EntityID: "43", YTTargetID: "2-43", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := ledger.Claim(context.Background(), now, owner.ID); err != nil || outcome != ytwrite.ClaimClaimed {
		t.Fatalf("claim map owner = %s, %v", outcome, err)
	}
	if _, err := ledger.Apply(context.Background(), now, owner.ID, "c-conflict", []ytwrite.MapRow{{EntityKind: "comment", HKKey: fmt.Sprintf("comment/op:%d", owner.ID), YTID: "c-conflict"}}); err != nil {
		t.Fatal(err)
	}
	conflict, _, err := ledger.Prepare(context.Background(), now, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend, Key: "conflict", PayloadSHA: "conflict-sha", Payload: []byte(`{"body":"conflict"}`), EntityKind: "task", EntityID: "44", YTTargetID: "2-44", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := ledger.Claim(context.Background(), now, conflict.ID); err != nil || outcome != ytwrite.ClaimClaimed {
		t.Fatalf("claim conflict op = %s, %v", outcome, err)
	}
	if _, err := ledger.MarkUnknown(context.Background(), now, conflict.ID, now, ytwrite.ReasonTransport, "uncertain"); err != nil {
		t.Fatal(err)
	}
	if status, body := post("op-token", conflict.ID, map[string]any{"applied": true, "yt_id": "c-conflict", "by": "operator"}); status != http.StatusConflict || body["error"] != "ytwrite_conflicting_result" {
		t.Fatalf("conflicting resolution = %d %v", status, body)
	}
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// GET /v1/yt/ops shows last_error to the operator's client only.
func TestYTOpsListLastErrorOnlyForOperator(t *testing.T) {
	_, dbURL := testdb.Schema(t, "yt_ops_view")
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"operator": "op-token", "peer": "peer-token"}}.Handler())
	t.Cleanup(h.Close)

	ctx := context.Background()
	now := time.Now().UTC()
	l := st.YTWriteLedger()
	op, _, err := l.Prepare(ctx, now, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend, Key: "view-detail", PayloadSHA: "sha",
		Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "51", YTTargetID: "2-51", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := l.Claim(ctx, now, op.ID); err != nil || outcome != ytwrite.ClaimClaimed {
		t.Fatalf("claim = %s, %v", outcome, err)
	}
	if _, err := l.MarkUnknown(ctx, now, op.ID, now, ytwrite.ReasonTransport, "read tcp: connection reset"); err != nil {
		t.Fatal(err)
	}

	list := func(token string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.URL+"/v1/yt/ops?state=all&task=51", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("list = %d %v, %v", resp.StatusCode, out, err)
		}
		ops, _ := out["ops"].([]any)
		if len(ops) != 1 {
			t.Fatalf("ops = %v", out)
		}
		return ops[0].(map[string]any)
	}
	if got := list("op-token")["last_error"]; got != "read tcp: connection reset" {
		t.Fatalf("operator last_error = %v", got)
	}
	if _, ok := list("peer-token")["last_error"]; ok {
		t.Fatal("peer token sees last_error")
	}
}

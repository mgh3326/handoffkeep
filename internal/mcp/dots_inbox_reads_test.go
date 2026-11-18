package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// Each dots_inbox call records one row: the principal, the listed count and
// the result (hk 4924).
func TestDotsInboxCallRecordsRead(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", "off")
	st := newOperatorStore(t)
	ctx := context.Background()
	if _, err := st.CreateDotsRequest(ctx, "caller", store.DotsRequestInput{Title: "Research", Body: "Investigate"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	for i := 0; i < 2; i++ {
		if _, failed := toolText(t, session, "dots_inbox", map[string]any{}); failed {
			t.Fatal("dots_inbox failed")
		}
	}
	reads, err := st.ListDotsInboxReads(ctx, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 2 {
		t.Fatalf("reads = %+v", reads)
	}
	for _, x := range reads {
		if x.Principal != "tester" || x.Listed != 1 || x.Result != "ok" || x.ReadAt.IsZero() {
			t.Fatalf("read = %+v", x)
		}
	}
}

// A failed dots_inbox call records its error too.
func TestDotsInboxErrorRecordsRead(t *testing.T) {
	st, pool := newOperatorStoreWithPool(t)
	ctx := context.Background()
	task, err := st.CreateDotsRequest(ctx, "caller", store.DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	key, _, _ := strings.Cut(task.BodyDoc, "#")
	if _, err := pool.Exec(ctx, `DELETE FROM documents WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
	o := operatorTools{service: api.Service{Store: st}, principal: principal{Actor: "operator:x@example.com", Channel: ChannelChatGPT, Access: true}}
	if _, _, err := o.dotsInbox(ctx, nil, dotsInboxInput{}); err == nil {
		t.Fatal("dots_inbox succeeded without its body document")
	}
	reads, err := st.ListDotsInboxReads(ctx, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 1 || reads[0].Principal != "operator:x@example.com" || reads[0].Listed != 0 || !strings.Contains(reads[0].Result, "dots request body not found") {
		t.Fatalf("reads = %+v", reads)
	}
}

// The Access channel records the operator identity, never the assertion.
func TestDotsInboxAccessRecordsOperatorPrincipal(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", "off")
	st := newOperatorStore(t)
	ctx := context.Background()
	access := newAccessFixture(t)
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, nil, access.verifier))
	defer srv.Close()
	assertion := access.token(t, jwt.MapClaims{"email": "operator@example.com"})
	session := connectMCP(t, srv.URL, map[string]string{"Cf-Access-Jwt-Assertion": assertion})
	if _, failed := toolText(t, session, "dots_inbox", map[string]any{}); failed {
		t.Fatal("dots_inbox failed")
	}
	reads, err := st.ListDotsInboxReads(ctx, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 1 || reads[0].Principal != "operator:operator@example.com" || reads[0].Result != "ok" {
		t.Fatalf("reads = %+v", reads)
	}
}

package ytwrite_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

func memory(*testing.T) ytwrite.Ledger { return ytwritetest.NewMemLedger() }

func TestProtocolOverMemoryLedger(t *testing.T) { ytwritetest.RunProtocol(t, memory) }

func TestIssueProtocolOverMemoryLedger(t *testing.T) { ytwritetest.RunIssueProtocol(t, memory) }

func TestMemoryLedgerContract(t *testing.T) { ytwritetest.RunLedger(t, memory) }

// StaleAfter must exceed DispatchTimeout: a dispatch still inside its call
// would otherwise be recovered as lost while it may yet send, and sent again.
func TestNewRefusesStaleAfterWithinDispatchTimeout(t *testing.T) {
	l := ytwritetest.NewMemLedger()
	remote := ytwritetest.NewFake()
	for _, tc := range []struct {
		name            string
		dispatch, stale time.Duration
	}{
		{"equal", 3 * time.Second, 3 * time.Second},
		{"shorter", 3 * time.Second, time.Second},
	} {
		if _, err := ytwrite.New(ytwrite.Config{Ledger: l, Remote: remote, DispatchTimeout: tc.dispatch, StaleAfter: tc.stale}); err == nil {
			t.Errorf("%s: New accepted StaleAfter %s with DispatchTimeout %s", tc.name, tc.stale, tc.dispatch)
		}
	}
	if _, err := ytwrite.New(ytwrite.Config{Ledger: l, Remote: remote, DispatchTimeout: 3 * time.Second, StaleAfter: 4 * time.Second}); err != nil {
		t.Fatalf("a StaleAfter above DispatchTimeout was refused: %v", err)
	}
}

func TestProvenanceLineFormat(t *testing.T) {
	at := time.Date(2026, 11, 9, 3, 4, 5, 0, time.FixedZone("KST", 9*3600))
	line := ytwrite.ProvenanceLine("worker", at, 42)
	if want := "작성: worker · 2026-11-08T18:04:05Z · hk op 42"; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	actor, got, id, ok := ytwrite.ParseProvenance(line)
	if !ok || actor != "worker" || id != 42 || !got.Equal(at) {
		t.Fatalf("parsed %q %v %d %v", actor, got, id, ok)
	}
	for _, bad := range []string{"", "작성: a · 2026-11-08 · hk op 1", "작성: a · 2026-11-08T18:04:05Z · hk op 0", "작성: a · 2026-11-08T18:04:05Z · hk op 1 extra", "written by a"} {
		if _, _, _, ok := ytwrite.ParseProvenance(bad); ok {
			t.Errorf("ParseProvenance(%q) accepted", bad)
		}
	}
}

func TestRenderCommentIsDeterministic(t *testing.T) {
	op := ytwrite.Op{ID: 7, Actor: "operator-desk", Payload: []byte(`{"body":"line one\nline two"}`), CreatedAt: time.Date(2026, 11, 9, 1, 2, 3, 999, time.UTC)}
	a, err := ytwrite.RenderComment(op)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ytwrite.RenderComment(op)
	want := "작성: operator-desk · 2026-11-09T01:02:03Z · hk op 7\n\nline one\nline two\n\n<!-- hk-op:7 -->"
	if a != want || b != want {
		t.Fatalf("rendered %q, want %q", a, want)
	}
	actor, at, id, ok := ytwrite.ParseProvenance(strings.SplitN(a, "\n", 2)[0])
	if !ok || actor != op.Actor || id != op.ID || !at.Equal(op.CreatedAt.Truncate(time.Second)) || at.Location() != time.UTC {
		t.Fatalf("parsed provenance: %q %v %d %v", actor, at, id, ok)
	}
	if !strings.HasSuffix(a, ytwrite.OpMarker(7)) || strings.Contains(a, ytwrite.OpMarker(70)) {
		t.Fatal("marker is not the exact op marker")
	}
}

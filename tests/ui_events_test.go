package tests

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ui"
)

// sseBlock is one blank-line-terminated block of the /ui/events stream.
type sseBlock struct {
	comment, retry, event string
	marks                 store.EventWatermarks
}

// readSSE parses the stream into blocks until it ends.
func readSSE(t *testing.T, body *bufio.Reader, out chan<- sseBlock) {
	var b sseBlock
	for {
		line, err := body.ReadString('\n')
		if err != nil {
			close(out)
			return
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			out <- b
			b = sseBlock{}
		case strings.HasPrefix(line, ":"):
			b.comment = strings.TrimSpace(line[1:])
		case strings.HasPrefix(line, "retry: "):
			b.retry = strings.TrimPrefix(line, "retry: ")
		case strings.HasPrefix(line, "event: "):
			b.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &b.marks); err != nil {
				t.Errorf("delta data %q: %v", line, err)
			}
		default:
			t.Errorf("unexpected SSE line %q", line)
		}
	}
}

// A document or task comment write moves only the change feed watermark; the
// stream must still emit delta for it. An idle stream carries ": ping"
// comments and nothing else.
func TestUIEventsSyncWatermarkAndHeartbeat(t *testing.T) {
	// Own schema: the stream compares global MAX ids, so writes from other
	// packages in a shared schema would emit deltas this test did not cause.
	_, url := testdb.Schema(t, "ui_events")
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	task, err := s.CreateTask(t.Context(), store.Task{Lane: "lane-a", Title: "sse", Kind: "implement", CreatedBy: "test-node", Project: projectRef("experiment")})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newUIJWTFixture(t)
	h := newUITestServerConfig(t, fixture, ui.Config{Store: s, PollInterval: 10 * time.Millisecond, SSEHeartbeat: 30 * time.Millisecond})
	defer h.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.URL+"/ui/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cf-Access-Jwt-Assertion", fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil))
	response, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	blocks := make(chan sseBlock, 64)
	go readSSE(t, bufio.NewReader(response.Body), blocks)
	next := func() sseBlock {
		t.Helper()
		select {
		case b, ok := <-blocks:
			if !ok {
				t.Fatal("stream ended")
			}
			return b
		case <-time.After(2 * time.Second):
			t.Fatal("no SSE block within 2s")
		}
		return sseBlock{}
	}
	first := next()
	if first.retry != "5000" || first.event != "delta" || first.marks.SyncMaxID == 0 {
		t.Fatalf("initial block=%+v", first)
	}
	// nextDelta skips heartbeats, counting them, and returns the next delta.
	pings := 0
	nextDelta := func() sseBlock {
		t.Helper()
		for {
			b := next()
			if b.comment == "ping" && b.event == "" {
				pings++
				continue
			}
			if b.event != "delta" {
				t.Fatalf("unexpected block %+v", b)
			}
			return b
		}
	}
	idle := time.After(150 * time.Millisecond)
	for waiting := true; waiting; {
		select {
		case b := <-blocks:
			if b.comment != "ping" || b.event != "" {
				t.Fatalf("idle stream sent %+v", b)
			}
			pings++
		case <-idle:
			waiting = false
		}
	}
	if pings == 0 {
		t.Fatal("idle stream sent no heartbeat")
	}

	if _, err := s.CreateTaskComment(t.Context(), task.ID, "node", "comment moves only the sync feed"); err != nil {
		t.Fatal(err)
	}
	afterComment := nextDelta()
	if afterComment.marks.SyncMaxID <= first.marks.SyncMaxID || afterComment.marks.TaskEventMaxID != first.marks.TaskEventMaxID || afterComment.marks.RelayMaxID != first.marks.RelayMaxID {
		t.Fatalf("comment delta=%+v initial=%+v", afterComment.marks, first.marks)
	}
	if _, _, err := s.PutDocument(t.Context(), store.Document{Key: "notes/sse", Kind: "note", Body: "body", CreatedBy: "test-node"}); err != nil {
		t.Fatal(err)
	}
	afterDocument := nextDelta()
	if afterDocument.marks.SyncMaxID <= afterComment.marks.SyncMaxID || afterDocument.marks.TaskEventMaxID != first.marks.TaskEventMaxID {
		t.Fatalf("document delta=%+v previous=%+v", afterDocument.marks, afterComment.marks)
	}
}

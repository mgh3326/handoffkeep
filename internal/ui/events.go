package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// sseRetry is the reconnect delay the stream asks EventSource to use after a
// dropped connection (a deploy restart, a proxy timeout).
const sseRetry = 5 * time.Second

// events streams a delta whenever a watermark moves (docs/ui.md "Live
// refresh"). An idle stream gets a ": ping" comment every sseHeartbeat so
// proxies that close silent connections (Cloudflare) keep it open.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	marks, err := h.store.EventWatermarks(r.Context())
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprintf(w, "retry: %d\n", sseRetry.Milliseconds())
	writeDelta(w, marks)

	poll := time.NewTicker(h.pollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(h.sseHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flush(w)
		case <-poll.C:
			next, err := h.store.EventWatermarks(r.Context())
			if err != nil {
				continue
			}
			if next != marks {
				marks = next
				writeDelta(w, marks)
			}
		}
	}
}

func writeDelta(w http.ResponseWriter, marks store.EventWatermarks) {
	payload, _ := json.Marshal(marks)
	fmt.Fprintf(w, "event: delta\ndata: %s\n\n", payload)
	flush(w)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

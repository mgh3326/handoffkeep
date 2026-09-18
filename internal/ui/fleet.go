package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ConsoleCSP is applied to the React fleet page and every /ui/api/* response.
// It structurally blocks browser-to-hub connections and runtime CDN loads.
const ConsoleCSP = "default-src 'self'; connect-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

const fleetModelUncollected = "미수집"
const fleetFailCacheTTL = 2 * time.Second

func setConsoleCSP(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", ConsoleCSP)
}

type fleetResponse struct {
	FetchedAt string      `json:"fetched_at"`
	Upstream  string      `json:"upstream"`
	Nodes     []fleetNode `json:"nodes"`
}

type fleetNode struct {
	MachineID      string         `json:"machine_id"`
	State          string         `json:"state"`
	LastPing       *int64         `json:"last_ping"`
	Sessions       []fleetSession `json:"sessions"`
	SnapshotStatus string         `json:"snapshot_status"`
	DisplayState   string         `json:"display_state"`
	Truncated      bool           `json:"truncated"`
	ReceivedAt     string         `json:"received_at"`
	Stale          bool           `json:"stale"`
}

type fleetSession struct {
	PaneID           string `json:"pane_id"`
	WorkspaceID      string `json:"workspace_id"`
	Label            string `json:"label"`
	Status           string `json:"status"`
	InteractiveReady *bool  `json:"interactive_ready,omitempty"`
	Model            string `json:"model"`
}

func (h *Handler) fleetAPI(w http.ResponseWriter, r *http.Request) {
	h.writeFleet(w, h.hub.fleet(r.Context()))
}

func (h *Handler) writeFleet(w http.ResponseWriter, response fleetResponse) {
	if response.Nodes == nil {
		response.Nodes = []fleetNode{}
	}
	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func (p *hubProxy) fleet(ctx context.Context) fleetResponse {
	if !p.configured() {
		return fleetResponse{Upstream: "unconfigured", Nodes: []fleetNode{}}
	}

	p.fleetMu.Lock()
	if p.fleetReady && time.Since(p.fleetAt) < p.cacheTTL {
		view := cloneFleet(p.fleetCached)
		p.fleetMu.Unlock()
		return view
	}
	if p.failReady && time.Since(p.failAt) < fleetFailCacheTTL {
		view := cloneFleet(p.failCached)
		p.fleetMu.Unlock()
		return view
	}
	if p.fleetWait != nil {
		wait := p.fleetWait
		p.fleetMu.Unlock()
		<-wait
		p.fleetMu.Lock()
		view := cloneFleet(p.lastResult)
		p.fleetMu.Unlock()
		return view
	}
	wait := make(chan struct{})
	p.fleetWait = wait
	p.fleetMu.Unlock()

	view := p.loadFleet()

	p.fleetMu.Lock()
	p.lastResult = cloneFleet(view)
	if view.Upstream == "ok" {
		p.fleetCached = cloneFleet(view)
		p.fleetAt = time.Now()
		p.fleetReady = true
		p.failReady = false
	} else {
		p.failCached = cloneFleet(view)
		p.failAt = time.Now()
		p.failReady = true
	}
	p.fleetWait = nil
	close(wait)
	p.fleetMu.Unlock()
	return view
}

func (p *hubProxy) loadFleet() fleetResponse {
	var wrapped struct {
		Nodes []hubNode `json:"nodes"`
	}
	status, err := p.getJSON(context.Background(), "/v1/nodes", &wrapped)
	if err == nil {
		view := fleetResponse{
			FetchedAt: time.Now().UTC().Format(time.RFC3339),
			Upstream:  "ok",
			Nodes:     projectFleetNodes(wrapped.Nodes),
		}
		copied := cloneFleet(view)
		p.fleetMu.Lock()
		p.lastSuccess = &copied
		p.fleetMu.Unlock()
		return view
	}

	view := fleetResponse{
		Upstream: classifyUpstream(status, err),
		Nodes:    []fleetNode{},
	}
	p.fleetMu.Lock()
	if p.lastSuccess != nil {
		view.Nodes = cloneFleet(*p.lastSuccess).Nodes
		view.FetchedAt = p.lastSuccess.FetchedAt
	}
	p.fleetMu.Unlock()
	return view
}

func projectFleetNodes(nodes []hubNode) []fleetNode {
	out := make([]fleetNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, projectFleetNode(node))
	}
	return out
}

func projectFleetNode(node hubNode) fleetNode {
	view := fleetNode{
		MachineID:      node.MachineID,
		State:          node.State,
		LastPing:       node.LastPingMS,
		Sessions:       []fleetSession{},
		SnapshotStatus: "unavailable",
		DisplayState:   "missing",
	}
	if node.SessionSnapshot == nil {
		return view
	}
	snap := node.SessionSnapshot
	// Empty sessions with snapshot_status=ok stay ok; they are not unavailable.
	view.SnapshotStatus = snap.SnapshotStatus
	view.Truncated = snap.Truncated
	view.ReceivedAt = snap.ReceivedAt
	view.Stale = snap.Stale
	view.Sessions = projectFleetSessions(snap.Sessions)
	view.DisplayState = classifyDisplayState(snap.SnapshotStatus, len(view.Sessions))
	return view
}

func classifyDisplayState(status string, n int) string {
	switch status {
	case "ok":
		if n == 0 {
			return "empty"
		}
		return "sessions"
	case "unavailable":
		return "unavailable"
	default:
		return "unknown"
	}
}

func projectFleetSessions(sessions []hubSession) []fleetSession {
	out := make([]fleetSession, 0, len(sessions))
	for _, session := range sessions {
		item := fleetSession{
			PaneID:      session.PaneID,
			WorkspaceID: session.WorkspaceID,
			Label:       session.Label,
			Status:      session.Status,
			Model:       fleetModelUncollected,
		}
		if session.InteractiveReady != nil {
			ready := *session.InteractiveReady
			item.InteractiveReady = &ready
		}
		out = append(out, item)
	}
	return out
}

func classifyUpstream(status int, err error) string {
	if err == nil {
		return "ok"
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "auth_failed"
	}
	if status != 0 {
		return "http_error"
	}
	if isTimeoutErr(err) {
		return "timeout"
	}
	return "http_error"
}

func isTimeoutErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr) && urlErr.Timeout()
}

func cloneFleet(src fleetResponse) fleetResponse {
	dst := src
	dst.Nodes = make([]fleetNode, len(src.Nodes))
	for i, node := range src.Nodes {
		copied := node
		copied.Sessions = make([]fleetSession, len(node.Sessions))
		for j, session := range node.Sessions {
			copied.Sessions[j] = session
			if session.InteractiveReady != nil {
				ready := *session.InteractiveReady
				copied.Sessions[j].InteractiveReady = &ready
			}
		}
		if node.LastPing != nil {
			ping := *node.LastPing
			copied.LastPing = &ping
		}
		dst.Nodes[i] = copied
	}
	return dst
}

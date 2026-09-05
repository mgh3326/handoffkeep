package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type hubProxy struct {
	url    string
	token  string
	client *http.Client

	mu       sync.Mutex
	snapshot *hubSnapshot
}

type hubStatusError int

func (e hubStatusError) Error() string { return fmt.Sprintf("hub status %d", e) }

type hubSnapshot struct {
	view        hubView
	collectedAt time.Time
}

type hubView struct {
	Status          string
	CollectedAt     time.Time
	Nodes           []hubNodeView
	Jobs            []hubJobView
	JobsUnsupported bool
	JobsUnavailable bool
}

type hubNodeView struct {
	MachineID          string
	State              string
	Accepting          string
	AcceptingOverride  string
	NotAcceptingReason string
	AcceptingAlert     bool
	LastPing           string
	Version            string
	FreePct            string
	CompressedMB       string
	SwapUsedMB         string
	FreePctAlert       bool
	SwapUsedAlert      bool
}

type hubJobView struct {
	Machine   string
	JobID     string
	OwnerLane string
	Pane      string
	Tier      string
	StartedAt string
}

func newHubProxy(rawURL, token string, client *http.Client) *hubProxy {
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	return &hubProxy{url: strings.TrimRight(strings.TrimSpace(rawURL), "/"), token: strings.TrimSpace(token), client: client}
}

func (p *hubProxy) configured() bool { return p.url != "" && p.token != "" }

func (p *hubProxy) current(ctx context.Context) hubView {
	if !p.configured() {
		return hubView{Status: "unconfigured"}
	}
	nodes, err := p.nodes(ctx)
	if err == nil {
		view := hubView{Status: "current", Nodes: nodeViews(nodes)}
		jobs, status, jobsErr := p.jobs(ctx)
		switch {
		case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
			view.JobsUnsupported = true
		case jobsErr != nil:
			view.JobsUnavailable = true
		default:
			view.Jobs = jobViews(jobs)
		}
		now := time.Now().UTC()
		view.CollectedAt = now
		p.mu.Lock()
		p.snapshot = &hubSnapshot{view: view, collectedAt: now}
		p.mu.Unlock()
		return view
	}
	p.mu.Lock()
	snapshot := p.snapshot
	p.mu.Unlock()
	if snapshot == nil {
		return hubView{Status: "unavailable"}
	}
	view := snapshot.view
	view.Status = "stale"
	view.CollectedAt = snapshot.collectedAt
	return view
}

func (p *hubProxy) nodes(parent context.Context) ([]hubNode, error) {
	var response struct {
		Nodes []hubNode `json:"nodes"`
	}
	_, err := p.getJSON(parent, "/v1/nodes", &response)
	return response.Nodes, err
}

func (p *hubProxy) jobs(parent context.Context) ([]hubJob, int, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url+"/v1/jobs", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	response, err := p.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, response.StatusCode, fmt.Errorf("hub jobs status %d", response.StatusCode)
	}
	var wrapped struct {
		Jobs []hubJob `json:"jobs"`
	}
	if err := json.NewDecoder(response.Body).Decode(&wrapped); err != nil {
		return nil, response.StatusCode, err
	}
	return wrapped.Jobs, response.StatusCode, nil
}

func (p *hubProxy) getJSON(parent context.Context, endpoint string, target any) (int, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url+endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	response, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, fmt.Errorf("hub nodes status %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return response.StatusCode, err
	}
	return response.StatusCode, nil
}

// emitLaneEvent uses the producer-less hub ingress endpoint. It deliberately
// returns only a duplicate flag and sanitized status error: callers never need
// the hub token or a remote response body in a browser response.
func (p *hubProxy) emitLaneEvent(parent context.Context, lane, eventID, text string) (bool, error) {
	if !p.configured() {
		return false, errors.New("hub is not configured")
	}
	payload, err := json.Marshal(struct {
		Kind    string `json:"kind"`
		Lane    string `json:"lane"`
		EventID string `json:"event_id"`
		Text    string `json:"text"`
		Label   string `json:"label"`
	}{Kind: "lane.event", Lane: lane, EventID: eventID, Text: text, Label: "operator-web"})
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/v1/relay/events", bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)
	response, err := p.client.Do(req)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusCreated:
		return false, nil
	case http.StatusConflict:
		return true, nil
	default:
		return false, hubStatusError(response.StatusCode)
	}
}

type hubNode struct {
	MachineID          string          `json:"machine_id"`
	State              string          `json:"state"`
	Accepting          bool            `json:"accepting"`
	AcceptingEffective bool            `json:"accepting_effective"`
	AcceptingOverride  string          `json:"accepting_override"`
	AlertClass         string          `json:"alert_class"`
	LastPingMS         *int64          `json:"last_ping_ms"`
	Memory             *hubMemory      `json:"memory"`
	RemoteMeta         json.RawMessage `json:"remote_meta"`
}

type hubMemory struct {
	FreePct      *float64 `json:"free_pct"`
	CompressedMB *float64 `json:"compressed_mb"`
	SwapUsedMB   *float64 `json:"swap_used_mb"`
	PSISomeAvg10 *float64 `json:"psi_some_avg10"`
	Source       string   `json:"source"`
}

type hubJob struct {
	Machine   string `json:"machine"`
	JobID     string `json:"job_id"`
	OwnerLane string `json:"owner_lane"`
	Pane      string `json:"pane"`
	Tier      string `json:"tier"`
	StartedAt string `json:"started_at"`
}

func nodeViews(nodes []hubNode) []hubNodeView {
	out := make([]hubNodeView, 0, len(nodes))
	for _, node := range nodes {
		view := hubNodeView{
			MachineID:         node.MachineID,
			State:             node.State,
			Accepting:         fmt.Sprint(node.AcceptingEffective),
			AcceptingOverride: node.AcceptingOverride,
			AcceptingAlert:    !node.AcceptingEffective,
			LastPing:          intValue(node.LastPingMS),
			FreePct:           "미측정",
			CompressedMB:      "미측정",
			SwapUsedMB:        "미측정",
		}
		if node.AcceptingEffective != node.Accepting && node.AcceptingOverride == "" {
			view.AcceptingOverride = fmt.Sprintf("reported %t", node.Accepting)
		}
		if !node.AcceptingEffective {
			view.NotAcceptingReason = node.AlertClass
			if view.NotAcceptingReason == "" {
				view.NotAcceptingReason = node.AcceptingOverride
			}
		}
		var meta map[string]json.RawMessage
		if json.Unmarshal(node.RemoteMeta, &meta) == nil {
			_ = json.Unmarshal(meta["version"], &view.Version)
		}
		if node.Memory != nil {
			view.FreePct = floatValue(node.Memory.FreePct)
			view.CompressedMB = floatValue(node.Memory.CompressedMB)
			view.SwapUsedMB = floatValue(node.Memory.SwapUsedMB)
			view.FreePctAlert = node.Memory.FreePct != nil && *node.Memory.FreePct < 30
			view.SwapUsedAlert = node.Memory.SwapUsedMB != nil && *node.Memory.SwapUsedMB > 1536
		}
		out = append(out, view)
	}
	return out
}

func jobViews(jobs []hubJob) []hubJobView {
	out := make([]hubJobView, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, hubJobView{Machine: job.Machine, JobID: job.JobID, OwnerLane: job.OwnerLane, Pane: job.Pane, Tier: job.Tier, StartedAt: job.StartedAt})
	}
	return out
}

func intValue(value *int64) string {
	if value == nil {
		return "미측정"
	}
	return fmt.Sprint(*value)
}

func floatValue(value *float64) string {
	if value == nil {
		return "미측정"
	}
	return fmt.Sprintf("%g", *value)
}

package ui

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const glanceBodyLimit = 256 * 1024

var acceptingMachineRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

var glanceStates = []string{"backlog", "claimed", "in_progress", "verifying", "join", "needs_decision", "hold"}

type glanceHub struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

type glanceTask struct {
	ID        int64     `json:"id"`
	Lane      string    `json:"lane"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	ClaimedBy string    `json:"claimed_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

type glanceTasks struct {
	ByState          map[string]int `json:"by_state"`
	DecisionsPending int            `json:"decisions_pending"`
	Active           []glanceTask   `json:"active"`
}

type glanceResponse struct {
	GeneratedAt  time.Time                    `json:"generated_at"`
	Hub          glanceHub                    `json:"hub"`
	Nodes        []map[string]json.RawMessage `json:"nodes"`
	Lanes        []json.RawMessage            `json:"lanes"`
	Jobs         []json.RawMessage            `json:"jobs"`
	Tasks        glanceTasks                  `json:"tasks"`
	ConsolePaths map[string]string            `json:"console_paths"`
	Truncated    bool                         `json:"truncated,omitempty"`
}

// serveAPI is deliberately reached only after ServeHTTP selected a verified
// email or service identity. Service principals have no browser cookie path.
func (h *Handler) serveAPI(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/ui/api/glance":
		h.glance(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/ui/api/nodes/"):
		h.setAccepting(w, r, identity)
	default:
		if r.Method == http.MethodPost {
			h.audit(apiIdentity(identity), writeAction(r.URL.Path), "-", "-", "invalid")
		}
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) glance(w http.ResponseWriter, r *http.Request) {
	counts, active, err := h.store.GlanceTasks(r.Context())
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	laneDecisions, err := h.store.ListOpenLaneDecisions(r.Context(), 1000)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	byState := make(map[string]int, len(glanceStates))
	for _, state := range glanceStates {
		byState[state] = counts[state]
	}
	response := glanceResponse{
		GeneratedAt: time.Now().UTC(),
		Hub:         glanceHub{OK: true},
		Nodes:       []map[string]json.RawMessage{},
		Lanes:       []json.RawMessage{},
		Jobs:        []json.RawMessage{},
		Tasks: glanceTasks{
			ByState:          byState,
			DecisionsPending: byState["needs_decision"] + len(laneDecisions),
			Active:           glanceActiveTasks(active),
		},
		ConsolePaths: map[string]string{
			"decisions": "/ui/decisions",
			"queue":     "/ui/queue",
			"fleet":     "/ui/fleet",
		},
	}

	if !h.hub.configured() {
		response.Hub = glanceHub{Error: "unconfigured"}
		h.writeGlance(w, response)
		return
	}
	nodes, status, err := h.hub.rawNodes(r.Context())
	if err != nil {
		response.Hub = glanceHub{Error: glanceHubError(status)}
		h.writeGlance(w, response)
		return
	}
	lanes, status, err := h.hub.rawLanes(r.Context())
	if err != nil {
		response.Hub = glanceHub{Error: glanceHubError(status)}
		h.writeGlance(w, response)
		return
	}
	jobs, status, err := h.hub.rawJobs(r.Context())
	if err != nil {
		response.Hub = glanceHub{Error: glanceHubError(status)}
		h.writeGlance(w, response)
		return
	}
	response.Nodes = nodes
	response.Lanes = lanes
	response.Jobs = jobs
	addActiveJobs(response.Nodes, response.Jobs)
	h.writeGlance(w, response)
}

func glanceActiveTasks(tasks []store.Task) []glanceTask {
	active := make([]glanceTask, 0, len(tasks))
	for _, task := range tasks {
		active = append(active, glanceTask{
			ID:        task.ID,
			Lane:      task.Lane,
			Title:     task.Title,
			Kind:      task.Kind,
			State:     task.State,
			ClaimedBy: task.ClaimedBy,
			UpdatedAt: task.UpdatedAt,
		})
	}
	return active
}

func addActiveJobs(nodes []map[string]json.RawMessage, jobs []json.RawMessage) {
	byMachine := make(map[string]int, len(nodes))
	for _, job := range jobs {
		var value struct {
			Machine string `json:"machine"`
		}
		if json.Unmarshal(job, &value) == nil {
			byMachine[value.Machine]++
		}
	}
	for index, node := range nodes {
		if node == nil {
			node = map[string]json.RawMessage{}
			nodes[index] = node
		}
		var machine string
		_ = json.Unmarshal(node["machine_id"], &machine)
		node["active_jobs"] = json.RawMessage(strconv.Itoa(byMachine[machine]))
	}
}

func glanceHubError(status int) string {
	if status != 0 {
		return fmt.Sprintf("status_%d", status)
	}
	return "unreachable"
}

func (h *Handler) writeGlance(w http.ResponseWriter, response glanceResponse) {
	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	for len(body) > glanceBodyLimit && len(response.Tasks.Active) > 0 {
		response.Truncated = true
		response.Tasks.Active = response.Tasks.Active[:len(response.Tasks.Active)-1]
		body, err = json.Marshal(response)
		if err != nil {
			http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
			return
		}
	}
	if len(body) > glanceBodyLimit && !response.Truncated {
		response.Truncated = true
		body, err = json.Marshal(response)
		if err != nil {
			http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func (h *Handler) setAccepting(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	outcome := writeOutcome{action: "accepting", target: "-", eventID: "-", result: "invalid"}
	defer func() {
		h.audit(apiIdentity(identity), outcome.action, outcome.target, outcome.eventID, outcome.result)
	}()

	machine, matched := strings.CutPrefix(r.URL.Path, "/ui/api/nodes/")
	if !matched {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	machine, matched = strings.CutSuffix(machine, "/accepting")
	if !matched || !acceptingMachineRE.MatchString(machine) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	outcome.target = machine
	if identity.ServiceName != "" {
		if !jsonContentType(r.Header.Get("Content-Type")) {
			outcome.result = "content_type_reject"
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
	} else if !sameOrigin(r) {
		outcome.result = "origin_reject"
		w.WriteHeader(http.StatusForbidden)
		return
	} else if !h.validHeaderCSRF(r, identity.Email) {
		outcome.result = "csrf_reject"
		w.WriteHeader(http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var input struct {
		Accepting *bool  `json:"accepting"`
		Reason    string `json:"reason"`
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&input); err != nil || input.Accepting == nil || len([]byte(input.Reason)) > 120 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	status, body, contentType, err := h.hub.setAccepting(r.Context(), machine, *input.Accepting, input.Reason)
	if err != nil {
		outcome.result = "hub_error"
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"hub_unavailable"}`))
		return
	}
	outcome.result = "ok"
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		outcome.result = fmt.Sprintf("status_%d", status)
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *Handler) validHeaderCSRF(r *http.Request, email string) bool {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	return token != "" && hmac.Equal([]byte(token), []byte(cookie.Value)) && h.validCSRFToken(token, email)
}

func jsonContentType(raw string) bool {
	mediaType, _, err := mime.ParseMediaType(raw)
	return err == nil && mediaType == "application/json"
}

func apiIdentity(identity cfaccess.Identity) string {
	if identity.ServiceName != "" {
		return "service:" + identity.ServiceName
	}
	return "operator:" + identity.Email
}

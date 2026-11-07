package ui

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const (
	// inboxWriteBodyLimit holds a full batch: 50 item keys, an action, a
	// snooze time and an event key.
	inboxWriteBodyLimit = 16 * 1024
	// inboxWritePath applies one inbox action to one or more items.
	inboxWritePath = "/ui/api/inbox/state"
	// inboxUndoPath restores the items of one earlier write.
	inboxUndoPath = "/ui/api/inbox/undo"
	// inboxReadLimit bounds the GET state read: the Work page's visible rows.
	inboxReadLimit = 200
)

type inboxItemView struct {
	ItemKey      string     `json:"item_key"`
	Read         bool       `json:"read"`
	Done         bool       `json:"done"`
	SnoozedUntil *time.Time `json:"snoozed_until"`
}

type inboxFailureView struct {
	ItemKey string `json:"item_key"`
	Error   string `json:"error"`
}

type inboxWriteResponse struct {
	Action  string             `json:"action"`
	Items   []inboxItemView    `json:"items"`
	Failed  []inboxFailureView `json:"failed"`
	Skipped int                `json:"skipped"`
}

func inboxItemViewOf(x store.InboxState) inboxItemView {
	return inboxItemView{ItemKey: x.ItemKey, Read: x.ReadAt != nil, Done: x.DoneAt != nil, SnoozedUntil: x.SnoozedUntil}
}

// inboxState is the console's inbox triage write (hk 3343): read/unread,
// done/undone and snooze, for one item or a bulk selection. Body:
// {action, items, until, event_key}. event_key is required: a click submitted
// twice replays the first result. One refused item does not roll back the
// rest, so a bulk write reports per-item outcomes.
func (h *Handler) inboxState(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	outcome := writeOutcome{action: "inbox", target: "-", eventID: "-", result: "invalid"}
	defer func() {
		h.audit(apiIdentity(identity), outcome.action, outcome.target, outcome.eventID, outcome.result)
	}()
	var input struct {
		Action   string   `json:"action"`
		Items    []string `json:"items"`
		Until    string   `json:"until"`
		EventKey string   `json:"event_key"`
	}
	if !h.decodeOperatorWriteLimited(w, r, identity, &outcome, &input, &input.EventKey, inboxWriteBodyLimit) {
		return
	}
	if len(input.Items) == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var until *time.Time
	if input.Action == store.InboxActionSnooze && input.Until != "" {
		parsed, err := time.Parse(time.RFC3339, input.Until)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_snooze")
			return
		}
		until = &parsed
	}
	outcome.target = auditTarget(input.Items[0])
	result, err := h.store.ApplyInbox(r.Context(), input.Items, input.Action, until, apiIdentity(identity), input.EventKey)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidInboxWrite), errors.Is(err, store.ErrInvalidInboxSnooze):
			writeJSONError(w, http.StatusBadRequest, inboxStoreCode(err))
		default:
			log.Printf("ui.inbox_write_failed err=%q", err.Error())
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			outcome.result = "error"
		}
		return
	}
	response := inboxWriteResponse{Action: input.Action, Items: make([]inboxItemView, 0, len(result.Items)), Failed: []inboxFailureView{}, Skipped: result.Skipped}
	for _, item := range result.Items {
		if item.Err != nil {
			code := inboxStoreCode(item.Err)
			if code == "internal_error" {
				log.Printf("ui.inbox_item_failed item_key=%q err=%q", item.ItemKey, item.Err.Error())
			}
			response.Failed = append(response.Failed, inboxFailureView{ItemKey: item.ItemKey, Error: code})
			continue
		}
		response.Items = append(response.Items, inboxItemViewOf(item.State))
	}
	outcome.result = "ok"
	writeBoardJSON(w, response)
}

// inboxUndo is the inverse of one inbox write: body {event_key}, the key the
// write was sent with. Every item of that write is restored in one
// transaction, including the decision request snooze it mirrored.
func (h *Handler) inboxUndo(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	outcome := writeOutcome{action: "inbox-undo", target: "-", eventID: "-", result: "invalid"}
	defer func() {
		h.audit(apiIdentity(identity), outcome.action, outcome.target, outcome.eventID, outcome.result)
	}()
	var input struct {
		EventKey string `json:"event_key"`
	}
	if !h.decodeOperatorWriteLimited(w, r, identity, &outcome, &input, &input.EventKey, inboxWriteBodyLimit) {
		return
	}
	restored, err := h.store.UndoInbox(r.Context(), input.EventKey, apiIdentity(identity))
	if err != nil {
		if errors.Is(err, store.ErrInvalidInboxWrite) {
			writeJSONError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		log.Printf("ui.inbox_undo_failed err=%q", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		outcome.result = "error"
		return
	}
	items := make([]inboxItemView, 0, len(restored))
	conflicted := []string{}
	for _, item := range restored {
		if errors.Is(item.Err, store.ErrInboxUndoConflict) {
			conflicted = append(conflicted, item.ItemKey)
			continue
		}
		items = append(items, inboxItemViewOf(item.State))
	}
	outcome.result = "ok"
	writeBoardJSON(w, map[string]any{"items": items, "conflicted": conflicted})
}

// inboxStates serves GET /ui/api/inbox/state?keys=task:1,task:2: the stored
// triage state of the visible rows, so a page that lists tasks (Work) shows
// the same read/done/snoozed marks Home does.
func (h *Handler) inboxStates(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	if identity.ServiceName != "" {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("keys"))
	keys := []string{}
	if raw != "" {
		keys = strings.Split(raw, ",")
	}
	if len(keys) > inboxReadLimit {
		writeJSONError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	for _, key := range keys {
		if !store.ValidInboxItemKey(key) {
			writeJSONError(w, http.StatusBadRequest, "invalid_query")
			return
		}
	}
	states, err := h.store.InboxStates(r.Context(), keys)
	if err != nil {
		log.Printf("ui.inbox_states_failed err=%q", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	now := time.Now().UTC()
	type stateView struct {
		inboxItemView
		Woken bool `json:"woken"`
	}
	items := make([]stateView, 0, len(keys))
	for _, key := range keys {
		state := states[key]
		items = append(items, stateView{inboxItemView: inboxItemViewOf(state), Woken: state.Woken(now)})
	}
	writeBoardJSON(w, map[string]any{"items": items})
}

// inboxStoreCode maps a store refusal to the console's wire word. The console
// never renders a bare code; it switches to a Korean message.
func inboxStoreCode(err error) string {
	switch {
	case errors.Is(err, store.ErrInvalidInboxItem):
		return "invalid_inbox_item"
	case errors.Is(err, store.ErrInvalidInboxSnooze):
		return "invalid_snooze"
	case errors.Is(err, store.ErrInvalidInboxWrite):
		return "invalid_request"
	case errors.Is(err, store.ErrDecisionRecordNotFound):
		return "not_found"
	case errors.Is(err, store.ErrDecisionRecordNotOpen):
		return "decision_not_open"
	default:
		return "internal_error"
	}
}

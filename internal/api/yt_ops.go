package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// ytOps is GET /v1/yt/ops: the YouTrack write ops (slice b1a) for the
// operator to watch. Query: state (a lifecycle state, "pending" — the default:
// prepared, dispatched and remote_unknown — or "all"), kind (an op kind, empty
// for all), task, after_id, limit. Each op is a ytwrite.OpView: no body, no
// key; last_error (the detail behind reason) only for the operator's client. remote_unknown reads as
// pending, never done. next_after_id is set when the page is full.
func (s Server) ytOps(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	// last_error is raw remote or operator detail: only the operator's client
	// gets it.
	view := ytwrite.ViewOf
	if client == operatorClientID {
		view = ytwrite.OperatorViewOf
	}
	q := r.URL.Query()
	limit, err := queryLimit(r, 100, store.YTOpListMax)
	if err != nil {
		appErr(w, r, err)
		return
	}
	var task, after int64
	if q.Has("task") {
		if task, err = queryCursor(r, "task"); err != nil || task < 1 {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
	}
	if q.Has("after_id") {
		if after, err = queryCursor(r, "after_id"); err != nil {
			appErr(w, r, err)
			return
		}
	}
	ops, err := s.Service.Store.ListYTWriteOps(r.Context(), store.YTOpFilter{State: q.Get("state"), Kind: q.Get("kind"), TaskID: task, AfterID: after, Limit: limit})
	if err != nil {
		appErr(w, r, err)
		return
	}
	views := make([]ytwrite.OpView, 0, len(ops))
	for _, op := range ops {
		views = append(views, view(op))
	}
	var next any
	if len(ops) == limit {
		next = ops[len(ops)-1].ID
	}
	jsonOut(w, http.StatusOK, map[string]any{"ops": views, "next_after_id": next})
}

type ytOpsResolveInput struct {
	Applied *bool  `json:"applied"`
	YTID    string `json:"yt_id"`
	Adopted *bool  `json:"adopted"`
	By      string `json:"by"`
	Note    string `json:"note"`
}

// ytOpsResolve is POST /v1/yt/ops/{id}/resolve. Only the operator bearer
// client may make this ledger write; Protocol.Resolve owns the transition.
func (s Server) ytOpsResolve(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if client != operatorClientID {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	id, err := pathID(r, "id")
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	defer r.Body.Close()
	var in ytOpsResolveInput
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	if in.Applied == nil || (*in.Applied && in.YTID == "") || (!*in.Applied && (in.YTID != "" || in.Adopted != nil || strings.TrimSpace(in.By) == "" || strings.TrimSpace(in.Note) == "")) {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	p := &ytwrite.Protocol{Config: ytwrite.Config{Ledger: s.Service.Store.YTWriteLedger(), Now: func() time.Time { return time.Now().UTC() }}}
	result, err := p.Resolve(r.Context(), id, ytwrite.Resolution{Applied: *in.Applied, YTID: in.YTID, Adopted: in.Adopted, By: in.By, Note: in.Note})
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"op_id": result.OpID, "status": result.Status, "state": result.State, "reason": result.Reason, "retry_after": result.RetryAfter.String(), "yt_id": result.YTID})
}

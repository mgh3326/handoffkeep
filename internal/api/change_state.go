package api

import (
	"math"
	"net/http"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// PR state in hk (hk 3198; hk:doc research/2026-11-06/hk3176-pr-state-contract).
// mergewake writes five sections of a pull request's state and every client
// reads them back. A refused write answers 400 invalid_context with the
// store's reason. An older observed_at is not an error: the answer says
// applied:false, reason:"stale" for that section.

// decodeChangeWrite authenticates the caller and decodes a PR-state body into
// v; it answers the failure itself and reports whether to go on. Any client
// may write, as with POST /v1/task-events.
func (s Server) decodeChangeWrite(w http.ResponseWriter, r *http.Request, v any) bool {
	if _, ok := s.auth(w, r); !ok {
		return false
	}
	defer r.Body.Close()
	if err := decode(r, v, bodyPRState); err != nil {
		appErr(w, r, err)
		return false
	}
	return true
}

func (s Server) applyChangeBatch(w http.ResponseWriter, r *http.Request, b store.ChangeBatch) {
	res, err := s.Service.Store.ApplyChangeBatch(r.Context(), b)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, res)
}

// changeStatePut is PUT /v1/changes/state.
func (s Server) changeStatePut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.ChangeRef
		store.ChangeStateWrite
	}
	if s.decodeChangeWrite(w, r, &in) {
		s.applyChangeBatch(w, r, store.ChangeBatch{ChangeRef: in.ChangeRef, State: &in.ChangeStateWrite})
	}
}

// changeChecksPut is PUT /v1/changes/checks.
func (s Server) changeChecksPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.ChangeRef
		store.ChangeChecksWrite
	}
	if s.decodeChangeWrite(w, r, &in) {
		s.applyChangeBatch(w, r, store.ChangeBatch{ChangeRef: in.ChangeRef, Checks: &in.ChangeChecksWrite})
	}
}

// changeThreadsPut is PUT /v1/changes/threads.
func (s Server) changeThreadsPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.ChangeRef
		store.ChangeThreadsWrite
	}
	if s.decodeChangeWrite(w, r, &in) {
		s.applyChangeBatch(w, r, store.ChangeBatch{ChangeRef: in.ChangeRef, Threads: &in.ChangeThreadsWrite})
	}
}

// changeFindingsPut is PUT /v1/changes/findings.
func (s Server) changeFindingsPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.ChangeRef
		store.ChangeFindingsWrite
	}
	if s.decodeChangeWrite(w, r, &in) {
		s.applyChangeBatch(w, r, store.ChangeBatch{ChangeRef: in.ChangeRef, Findings: &in.ChangeFindingsWrite})
	}
}

// changeGatePut is PUT /v1/changes/gate.
func (s Server) changeGatePut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.ChangeRef
		store.ChangeGateWrite
	}
	if s.decodeChangeWrite(w, r, &in) {
		s.applyChangeBatch(w, r, store.ChangeBatch{ChangeRef: in.ChangeRef, Gate: &in.ChangeGateWrite})
	}
}

// changeBatch is POST /v1/changes/batch: any subset of the five sections for
// one PR in one transaction.
func (s Server) changeBatch(w http.ResponseWriter, r *http.Request) {
	var in store.ChangeBatch
	if s.decodeChangeWrite(w, r, &in) {
		s.applyChangeBatch(w, r, in)
	}
}

// changeOne is GET /v1/changes/one?repo=owner/name&pr=N[&all_heads=1][&include=checks,threads,findings,gate]:
// one PR's state. 404 for a PR hk has never seen.
func (s Server) changeOne(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	pr, err := strconv.Atoi(q.Get("pr"))
	if err != nil || pr < 1 || pr > math.MaxInt32 || !store.ValidChangeRepo(q.Get("repo")) {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	opt := store.ChangeReadOptions{}
	switch q.Get("all_heads") {
	case "", "0":
	case "1":
		opt.AllHeads = true
	default:
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if opt.Include, err = store.ParseChangeInclude(q.Get("include")); err != nil {
		appErr(w, r, err)
		return
	}
	v, found, err := s.Service.Store.GetChangeState(r.Context(), store.ChangeRef{Repo: q.Get("repo"), PR: pr}, opt)
	if err != nil {
		appErr(w, r, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	jsonOut(w, http.StatusOK, v)
}

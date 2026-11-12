package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ytAuthority is GET /v1/yt/authority: every kind's canonical side (hk 4122).
func (s Server) ytAuthority(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	rows, err := s.Service.Store.YTAuthorities(r.Context())
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"authority": rows})
}

type ytAuthoritySetInput struct {
	Kinds     []store.YTAuthorityKind `json:"kinds"`
	Authority store.YTAuthoritySide   `json:"authority"`
	By        string                  `json:"by"`
	Note      string                  `json:"note"`
}

// ytAuthoritySet is POST /v1/yt/authority, the switch and its rollback. Only
// the operator bearer client may make it. It runs in this server because the
// write modes it checks are this process's (store.SetYTAuthority).
func (s Server) ytAuthoritySet(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if client != operatorClientID {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	defer r.Body.Close()
	var in ytAuthoritySetInput
	if err := decode(r, &in, bodySmall); err != nil {
		appErr(w, r, err)
		return
	}
	before, after, err := s.Service.Store.SetYTAuthority(r.Context(), in.Kinds, in.Authority, in.By, in.Note)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"before": before, "after": after})
}

// importResourceKind is the authority kind a /v1/import resource carries.
var importResourceKind = map[string]store.YTAuthorityKind{
	"projects":         store.YTAuthorityTasks,
	"tasks":            store.YTAuthorityTasks,
	"comments":         store.YTAuthorityComments,
	"comment-authors":  store.YTAuthorityComments,
	"documents":        store.YTAuthorityDocuments,
	"task-documents":   store.YTAuthorityDocuments,
	"document-authors": store.YTAuthorityDocuments,
	"decisions":        store.YTAuthorityDecisions,
}

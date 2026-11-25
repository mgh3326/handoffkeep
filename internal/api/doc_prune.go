package api

import (
	"net/http"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// docPruneMaxDays bounds older_than_days so the cutoff duration cannot
// overflow (about 100 years).
const docPruneMaxDays = 36500

// docPruneRequest is the body of POST /v1/documents/prune. Apply false is a
// dry run; the store checks the prefix and the limit. Retention true selects
// document retention (hk 5057): review/mwr-* by merged change and screens/*
// by ended job, both DocRetentionDays old, and the merge link (hk 6280):
// review/mwr-*, briefs/mwr-* and screens/mwr-* by the review job's merged
// change, deleted on apply only when store.DocRetentionMergeLinkApply is on
// and classified per family in the dry run's merge_link either way
// (reports/mwr-* is never deleted). Prefix and older_than_days are
// ignored in retention mode; the cutoff is always the server clock minus
// DocRetentionDays.
type docPruneRequest struct {
	Prefix        string `json:"prefix"`
	OlderThanDays int    `json:"older_than_days"`
	Limit         int    `json:"limit"`
	Apply         bool   `json:"apply"`
	Retention     bool   `json:"retention"`
}

// docPruneWriter reports whether client may prune documents: the operator,
// the operator desk and the panewire design session, the same clients that
// write the bench catalog.
func docPruneWriter(client string) bool {
	return client == operatorClientID || client == operatorDeskClientID || client == panewireDesignClientID
}

// docPrune answers POST /v1/documents/prune with the store's counts. The
// cutoff is computed here from the server clock and passed to the store.
func (s Server) docPrune(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if !docPruneWriter(client) {
		writeError(w, http.StatusForbidden, "operator_required")
		return
	}
	defer r.Body.Close()
	var in docPruneRequest
	if e := decode(r, &in, bodySmall); e != nil {
		appErr(w, r, e)
		return
	}
	if in.Retention {
		cutoff := time.Now().UTC().Add(-time.Duration(store.DocRetentionDays) * 24 * time.Hour)
		res, e := s.Service.Store.PruneRetention(r.Context(), cutoff, in.Limit, in.Apply, client)
		if e != nil {
			appErr(w, r, e)
			return
		}
		jsonOut(w, http.StatusOK, res)
		return
	}
	if in.OlderThanDays < 1 || in.OlderThanDays > docPruneMaxDays {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(in.OlderThanDays) * 24 * time.Hour)
	res, e := s.Service.Store.PruneDocuments(r.Context(), in.Prefix, cutoff, in.Limit, in.Apply, client)
	if e != nil {
		appErr(w, r, e)
		return
	}
	jsonOut(w, http.StatusOK, res)
}

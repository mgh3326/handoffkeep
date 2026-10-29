package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func quotaDecode(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		jsonOut(w, 400, map[string]string{"error": "invalid_quota_request"})
		return false
	}
	return true
}
func (s Server) quotaAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	client, ok := s.auth(w, r)
	if !ok {
		return "", false
	}
	if !store.ValidQuotaKey(r.PathValue("key")) {
		jsonOut(w, 400, map[string]string{"error": "invalid_account_key"})
		return "", false
	}
	return client, true
}
func quotaError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrQuotaLease) {
		jsonOut(w, 409, map[string]string{"error": "quota_lease_conflict"})
	} else {
		jsonOut(w, 500, map[string]string{"error": "quota_store_unavailable"})
	}
}

// quotaScope limits writes to the client's granted account keys. Reads
// (latest) deliberately stay open to every authenticated client.
func (s Server) quotaScope(w http.ResponseWriter, client, key string) bool {
	if !s.QuotaAccounts[client][key] {
		jsonOut(w, 403, map[string]string{"error": "quota_account_forbidden"})
		return false
	}
	return true
}
func (s Server) quotaLatest(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.quotaAuth(w, r); !ok {
		return
	}
	x, err := s.Service.Store.LatestQuota(r.Context(), r.PathValue("key"))
	if err != nil {
		quotaError(w, err)
		return
	}
	jsonOut(w, 200, x)
}
func (s Server) quotaPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.quotaAuth(w, r)
	if !ok || !s.quotaScope(w, client, r.PathValue("key")) {
		return
	}
	var input struct {
		Holder      string                 `json:"holder"`
		Observation store.QuotaObservation `json:"observation"`
	}
	if !quotaDecode(w, r, &input) {
		return
	}
	if !store.ValidQuotaHolder(input.Holder) || !store.ValidQuotaObservation(input.Observation, time.Now()) {
		jsonOut(w, 400, map[string]string{"error": "invalid_quota_observation"})
		return
	}
	x, deduped, err := s.Service.Store.PutQuota(r.Context(), r.PathValue("key"), client, input.Holder, input.Observation)
	if err != nil {
		quotaError(w, err)
		return
	}
	if deduped {
		jsonOut(w, 200, x)
		return
	}
	jsonOut(w, 201, x)
}
func (s Server) quotaLease(w http.ResponseWriter, r *http.Request) {
	client, ok := s.quotaAuth(w, r)
	if !ok || !s.quotaScope(w, client, r.PathValue("key")) {
		return
	}
	var input struct {
		Holder string `json:"holder"`
		TTLS   int    `json:"ttl_s"`
	}
	if !quotaDecode(w, r, &input) {
		return
	}
	if input.TTLS == 0 {
		input.TTLS = 120
	}
	if !store.ValidQuotaHolder(input.Holder) || input.TTLS < 1 || input.TTLS > 120 {
		jsonOut(w, 400, map[string]string{"error": "invalid_quota_lease"})
		return
	}
	x, err := s.Service.Store.LeaseQuota(r.Context(), r.PathValue("key"), client, input.Holder, input.TTLS)
	if err != nil {
		quotaError(w, err)
		return
	}
	jsonOut(w, 200, x)
}
func (s Server) quotaRelease(w http.ResponseWriter, r *http.Request) {
	client, ok := s.quotaAuth(w, r)
	if !ok || !s.quotaScope(w, client, r.PathValue("key")) {
		return
	}
	var input struct {
		Holder string `json:"holder"`
	}
	if !quotaDecode(w, r, &input) {
		return
	}
	if !store.ValidQuotaHolder(input.Holder) {
		jsonOut(w, 400, map[string]string{"error": "invalid_quota_lease"})
		return
	}
	if err := s.Service.Store.ReleaseQuota(r.Context(), r.PathValue("key"), client, input.Holder); err != nil {
		quotaError(w, err)
		return
	}
	w.WriteHeader(204)
}

// quotaPresenceLimit caps one presence listing; the response says when rows
// were left out.
const quotaPresenceLimit = 1000

func (s Server) quotaPresencePut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	var input store.QuotaPresence
	if !quotaDecode(w, r, &input) {
		return
	}
	if !store.ValidQuotaPresence(input, time.Now()) {
		jsonOut(w, 400, map[string]string{"error": "invalid_quota_presence"})
		return
	}
	// The account key arrives in the body, so the observation PUT's grant
	// check runs here, after validation and before the store.
	if !s.quotaScope(w, client, input.AccountKey) {
		return
	}
	x, err := s.Service.Store.PutQuotaPresence(r.Context(), input)
	if err != nil {
		quotaError(w, err)
		return
	}
	jsonOut(w, 200, x)
}

// quotaPresenceList is open to every authenticated client, like latest.
func (s Server) quotaPresenceList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	host, key := q.Get("host_id"), q.Get("account_key")
	if host != "" && !store.ValidQuotaHost(host) || key != "" && !store.ValidQuotaKey(key) {
		jsonOut(w, 400, map[string]string{"error": "invalid_quota_presence_filter"})
		return
	}
	records, truncated, err := s.Service.Store.QuotaPresence(r.Context(), host, key, quotaPresenceLimit)
	if err != nil {
		quotaError(w, err)
		return
	}
	jsonOut(w, 200, map[string]any{"records": records, "truncated": truncated})
}

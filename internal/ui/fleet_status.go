package ui

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
)

// The fleet status page reads the panewire hub's periodic snapshot, the hk
// document fleet/status/latest (kind note). The contract is panewire's
// docs/fleet-status.md: hosts with their acceptance, one quota row per
// (host, account) reading, and the dry-run plan per grade. The document
// carries no credential; an account appears only as the shared quota
// cache's <pool>:<16-hex> fingerprint.
const fleetStatusDocKey = "fleet/status/latest"

// fleetStatusVersion is the only snapshot version this console parses; a
// different v is off-contract and answers the named error, never a guess.
const fleetStatusVersion = 1

type fleetStatusSnapshot struct {
	V     int                `json:"v"`
	At    time.Time          `json:"at"`
	Hosts []fleetStatusHost  `json:"hosts"`
	Quota []fleetStatusQuota `json:"quota"`
	Plan  []fleetStatusPlan  `json:"plan"`
}

type fleetStatusHost struct {
	Host      string `json:"host"`
	Connected bool   `json:"connected"`
	Version   string `json:"version,omitempty"`
	Accepting bool   `json:"accepting"`
	LanesUsed int    `json:"lanes_used"`
	LanesCap  int    `json:"lanes_cap"`
}

type fleetStatusWindow struct {
	Window   string   `json:"window"`
	UsedPct  *float64 `json:"used_pct"`
	ResetsAt string   `json:"resets_at,omitempty"`
}

type fleetStatusQuota struct {
	Host       string              `json:"host"`
	Pool       string              `json:"pool"`
	Account    string              `json:"account"`
	Windows    []fleetStatusWindow `json:"windows"`
	MeasuredAt time.Time           `json:"measured_at"`
	AgeS       int64               `json:"age_s"`
	FreshTTLS  int64               `json:"fresh_ttl_s"`
	Fresh      bool                `json:"fresh"`
	FreeState  string              `json:"free_state,omitempty"`
}

type fleetStatusPlan struct {
	Grade   string `json:"grade"`
	T       string `json:"t"`
	Seat    string `json:"seat,omitempty"`
	Host    string `json:"host,omitempty"`
	Pool    string `json:"pool,omitempty"`
	Account string `json:"account,omitempty"`
	Tier    string `json:"tier,omitempty"`
	Outcome string `json:"outcome"`
}

type fleetStatusResponse struct {
	// Snapshot is nil until the hub has written its first document.
	Snapshot *fleetStatusSnapshot `json:"snapshot"`
	// UpdatedAt is when the store last recorded the document, so the page
	// can tell a stalled writer from old readings inside a fresh snapshot.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// fleetStatusAPI serves GET /ui/api/fleet-status: the stored snapshot parsed
// and validated against the contract, or snapshot:null before the first
// write. A body that does not match the contract is a named 502 — the page
// must never render a half-parsed picture as if it were live.
func (h *Handler) fleetStatusAPI(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	if identity.ServiceName != "" {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	doc, found, err := h.store.GetDocument(r.Context(), fleetStatusDocKey)
	if err != nil {
		log.Printf("ui.fleet_status_error err=%q", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if !found {
		writeBoardJSON(w, fleetStatusResponse{})
		return
	}
	snapshot, err := parseFleetStatusSnapshot(doc.Body)
	if err != nil {
		log.Printf("ui.fleet_status_invalid key=%q err=%q", fleetStatusDocKey, err)
		writeJSONError(w, http.StatusBadGateway, "fleet_status_invalid")
		return
	}
	updated := doc.UpdatedAt.UTC()
	writeBoardJSON(w, fleetStatusResponse{Snapshot: snapshot, UpdatedAt: &updated})
}

// parseFleetStatusSnapshot decodes one JSON document into the contract's
// shape. Unknown fields are tolerated (the hub may add them); required
// fields must be present and non-empty, and the lists normalize to empty
// arrays so the response never carries null where the contract has a list.
func parseFleetStatusSnapshot(body string) (*fleetStatusSnapshot, error) {
	var snap fleetStatusSnapshot
	dec := json.NewDecoder(strings.NewReader(body))
	if err := dec.Decode(&snap); err != nil {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the snapshot")
	}
	if err := snap.validate(); err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *fleetStatusSnapshot) validate() error {
	if s.V != fleetStatusVersion {
		return errors.New("unsupported snapshot version")
	}
	if s.At.IsZero() {
		return errors.New("missing snapshot time")
	}
	if s.Hosts == nil {
		s.Hosts = []fleetStatusHost{}
	}
	if s.Quota == nil {
		s.Quota = []fleetStatusQuota{}
	}
	if s.Plan == nil {
		s.Plan = []fleetStatusPlan{}
	}
	for _, host := range s.Hosts {
		if host.Host == "" {
			return errors.New("host row missing host")
		}
	}
	for i, row := range s.Quota {
		if row.Host == "" || row.Pool == "" || row.Account == "" {
			return errors.New("quota row missing host, pool or account")
		}
		if row.MeasuredAt.IsZero() {
			return errors.New("quota row missing measured_at")
		}
		if row.Windows == nil {
			row.Windows = []fleetStatusWindow{}
		}
		for _, window := range row.Windows {
			if window.Window == "" {
				return errors.New("quota window missing name")
			}
		}
		s.Quota[i] = row
	}
	for _, plan := range s.Plan {
		if plan.Grade == "" || plan.T == "" || plan.Outcome == "" {
			return errors.New("plan row missing grade, scale or outcome")
		}
	}
	return nil
}

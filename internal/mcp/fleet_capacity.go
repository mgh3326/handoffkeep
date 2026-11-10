package mcp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	fleetCapacityDefaultLimit = 20
	fleetCapacityMaxLimit     = 100
)

type fleetCapacityInput struct {
	Job   string `json:"job,omitempty"`
	Host  string `json:"host,omitempty"`
	Since string `json:"since,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type fleetCapacityWindow struct {
	Kind      string     `json:"kind"`
	Window    string     `json:"window,omitempty"`
	State     string     `json:"state,omitempty"`
	UsedPct   *float64   `json:"used_pct,omitempty"`
	ResetsAt  *time.Time `json:"resets_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Expired   *bool      `json:"expired,omitempty"`
	AgeSecond int64      `json:"age_seconds"`
}

type fleetCapacityAccount struct {
	AccountKey string                `json:"account_key"`
	Pool       string                `json:"pool"`
	Windows    []fleetCapacityWindow `json:"windows"`
}

type fleetCapacityPresence struct {
	HostID   string                     `json:"host_id"`
	Accounts []fleetCapacityHostAccount `json:"accounts"`
}

type fleetCapacityHostAccount struct {
	AccountKey string `json:"account_key"`
	AgeSeconds int64  `json:"age_seconds"`
}

type fleetCapacityCandidate struct {
	Host    string `json:"host"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

type fleetCapacityPlacement struct {
	JobID      string                   `json:"job_id"`
	DecidedAt  time.Time                `json:"decided_at"`
	Outcome    string                   `json:"outcome"`
	Reason     string                   `json:"reason"`
	ChosenHost *string                  `json:"chosen_host,omitempty"`
	Profile    string                   `json:"profile"`
	Model      string                   `json:"model"`
	Candidates []fleetCapacityCandidate `json:"candidates"`
}

func (o operatorTools) fleetCapacity(ctx context.Context, _ *gmcp.CallToolRequest, in fleetCapacityInput) (*gmcp.CallToolResult, any, error) {
	if in.Job != "" && !store.ValidJobID(in.Job) {
		return nil, nil, errors.New("invalid job filter")
	}
	if in.Host != "" && !store.ValidJobHost(in.Host) {
		return nil, nil, errors.New("invalid host filter")
	}
	var since time.Time
	if in.Since != "" {
		parsed, err := time.Parse(time.RFC3339, in.Since)
		if err != nil {
			return nil, nil, errors.New("since must be RFC3339")
		}
		since = parsed.UTC()
	}
	limit := clampLimit(in.Limit, fleetCapacityDefaultLimit, fleetCapacityMaxLimit)
	now := time.Now().UTC()
	keys, err := o.service.Store.QuotaAccountKeys(ctx)
	if err != nil {
		return nil, nil, err
	}
	accounts := make([]fleetCapacityAccount, 0, len(keys))
	for _, key := range keys {
		latest, err := o.service.Store.LatestQuota(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		account := fleetCapacityAccount{AccountKey: key, Pool: strings.SplitN(key, ":", 2)[0], Windows: []fleetCapacityWindow{}}
		observations := append([]store.QuotaObservation(nil), latest.Manual...)
		if latest.Observation != nil {
			observations = append(observations, *latest.Observation)
		}
		for _, observation := range observations {
			age := int64(now.Sub(observation.MeasuredAt).Seconds())
			var expired *bool
			if observation.Kind == "manual" && observation.ExpiresAt != nil {
				value := !observation.ExpiresAt.After(now)
				expired = &value
			}
			for _, bucket := range observation.Buckets {
				used := bucket.UsedPct
				account.Windows = append(account.Windows, fleetCapacityWindow{Kind: observation.Kind, Window: bucket.Window, UsedPct: &used, ResetsAt: bucket.ResetsAt, ExpiresAt: observation.ExpiresAt, Expired: expired, AgeSecond: age})
			}
			if observation.FreeBucket != nil {
				account.Windows = append(account.Windows, fleetCapacityWindow{Kind: observation.Kind, Window: "free", State: observation.FreeBucket.State, ResetsAt: observation.FreeBucket.ResetsAt, AgeSecond: age})
			}
		}
		accounts = append(accounts, account)
	}

	presenceRows, _, err := o.service.Store.QuotaPresence(ctx, "", "", 100000)
	if err != nil {
		return nil, nil, err
	}
	presenceByHost := map[string]*fleetCapacityPresence{}
	for _, row := range presenceRows {
		entry := presenceByHost[row.HostID]
		age := int64(now.Sub(row.LastSeen).Seconds())
		if entry == nil {
			entry = &fleetCapacityPresence{HostID: row.HostID, Accounts: []fleetCapacityHostAccount{}}
			presenceByHost[row.HostID] = entry
		}
		entry.Accounts = append(entry.Accounts, fleetCapacityHostAccount{AccountKey: row.AccountKey, AgeSeconds: age})
	}
	presence := make([]fleetCapacityPresence, 0, len(presenceByHost))
	for _, entry := range presenceByHost {
		presence = append(presence, *entry)
	}

	placements, err := o.service.Store.ListFleetCapacityPlacements(ctx, in.Job, in.Host, since, limit)
	if err != nil {
		return nil, nil, err
	}
	placementViews := make([]fleetCapacityPlacement, 0, len(placements))
	for _, placement := range placements {
		view := fleetCapacityPlacement{
			JobID: placement.JobID, DecidedAt: placement.DecidedAt, Outcome: placement.Outcome,
			Reason: placement.Reason, ChosenHost: placement.ChosenHost, Profile: placement.Profile,
			Model: placement.Model, Candidates: make([]fleetCapacityCandidate, 0, len(placement.Candidates)),
		}
		for _, candidate := range placement.Candidates {
			view.Candidates = append(view.Candidates, fleetCapacityCandidate{Host: candidate.Host, Outcome: candidate.Outcome, Reason: candidate.Reason})
		}
		placementViews = append(placementViews, view)
	}
	return jsonResult(map[string]any{
		"as_of": now, "accounts": accounts, "presence": presence, "placements": placementViews,
		"placement_note": placementNote(len(placementViews)),
		"staleness_rule": "handoffkeep has no panewire quota staleness threshold; age_seconds is reported without a staleness flag; manual observations include expires_at and expired",
	})
}

func placementNote(count int) string {
	if count == 0 {
		return "no placement records"
	}
	return "placement records are evidence of recorded attempts; absence of rows does not establish that no spawn was attempted"
}

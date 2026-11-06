package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

const PlacementRetention = 180 * 24 * time.Hour

var ErrPlacementConflict = errors.New("placement_conflict")

// PlacementDecision is runtug's immutable record of one placement attempt.
// DecidedOn comes from the authenticated node, never from the request body.
type PlacementDecision struct {
	ID          int64                `json:"id"`
	JobID       string               `json:"job_id"`
	Attempt     int                  `json:"attempt"`
	TaskID      *int64               `json:"task_id,omitempty"`
	DecidedAt   time.Time            `json:"decided_at"`
	DecidedOn   string               `json:"decided_on"`
	ToolVersion string               `json:"tool_version"`
	Source      string               `json:"source"`
	Ranking     string               `json:"ranking"`
	StateSource string               `json:"state_source"`
	ChosenHost  *string              `json:"chosen_host,omitempty"`
	Outcome     string               `json:"outcome"`
	Reason      string               `json:"reason"`
	Profile     string               `json:"profile"`
	Pool        string               `json:"pool"`
	AccountKey  string               `json:"account_key"`
	Family      string               `json:"family"`
	Model       string               `json:"model"`
	Role        string               `json:"role"`
	PaneID      string               `json:"pane_id"`
	Repo        string               `json:"repo"`
	Candidates  []PlacementCandidate `json:"candidates"`
}

type PlacementCandidate struct {
	Host         string   `json:"host"`
	Outcome      string   `json:"outcome"`
	Reason       string   `json:"reason"`
	Score        *float64 `json:"score,omitempty"`
	SignalAgeS   *float64 `json:"signal_age_s,omitempty"`
	CPUBusyShort *float64 `json:"cpu_busy_short,omitempty"`
	CPUBusy5m    *float64 `json:"cpu_busy_5m,omitempty"`
	MemAvailable *float64 `json:"mem_available,omitempty"`
	SwapInMBPS   *float64 `json:"swap_in_mbps,omitempty"`
	PowerSource  string   `json:"power_source,omitempty"`
	BridgedJobs  *int     `json:"bridged_jobs,omitempty"`
	ActiveJobs   *int     `json:"active_jobs,omitempty"`
	Tasks        *int     `json:"tasks,omitempty"`
	TaskSlots    *int     `json:"task_slots,omitempty"`
	Power        *float64 `json:"power,omitempty"`
}

// ErrInvalidPlacement wraps a placement record's validation failure. Reasons
// name the field and the rule, never the submitted value. Its text
// is the wire code (invalid_context, unchanged for nodes); the text after
// "invalid_context: " names the failing field, so a node's spool can log why
// one record was refused and quarantine it (desk 2026-11-07: 733 records
// queued behind one refused record).
var ErrInvalidPlacement = errors.New("invalid_context")

func placementInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidPlacement, fmt.Sprintf(format, args...))
}

func checkPlacement(d PlacementDecision) error {
	now := time.Now().UTC()
	switch {
	case !ValidJobID(d.JobID):
		return placementInvalid("job_id")
	case d.Attempt < 1 || d.Attempt > 1000000:
		return placementInvalid("attempt out of range")
	case !ValidJobHost(d.DecidedOn):
		return placementInvalid("decided_on")
	case d.DecidedAt.IsZero():
		return placementInvalid("decided_at missing")
	case d.DecidedAt.After(now.Add(5 * time.Minute)):
		return placementInvalid("decided_at in the future")
	case d.DecidedAt.Before(now.Add(-PlacementRetention)):
		return placementInvalid("decided_at older than retention")
	}
	if d.Source != "auto" && d.Source != "forced" {
		return placementInvalid("source must be auto or forced")
	}
	if d.Outcome != "placed" && d.Outcome != "refused" && d.Outcome != "failed" {
		return placementInvalid("outcome must be placed, refused or failed")
	}
	switch {
	case d.Ranking == "":
		return placementInvalid("ranking empty")
	case d.StateSource == "":
		return placementInvalid("state_source empty")
	case d.TaskID != nil && *d.TaskID < 1:
		return placementInvalid("task_id")
	case d.ChosenHost != nil && !ValidJobHost(*d.ChosenHost):
		return placementInvalid("chosen_host")
	case d.Candidates == nil:
		return placementInvalid("candidates missing (send [] when there are none)")
	case len(d.Candidates) > 64:
		return placementInvalid("candidates over 64")
	}
	if d.Role != "" && d.Role != "lead" && d.Role != "worker" && d.Role != "review" && d.Role != "consult" {
		return placementInvalid("role must be lead, worker, review or consult")
	}
	if d.Outcome == "placed" && (d.TaskID == nil || d.ChosenHost == nil || d.Family == "" || d.Role == "") {
		return placementInvalid("placed decisions require task_id, chosen_host, family and role")
	}
	for _, f := range []struct{ name, v string }{{"job_id", d.JobID}, {"tool_version", d.ToolVersion}, {"ranking", d.Ranking}, {"state_source", d.StateSource}, {"reason", d.Reason}, {"profile", d.Profile}, {"pool", d.Pool}, {"account_key", d.AccountKey}, {"family", d.Family}, {"model", d.Model}, {"pane_id", d.PaneID}, {"repo", d.Repo}} {
		if !validText(f.v, 256) {
			return placementInvalid("%s: not printable text of at most 256 bytes", f.name)
		}
		if err := guard.Reject(f.v); err != nil {
			return err
		}
	}
	// Only an opaque account alias is accepted; an email, path or credential
	// URL has no place in this fleet-readable log.
	if strings.ContainsAny(d.AccountKey, "@/\\ \t\n") {
		return placementInvalid("account_key must be an opaque alias or fingerprint")
	}
	for i, c := range d.Candidates {
		switch {
		case !ValidJobHost(c.Host):
			return placementInvalid("candidates[%d].host", i)
		case c.Outcome == "" || !validText(c.Outcome, 64):
			return placementInvalid("candidates[%d].outcome", i)
		case !validText(c.Reason, 256):
			return placementInvalid("candidates[%d].reason: not printable text of at most 256 bytes", i)
		case c.PowerSource != "" && c.PowerSource != "ac" && c.PowerSource != "battery":
			return placementInvalid("candidates[%d].power_source must be empty, ac or battery", i)
		}
		if err := guard.Reject(c.Reason); err != nil {
			return err
		}
		for _, n := range []*float64{c.Score, c.SignalAgeS, c.CPUBusyShort, c.CPUBusy5m, c.MemAvailable, c.SwapInMBPS, c.Power} {
			if n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0)) {
				return placementInvalid("candidates[%d] signal is NaN or Inf", i)
			}
		}
	}
	return nil
}

const placementColumns = `id,job_id,attempt,task_id,decided_at,decided_on,tool_version,source,ranking,state_source,chosen_host,outcome,reason,profile,pool,account_key,family,model,role,pane_id,repo,candidates`

func scanPlacement(row interface{ Scan(...any) error }) (PlacementDecision, error) {
	var d PlacementDecision
	var raw []byte
	err := row.Scan(&d.ID, &d.JobID, &d.Attempt, &d.TaskID, &d.DecidedAt, &d.DecidedOn, &d.ToolVersion, &d.Source, &d.Ranking, &d.StateSource, &d.ChosenHost, &d.Outcome, &d.Reason, &d.Profile, &d.Pool, &d.AccountKey, &d.Family, &d.Model, &d.Role, &d.PaneID, &d.Repo, &raw)
	if err != nil {
		return d, err
	}
	_, err = scanJSONB(raw, &d.Candidates)
	return d, err
}

func (s *Store) RecordPlacement(ctx context.Context, d PlacementDecision) (PlacementDecision, bool, error) {
	if err := checkPlacement(d); err != nil {
		return d, false, err
	}
	d.ID = 0
	// Postgres stores microseconds; normalize before comparing a retry.
	d.DecidedAt = d.DecidedAt.UTC().Truncate(time.Microsecond)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return d, false, err
	}
	defer tx.Rollback(ctx)
	if d.TaskID != nil {
		var id int64
		if err = tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, *d.TaskID).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return d, false, ErrTaskNotFound
		} else if err != nil {
			return d, false, err
		}
	}
	raw, err := json.Marshal(d.Candidates)
	if err != nil {
		return d, false, err
	}
	stored, err := scanPlacement(tx.QueryRow(ctx, `INSERT INTO placement_decisions(job_id,attempt,task_id,decided_at,decided_on,tool_version,source,ranking,state_source,chosen_host,outcome,reason,profile,pool,account_key,family,model,role,pane_id,repo,candidates) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21::jsonb) ON CONFLICT(job_id,attempt) DO NOTHING RETURNING `+placementColumns,
		d.JobID, d.Attempt, d.TaskID, d.DecidedAt, d.DecidedOn, d.ToolVersion, d.Source, d.Ranking, d.StateSource, d.ChosenHost, d.Outcome, d.Reason, d.Profile, d.Pool, d.AccountKey, d.Family, d.Model, d.Role, d.PaneID, d.Repo, string(raw)))
	created := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		stored, err = scanPlacement(tx.QueryRow(ctx, `SELECT `+placementColumns+` FROM placement_decisions WHERE job_id=$1 AND attempt=$2`, d.JobID, d.Attempt))
		if err == nil {
			d.ID = stored.ID
			stored.DecidedAt = stored.DecidedAt.UTC()
			if !reflect.DeepEqual(d, stored) {
				return stored, false, ErrPlacementConflict
			}
		}
	}
	if err != nil {
		return d, false, err
	}
	if created && d.TaskID != nil && d.Outcome == "placed" {
		if err = s.recordLinearBoardPlacementTx(ctx, tx, stored); err != nil {
			return d, false, err
		}
	}
	return stored, created, tx.Commit(ctx)
}

func (s *Store) ListPlacements(ctx context.Context, job, host string, since time.Time, limit int) ([]PlacementDecision, error) {
	if (job != "" && !ValidJobID(job)) || (host != "" && !ValidJobHost(host)) || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid placement filter")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+placementColumns+` FROM placement_decisions WHERE ($1='' OR job_id=$1) AND ($2='' OR chosen_host=$2) AND decided_at >= $3 ORDER BY decided_at DESC,id DESC LIMIT $4`, job, host, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlacementDecision{}
	for rows.Next() {
		d, err := scanPlacement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) PrunePlacements(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM placement_decisions WHERE id IN (SELECT id FROM placement_decisions WHERE decided_at < $1 ORDER BY decided_at,id LIMIT 1000)`, time.Now().UTC().Add(-PlacementRetention))
	return tag.RowsAffected(), err
}

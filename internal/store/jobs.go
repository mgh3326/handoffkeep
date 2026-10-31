package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// JobResultMaxBytes caps the result object written with state=completed.
const JobResultMaxBytes = 64 << 10

// jobFieldMax caps every allowlisted scalar field.
const jobFieldMax = 256

var (
	ErrJobNotFound      = errors.New("job_not_found")
	ErrJobIDTaken       = errors.New("job_id_taken")
	ErrJobStateConflict = errors.New("job_state_conflict")
)

// jobIDRE is runtug's validID alphabet: <task>-<role>-<suffix> and
// mergewake's mwr-<pr>-<sha7>-p<n>-<unix> both fit it.
var jobIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// jobHostRE is the fleet host id alphabet: the node's HUB_MACHINE_ID.
var jobHostRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

var jobStates = map[string]bool{
	"claimed": true, "spawned": true, "completed": true, "escalated": true, "lost": true,
}

// jobTerminal names the states a record can never leave.
var jobTerminal = map[string]bool{"completed": true, "escalated": true, "lost": true}

// jobRank orders the state machine: claimed -> spawned -> a terminal state.
// A move is legal when it does not go down in rank; a terminal record is
// never rewritten, not even to itself.
var jobRank = map[string]int{"claimed": 0, "spawned": 1, "completed": 2, "escalated": 2, "lost": 2}

// ValidJobID reports whether id is a legal job id (runtug's validID, minus
// the "." and ".." corner cases the length range already excludes).
func ValidJobID(id string) bool { return jobIDRE.MatchString(id) }

// ValidJobHost reports whether host is a legal fleet host id.
func ValidJobHost(host string) bool { return jobHostRE.MatchString(host) }

// Job is the cross-host job record. Only allowlisted metadata is stored —
// never brief text, command lines, pane output or absolute local paths.
type Job struct {
	JobID      string          `json:"job_id"`
	TaskID     int64           `json:"task_id"`
	Host       string          `json:"host"`
	State      string          `json:"state"`
	Role       string          `json:"role,omitempty"`
	Label      string          `json:"label,omitempty"`
	Family     string          `json:"family,omitempty"`
	Profile    string          `json:"profile,omitempty"`
	Model      string          `json:"model,omitempty"`
	Pool       string          `json:"pool,omitempty"`
	OwnerLane  string          `json:"owner_lane,omitempty"`
	ParentLane string          `json:"parent_lane,omitempty"`
	ParentJob  string          `json:"parent_job,omitempty"`
	PaneID     string          `json:"pane_id,omitempty"`
	Worktree   *JobWorktree    `json:"worktree,omitempty"`
	ReportDoc  string          `json:"report_doc,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// JobWorktree records where a job runs: the remote, the checked out branch
// (null on a detached worktree) and the head commit.
type JobWorktree struct {
	Origin string  `json:"origin"`
	Branch *string `json:"branch"`
	Head   string  `json:"head"`
}

// JobUpdate is one PUT body: the fields present are merged over the record.
// Pointer fields distinguish "absent" from an explicit value.
type JobUpdate struct {
	State      *string         `json:"state"`
	Role       *string         `json:"role"`
	Label      *string         `json:"label"`
	Family     *string         `json:"family"`
	Profile    *string         `json:"profile"`
	Model      *string         `json:"model"`
	Pool       *string         `json:"pool"`
	OwnerLane  *string         `json:"owner_lane"`
	ParentLane *string         `json:"parent_lane"`
	ParentJob  *string         `json:"parent_job"`
	PaneID     *string         `json:"pane_id"`
	Worktree   *JobWorktree    `json:"worktree"`
	ReportDoc  *string         `json:"report_doc"`
	Result     json.RawMessage `json:"result"`
}

func validJobField(s string) bool { return len(s) <= jobFieldMax && !stringContainsNUL(s) }

func stringContainsNUL(s string) bool {
	for _, r := range s {
		if r == 0 {
			return true
		}
	}
	return false
}

// checkJobUpdate validates an update before it reaches the row. Result is
// checked here because it is request-shaped: object type, size and content.
func checkJobUpdate(u JobUpdate) error {
	if u.State != nil && !jobStates[*u.State] {
		return fmt.Errorf("invalid job state")
	}
	for _, f := range []*string{
		u.Role, u.Label, u.Family, u.Profile, u.Model, u.Pool,
		u.OwnerLane, u.ParentLane, u.PaneID, u.ReportDoc,
	} {
		if f != nil && !validJobField(*f) {
			return fmt.Errorf("invalid job field")
		}
	}
	if u.ParentJob != nil && *u.ParentJob != "" && !ValidJobID(*u.ParentJob) {
		return fmt.Errorf("invalid parent_job")
	}
	if u.Worktree != nil {
		if !validJobField(u.Worktree.Origin) || u.Worktree.Origin == "" ||
			!validJobField(u.Worktree.Head) || u.Worktree.Head == "" ||
			(u.Worktree.Branch != nil && !validJobField(*u.Worktree.Branch)) {
			return fmt.Errorf("invalid worktree")
		}
	}
	if len(u.Result) > 0 {
		if len(u.Result) > JobResultMaxBytes {
			return fmt.Errorf("job result too large")
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(u.Result, &obj); err != nil || obj == nil {
			return fmt.Errorf("job result must be a JSON object")
		}
		if u.State == nil || *u.State != "completed" {
			return fmt.Errorf("job result requires state completed")
		}
		// The serialized object is scanned like a text attachment, so a
		// secret-shaped value anywhere in it is refused.
		if err := guard.Reject(string(u.Result)); err != nil {
			return err
		}
	}
	return nil
}

const jobColumns = `job_id,task_id,host,state,role,label,family,profile,model,pool,owner_lane,parent_lane,parent_job,pane_id,worktree,report_doc,result,created_by,created_at,updated_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var worktree, result []byte
	err := row.Scan(&j.JobID, &j.TaskID, &j.Host, &j.State, &j.Role, &j.Label,
		&j.Family, &j.Profile, &j.Model, &j.Pool, &j.OwnerLane, &j.ParentLane,
		&j.ParentJob, &j.PaneID, &worktree, &j.ReportDoc, &result,
		&j.CreatedBy, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return j, err
	}
	if worktree != nil {
		j.Worktree = &JobWorktree{}
		if err = json.Unmarshal(worktree, j.Worktree); err != nil {
			return j, err
		}
	}
	j.Result = result
	return j, nil
}

// ClaimJob inserts the claim row. An identical replay (same job_id, task_id
// and host) returns the stored row with created=false; a different claim on
// a taken id is ErrJobIDTaken.
func (s *Store) ClaimJob(ctx context.Context, j Job) (Job, bool, error) {
	if !ValidJobID(j.JobID) || !ValidJobHost(j.Host) || j.TaskID < 1 || j.CreatedBy == "" || !validJobField(j.CreatedBy) {
		return j, false, errors.New("invalid job claim")
	}
	j.State = "claimed"
	now := time.Now().UTC()
	j.CreatedAt, j.UpdatedAt = now, now
	stored, err := scanJob(s.pool.QueryRow(ctx,
		`INSERT INTO jobs(`+jobColumns+`) VALUES($1,$2,$3,'claimed','','','','','','','','','','',NULL,'',NULL,$4,$5,$5)
		 ON CONFLICT (job_id) DO NOTHING RETURNING `+jobColumns,
		j.JobID, j.TaskID, j.Host, j.CreatedBy, now))
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return j, false, err
	}
	existing, found, err := s.GetJob(ctx, j.JobID)
	if err != nil {
		return j, false, err
	}
	if !found {
		return j, false, errors.New("job claim raced")
	}
	if existing.TaskID != j.TaskID || existing.Host != j.Host {
		return existing, false, ErrJobIDTaken
	}
	return existing, false, nil
}

// GetJob returns one job record by id.
func (s *Store) GetJob(ctx context.Context, id string) (Job, bool, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return j, false, nil
	}
	if err != nil {
		return j, false, err
	}
	return j, true, nil
}

// ListJobsByTask returns every job claimed for a task, oldest claim first.
func (s *Store) ListJobsByTask(ctx context.Context, taskID int64, limit int) ([]Job, error) {
	out := []Job{}
	rows, err := s.pool.Query(ctx, `SELECT `+jobColumns+` FROM jobs WHERE task_id=$1 ORDER BY created_at ASC, job_id ASC LIMIT $2`, taskID, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return out, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateJob merges the allowlisted fields of u over the record. States only
// move forward, a terminal record is never rewritten, and result is
// write-once together with state=completed — violations are
// ErrJobStateConflict.
func (s *Store) UpdateJob(ctx context.Context, id string, u JobUpdate) (Job, error) {
	var j Job
	if !ValidJobID(id) {
		return j, errors.New("invalid job id")
	}
	if err := checkJobUpdate(u); err != nil {
		return j, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return j, err
	}
	defer tx.Rollback(ctx)
	j, err = scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return j, ErrJobNotFound
	}
	if err != nil {
		return j, err
	}
	if jobTerminal[j.State] {
		return j, fmt.Errorf("%w: state %s is terminal", ErrJobStateConflict, j.State)
	}
	if u.State != nil && jobRank[*u.State] < jobRank[j.State] {
		return j, fmt.Errorf("%w: %s cannot move to %s", ErrJobStateConflict, j.State, *u.State)
	}
	// The update is valid; merge the present fields over the record.
	if u.State != nil {
		j.State = *u.State
	}
	for dst, src := range map[*string]*string{
		&j.Role: u.Role, &j.Label: u.Label, &j.Family: u.Family,
		&j.Profile: u.Profile, &j.Model: u.Model, &j.Pool: u.Pool,
		&j.OwnerLane: u.OwnerLane, &j.ParentLane: u.ParentLane,
		&j.ParentJob: u.ParentJob, &j.PaneID: u.PaneID,
		&j.ReportDoc: u.ReportDoc,
	} {
		if src != nil {
			*dst = *src
		}
	}
	if u.Worktree != nil {
		j.Worktree = u.Worktree
	}
	if u.Result != nil {
		j.Result = u.Result
	}
	j.UpdatedAt = time.Now().UTC()
	var worktreeArg, resultArg any
	if j.Worktree != nil {
		worktree, err := json.Marshal(j.Worktree)
		if err != nil {
			return j, err
		}
		worktreeArg = string(worktree)
	}
	if j.Result != nil {
		resultArg = string(j.Result)
	}
	j, err = scanJob(tx.QueryRow(ctx,
		`UPDATE jobs SET state=$2,role=$3,label=$4,family=$5,profile=$6,model=$7,pool=$8,owner_lane=$9,parent_lane=$10,parent_job=$11,pane_id=$12,worktree=$13::jsonb,report_doc=$14,result=$15::jsonb,updated_at=$16
		 WHERE job_id=$1 RETURNING `+jobColumns,
		j.JobID, j.State, j.Role, j.Label, j.Family, j.Profile, j.Model, j.Pool,
		j.OwnerLane, j.ParentLane, j.ParentJob, j.PaneID, worktreeArg, j.ReportDoc,
		resultArg, j.UpdatedAt))
	if err != nil {
		return j, err
	}
	return j, tx.Commit(ctx)
}

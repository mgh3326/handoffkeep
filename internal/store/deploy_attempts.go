package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The work record's deploys (hk 3036; design hk:doc
// design/2026-11-06/hk-work-record-design-v1.2 §2, §5, §13.1 R6). A service is
// registered by the operator; a deploy is one attempt to put a ref of that
// service on some of its targets, and deploy_changes freezes at start which
// changes the attempt carries. What is pending and when a task is deployed
// are computed from these rows (containedSQL), never stored.
//
// Deploy-record documents (deploy_record.go, applyDeployRecordTx) are a
// separate path and keep moving tasks on their own.

var (
	ErrServiceNotFound       = errors.New("service_not_found")
	ErrDeployNotFound        = errors.New("deploy_not_found")
	ErrDeployAttemptConflict = errors.New("deploy_attempt_conflict")
	ErrDeployAttemptFinished = errors.New("deploy_attempt_finished")
)

// Deploy modes of a service.
const (
	DeployModeManual = "manual"
	DeployModeAuto   = "auto"
	DeployModeNone   = "none"
)

// Deploy actions and attempt states.
const (
	DeployActionDeploy   = "deploy"
	DeployActionRollback = "rollback"

	DeployRequested  = "requested"
	DeployRunning    = "running"
	DeploySuccess    = "success"
	DeployFailed     = "failed"
	DeployRolledBack = "rolled_back"
)

// Store-owned caps of a start body: the API sizes its decode by them.
const (
	MaxDeployCandidates = 500
	MaxDeployPaths      = 20000
	MaxDeployBytes      = 2 << 20
	maxDeployText       = 500
	maxServiceList      = 100
)

var (
	repoRE      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	attemptIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)
)

// Service is one deployable service in the registry.
type Service struct {
	Name         string    `json:"name"`
	Repo         string    `json:"repo"`
	PathPatterns []string  `json:"path_patterns"`
	DeployMode   string    `json:"deploy_mode"`
	Targets      []string  `json:"targets"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// DeployCandidate is one PR a start names, with the paths it changed.
type DeployCandidate struct {
	PR    string   `json:"pr"`
	Paths []string `json:"paths"`
}

// DeployStart is the body of a start. Changes are the candidate PRs between
// previous_ref and deployed_ref; for a rollback, the PRs the rollback removes.
type DeployStart struct {
	AttemptID   string            `json:"attempt_id"`
	Service     string            `json:"service"`
	Targets     []string          `json:"targets"`
	Action      string            `json:"action"`
	PreviousRef string            `json:"previous_ref,omitempty"`
	DeployedRef string            `json:"deployed_ref"`
	State       string            `json:"state,omitempty"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	JobID       string            `json:"job_id,omitempty"`
	ApprovalRef string            `json:"approval_ref,omitempty"`
	Source      string            `json:"source"`
	Changes     []DeployCandidate `json:"changes"`
}

// DeployFinish is the body of a finish.
type DeployFinish struct {
	State      string `json:"state"`
	FailedStep string `json:"failed_step,omitempty"`
}

// DeployChange is one frozen deploy_changes row.
type DeployChange struct {
	ChangeID       int64   `json:"change_id"`
	PR             string  `json:"pr"`
	Included       bool    `json:"included"`
	MatchedPattern *string `json:"matched_pattern,omitempty"`
}

// Deploy is one attempt with its frozen changes.
type Deploy struct {
	ID          int64          `json:"id"`
	AttemptID   string         `json:"attempt_id"`
	Service     string         `json:"service"`
	Targets     []string       `json:"targets"`
	Action      string         `json:"action"`
	PreviousRef *string        `json:"previous_ref,omitempty"`
	DeployedRef string         `json:"deployed_ref"`
	State       string         `json:"state"`
	StartedAt   time.Time      `json:"started_at"`
	FinishedAt  *time.Time     `json:"finished_at,omitempty"`
	FailedStep  *string        `json:"failed_step,omitempty"`
	JobID       *string        `json:"job_id,omitempty"`
	ApprovalRef *string        `json:"approval_ref,omitempty"`
	Source      string         `json:"source"`
	PayloadHash string         `json:"payload_hash"`
	Changes     []DeployChange `json:"changes"`
}

// DeployPathMatch reports whether a changed file path matches a service path
// pattern: "dir/**" matches every path under dir/, any other pattern is
// path.Match. A malformed pattern matches nothing (PutService refuses one).
func DeployPathMatch(pattern, p string) bool {
	if dir, ok := strings.CutSuffix(pattern, "/**"); ok {
		return strings.HasPrefix(p, dir+"/")
	}
	ok, err := path.Match(pattern, p)
	return err == nil && ok
}

func checkTextList(field string, xs []string, valid func(string) bool) error {
	seen := map[string]bool{}
	for _, x := range xs {
		if !valid(x) || seen[x] {
			return fmt.Errorf("invalid service: %s has an empty, invalid or repeated entry", field)
		}
		seen[x] = true
	}
	return nil
}

func validPattern(p string) bool {
	if p == "" || !validText(p, maxDeployText) {
		return false
	}
	_, err := path.Match(strings.TrimSuffix(p, "/**"), "")
	return err == nil
}

func checkService(x Service) error {
	switch {
	case !validName(x.Name):
		return errors.New("invalid service: name must match [A-Za-z0-9._-]{1,128}")
	case !repoRE.MatchString(x.Repo):
		return errors.New("invalid service: repo must be owner/name")
	case len(x.PathPatterns) == 0:
		return errors.New("invalid service: at least one path pattern is required")
	case x.DeployMode != DeployModeManual && x.DeployMode != DeployModeAuto && x.DeployMode != DeployModeNone:
		return errors.New("invalid service: deploy_mode must be manual, auto or none")
	case x.DeployMode != DeployModeNone && len(x.Targets) == 0:
		return errors.New("invalid service: a deployed service needs at least one target")
	}
	if err := checkTextList("path_patterns", x.PathPatterns, validPattern); err != nil {
		return err
	}
	return checkTextList("targets", x.Targets, validName)
}

const serviceColumns = `name,repo,path_patterns,deploy_mode,targets,created_at,updated_at`

func scanService(row pgx.Row, x *Service) error {
	return row.Scan(&x.Name, &x.Repo, &x.PathPatterns, &x.DeployMode, &x.Targets, &x.CreatedAt, &x.UpdatedAt)
}

// PutService creates or replaces a service. Past attempts keep the targets
// and the frozen changes they recorded.
func (s *Store) PutService(ctx context.Context, x Service) (Service, error) {
	if x.Targets == nil {
		x.Targets = []string{}
	}
	if err := checkService(x); err != nil {
		return Service{}, err
	}
	now := time.Now().UTC()
	var out Service
	err := scanService(s.pool.QueryRow(ctx, `INSERT INTO services(name,repo,path_patterns,deploy_mode,targets,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$6)
ON CONFLICT(name) DO UPDATE SET repo=EXCLUDED.repo, path_patterns=EXCLUDED.path_patterns, deploy_mode=EXCLUDED.deploy_mode, targets=EXCLUDED.targets, updated_at=EXCLUDED.updated_at
RETURNING `+serviceColumns, x.Name, x.Repo, x.PathPatterns, x.DeployMode, x.Targets, now), &out)
	return out, err
}

// GetService reads one service.
func (s *Store) GetService(ctx context.Context, name string) (Service, bool, error) {
	var out Service
	err := scanService(s.pool.QueryRow(ctx, `SELECT `+serviceColumns+` FROM services WHERE name=$1`, name), &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return Service{}, false, nil
	}
	return out, err == nil, err
}

// ListServices reads every service by name.
func (s *Store) ListServices(ctx context.Context) ([]Service, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+serviceColumns+` FROM services ORDER BY name LIMIT $1`, maxServiceList)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Service, error) {
		var x Service
		return x, scanService(row, &x)
	})
}

// containedSQL is the one containment rule (§13.1 R6): change change is
// contained on (service, target) when the newest successful attempt of the
// service covering the target that carries the change as included is a
// deploy, not a rollback. The arguments are SQL expressions.
func containedSQL(service, target, change string) string {
	return `COALESCE((SELECT d.action='deploy' FROM deploys d JOIN deploy_changes dc ON dc.deploy_id=d.id
	WHERE d.service=` + service + ` AND d.state='success' AND ` + target + `=ANY(d.targets) AND dc.change_id=` + change + ` AND dc.included
	ORDER BY d.finished_at DESC, d.id DESC LIMIT 1), false)`
}

func checkDeployStart(in DeployStart) error {
	paths := 0
	for _, c := range in.Changes {
		paths += len(c.Paths)
		if !taskEventPRRE.MatchString(c.PR) {
			return errors.New("invalid deploy: every change needs a PR URL")
		}
		for _, p := range c.Paths {
			if p == "" || !validText(p, maxDeployText) {
				return errors.New("invalid deploy: a changed path is empty or too long")
			}
		}
	}
	switch {
	case !attemptIDRE.MatchString(in.AttemptID):
		return errors.New("invalid deploy: attempt_id must match [A-Za-z0-9][A-Za-z0-9._:-]{0,199}")
	case !validName(in.Service):
		return errors.New("invalid deploy: service must be a service name")
	case len(in.Targets) == 0 || checkTextList("targets", in.Targets, validName) != nil:
		return errors.New("invalid deploy: targets must be distinct target names, at least one")
	case in.Action != DeployActionDeploy && in.Action != DeployActionRollback:
		return errors.New("invalid deploy: action must be deploy or rollback")
	case in.DeployedRef == "" || !validText(in.DeployedRef, maxDeployText) || !validText(in.PreviousRef, maxDeployText):
		return errors.New("invalid deploy: deployed_ref is required and refs are at most 500 bytes")
	case in.State != DeployRequested && in.State != DeployRunning:
		return errors.New("invalid deploy: a start state is requested or running")
	case in.Source != "installer" && in.Source != "mergewake":
		return errors.New("invalid deploy: source must be installer or mergewake")
	case !validText(in.JobID, maxDeployText) || !validText(in.ApprovalRef, maxDeployText):
		return errors.New("invalid deploy: job_id and approval_ref are at most 500 bytes")
	case len(in.Changes) > MaxDeployCandidates || paths > MaxDeployPaths:
		return fmt.Errorf("invalid deploy: at most %d changes and %d paths", MaxDeployCandidates, MaxDeployPaths)
	}
	return nil
}

// deployPayloadHash is the hash an attempt is idempotent on: the normalized
// start body, as JSON.
func deployPayloadHash(in DeployStart) (string, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

// StartDeploy records an attempt and freezes its changes in one transaction.
// A candidate is included when it is a PR of the service's repo and one of
// its paths matches a service pattern (the first matching pattern is kept). A
// PR hk has not seen becomes a merged stub change. The same attempt_id with
// the same payload returns the stored attempt and created=false; another
// payload fails with ErrDeployAttemptConflict. Start locks no task row: it
// inserts only new changes keys and reads existing ones without a lock
// (lockorder.go).
func (s *Store) StartDeploy(ctx context.Context, in DeployStart) (Deploy, bool, error) {
	if in.State == "" {
		in.State = DeployRequested
	}
	if in.StartedAt != nil {
		at := in.StartedAt.UTC()
		in.StartedAt = &at
	}
	if err := checkDeployStart(in); err != nil {
		return Deploy{}, false, err
	}
	hash, err := deployPayloadHash(in)
	if err != nil {
		return Deploy{}, false, err
	}
	now := time.Now().UTC()
	started := now
	if in.StartedAt != nil {
		started = *in.StartedAt
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deploy{}, false, err
	}
	defer tx.Rollback(ctx)
	var svc Service
	if err = scanService(tx.QueryRow(ctx, `SELECT `+serviceColumns+` FROM services WHERE name=$1`, in.Service), &svc); errors.Is(err, pgx.ErrNoRows) {
		return Deploy{}, false, ErrServiceNotFound
	} else if err != nil {
		return Deploy{}, false, err
	}
	for _, t := range in.Targets {
		if !containsString(svc.Targets, t) {
			return Deploy{}, false, fmt.Errorf("invalid deploy: target %q is not a target of the service", t)
		}
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO deploys(attempt_id,service,targets,action,previous_ref,deployed_ref,state,started_at,job_id,approval_ref,source,payload_hash)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(attempt_id) DO NOTHING RETURNING id`,
		in.AttemptID, in.Service, in.Targets, in.Action, nullStr(in.PreviousRef), in.DeployedRef, in.State, started, nullStr(in.JobID), nullStr(in.ApprovalRef), in.Source, hash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		var stored string
		if err = tx.QueryRow(ctx, `SELECT payload_hash FROM deploys WHERE attempt_id=$1`, in.AttemptID).Scan(&stored); err != nil {
			return Deploy{}, false, err
		}
		if stored != hash {
			return Deploy{}, false, ErrDeployAttemptConflict
		}
		d, err := readDeployTx(ctx, tx, in.AttemptID)
		return d, false, err
	}
	if err != nil {
		return Deploy{}, false, err
	}
	// Changes in (repo, number) order, so two starts that insert the same new
	// keys wait on each other in one order.
	type candidate struct {
		repo   string
		number int
		paths  []string
	}
	byKey := map[string]int{}
	var cands []candidate
	for _, c := range in.Changes {
		repo, n, _ := parsePRURL(c.PR)
		key := changeURL(repo, n)
		if i, ok := byKey[key]; ok {
			cands[i].paths = append(cands[i].paths, c.Paths...)
			continue
		}
		byKey[key] = len(cands)
		cands = append(cands, candidate{repo: repo, number: n, paths: c.Paths})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].repo != cands[j].repo {
			return cands[i].repo < cands[j].repo
		}
		return cands[i].number < cands[j].number
	})
	for _, c := range cands {
		var changeID int64
		err = tx.QueryRow(ctx, `SELECT id FROM changes WHERE repo=$1 AND pr_number=$2`, c.repo, c.number).Scan(&changeID)
		if errors.Is(err, pgx.ErrNoRows) {
			changeID, err = upsertChangeTx(ctx, tx, changeWrite{repo: c.repo, number: c.number, state: ChangeMerged, mergedAt: &now}, now, now)
		}
		if err != nil {
			return Deploy{}, false, err
		}
		var matched *string
		if c.repo == svc.Repo {
			matched = matchedPattern(svc.PathPatterns, c.paths)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO deploy_changes(deploy_id,change_id,included,matched_pattern) VALUES($1,$2,$3,$4)`, id, changeID, matched != nil, matched); err != nil {
			return Deploy{}, false, err
		}
	}
	d, err := readDeployTx(ctx, tx, in.AttemptID)
	if err != nil {
		return Deploy{}, false, err
	}
	return d, true, tx.Commit(ctx)
}

func containsString(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// matchedPattern returns the first pattern, in service order, that one of
// paths matches, or nil.
func matchedPattern(patterns, paths []string) *string {
	for _, pat := range patterns {
		for _, p := range paths {
			if DeployPathMatch(pat, p) {
				return &pat
			}
		}
	}
	return nil
}

const deployColumns = `id,attempt_id,service,targets,action,previous_ref,deployed_ref,state,started_at,finished_at,failed_step,job_id,approval_ref,source,payload_hash`

func readDeployTx(ctx context.Context, q pgx.Tx, attemptID string) (Deploy, error) {
	var d Deploy
	err := q.QueryRow(ctx, `SELECT `+deployColumns+` FROM deploys WHERE attempt_id=$1`, attemptID).Scan(
		&d.ID, &d.AttemptID, &d.Service, &d.Targets, &d.Action, &d.PreviousRef, &d.DeployedRef, &d.State, &d.StartedAt, &d.FinishedAt, &d.FailedStep, &d.JobID, &d.ApprovalRef, &d.Source, &d.PayloadHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deploy{}, ErrDeployNotFound
	}
	if err != nil {
		return Deploy{}, err
	}
	rows, err := q.Query(ctx, `SELECT dc.change_id,c.repo,c.pr_number,dc.included,dc.matched_pattern FROM deploy_changes dc JOIN changes c ON c.id=dc.change_id WHERE dc.deploy_id=$1 ORDER BY dc.change_id`, d.ID)
	if err != nil {
		return Deploy{}, err
	}
	d.Changes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (DeployChange, error) {
		var c DeployChange
		var repo string
		var n int
		err := row.Scan(&c.ChangeID, &repo, &n, &c.Included, &c.MatchedPattern)
		c.PR = changeURL(repo, n)
		return c, err
	})
	if d.Changes == nil {
		d.Changes = []DeployChange{}
	}
	return d, err
}

// FinishDeploy ends an attempt: requested or running moves to success, failed
// or rolled_back once. The same state again returns the stored attempt;
// another state fails with ErrDeployAttemptFinished. A success applies the
// task rule (deployedTasksTx) in the same transaction.
func (s *Store) FinishDeploy(ctx context.Context, attemptID string, in DeployFinish) (Deploy, error) {
	switch {
	case !attemptIDRE.MatchString(attemptID):
		return Deploy{}, errors.New("invalid deploy: attempt_id must match [A-Za-z0-9][A-Za-z0-9._:-]{0,199}")
	case in.State != DeploySuccess && in.State != DeployFailed && in.State != DeployRolledBack:
		return Deploy{}, errors.New("invalid deploy: a finish state is success, failed or rolled_back")
	case !validText(in.FailedStep, maxDeployText):
		return Deploy{}, errors.New("invalid deploy: failed_step is at most 500 bytes")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deploy{}, err
	}
	defer tx.Rollback(ctx)
	if in.State == DeploySuccess {
		// Level 2 before the deploys row and the task rows (lockorder.go).
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
			return Deploy{}, err
		}
	}
	var id int64
	var state string
	err = tx.QueryRow(ctx, `SELECT id,state FROM deploys WHERE attempt_id=$1 FOR UPDATE`, attemptID).Scan(&id, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deploy{}, ErrDeployNotFound
	}
	if err != nil {
		return Deploy{}, err
	}
	switch state {
	case in.State:
		return readDeployTx(ctx, tx, attemptID)
	case DeployRequested, DeployRunning:
	default:
		return Deploy{}, ErrDeployAttemptFinished
	}
	now := time.Now().UTC()
	if _, err = tx.Exec(ctx, `UPDATE deploys SET state=$2, finished_at=$3, failed_step=$4 WHERE id=$1`, id, in.State, now, nullStr(in.FailedStep)); err != nil {
		return Deploy{}, err
	}
	if in.State == DeploySuccess {
		if err = s.deployedTasksTx(ctx, tx, id, attemptID, now); err != nil {
			return Deploy{}, err
		}
	}
	d, err := readDeployTx(ctx, tx, attemptID)
	if err != nil {
		return Deploy{}, err
	}
	return d, tx.Commit(ctx)
}

// changeDeployedSQL says whether change c needs nothing more to be deployed:
// it is contained on every current target of every service that ever
// included it. A change no service included needs no deploy when every
// service of its repo is deploy_mode none or left it out by the path filter
// (a repo without a service has no deploy flow: merged is the end, §2).
func changeDeployedSQL() string {
	includedBy := `SELECT d.service FROM deploy_changes dc JOIN deploys d ON d.id=dc.deploy_id WHERE dc.change_id=c.id AND dc.included`
	return `CASE WHEN EXISTS(` + includedBy + `)
	THEN NOT EXISTS(SELECT 1 FROM services s, unnest(s.targets) AS t(target) WHERE s.name IN (` + includedBy + `) AND NOT ` + containedSQL("s.name", "t.target", "c.id") + `)
	ELSE NOT EXISTS(SELECT 1 FROM services s WHERE s.repo=c.repo AND s.deploy_mode<>'none'
		AND NOT EXISTS(SELECT 1 FROM deploy_changes dc JOIN deploys d ON d.id=dc.deploy_id WHERE d.service=s.name AND dc.change_id=c.id AND NOT dc.included))
	END`
}

// deployedTasksTx is the task rule: every merged task linked as implements to
// a change of attempt deployID moves to deployed (hk.deploy, source_ref
// "deploy:<attempt_id>") when it has at least one implements change and each
// one is deployed (changeDeployedSQL). The caller holds the level-2 lock; task
// rows are locked in ascending id.
func (s *Store) deployedTasksTx(ctx context.Context, tx pgx.Tx, deployID int64, attemptID string, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE state=$2 AND id IN (
	SELECT ct.task_id FROM change_tasks ct JOIN deploy_changes dc ON dc.change_id=ct.change_id WHERE dc.deploy_id=$1 AND ct.relation=$3)
ORDER BY id FOR UPDATE`, deployID, StateMerged, ChangeImplements)
	if err != nil {
		return err
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Task, error) {
		var x Task
		return x, scanTask(row, &x)
	})
	if err != nil {
		return err
	}
	deployed := `SELECT bool_and(` + changeDeployedSQL() + `) IS TRUE
FROM change_tasks ct JOIN changes c ON c.id=ct.change_id WHERE ct.task_id=$1 AND ct.relation=$2`
	for _, x := range tasks {
		var ok bool
		if err = tx.QueryRow(ctx, deployed, x.ID, ChangeImplements).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			continue
		}
		ev := sourceEvent{Source: SourceDeploy, SourceRef: "deploy:" + attemptID, At: now, Note: "deploy attempt " + attemptID + " finished; every implements change is deployed"}
		if _, err = s.applySourceEventTx(ctx, tx, x, ev, now); err != nil {
			return err
		}
	}
	return nil
}

// PendingChanges lists the merged, not reverted changes of a service's repo
// that are not contained on every target of the service, oldest merge first.
// A change an attempt of the service recorded as left out by the path filter
// is not the service's and is never pending.
func (s *Store) PendingChanges(ctx context.Context, service string, limit int) ([]Change, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	if _, found, err := s.GetService(ctx, service); err != nil {
		return nil, err
	} else if !found {
		return nil, ErrServiceNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id FROM changes c JOIN services s ON s.repo=c.repo
WHERE s.name=$1 AND c.state='merged' AND c.reverted_by_change_id IS NULL
	AND NOT EXISTS(SELECT 1 FROM deploy_changes dc JOIN deploys d ON d.id=dc.deploy_id WHERE d.service=s.name AND dc.change_id=c.id AND NOT dc.included)
	AND EXISTS(SELECT 1 FROM unnest(s.targets) AS t(target) WHERE NOT `+containedSQL("s.name", "t.target", "c.id")+`)
ORDER BY c.merged_at, c.id LIMIT $2`, service, limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []Change{}, nil
	}
	got, err := s.listChangesWhere(ctx, s.pool, `id = ANY($1)`, len(ids), ids)
	if err != nil {
		return nil, err
	}
	at := make(map[int64]Change, len(got))
	for _, c := range got {
		at[c.ID] = c
	}
	out := make([]Change, 0, len(ids))
	for _, id := range ids {
		out = append(out, at[id])
	}
	return out, nil
}

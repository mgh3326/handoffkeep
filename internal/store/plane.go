package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const PlaneDrainAdvisoryLock int64 = 824180175

const (
	PlaneOpWorkItemCreate = "work_item_create"
	PlaneOpWorkItemUpdate = "work_item_update"
	// PlaneOpWorkItemRemove takes a work item out of the projection when its
	// task reaches a terminal state. The mirror's selection rule is
	// non-terminal tasks only, so terminal rows leave the board; their
	// history stays in hk.
	PlaneOpWorkItemRemove = "work_item_remove"
)

// planeStateFromLinearName derives Plane workflow state names from the Linear
// display names that LinearTaskStateMapping — the sole hk-to-remote state
// mapping table — can emit. It is keyed by Linear name, not by hk state, so
// it stays a derivation of the sole table rather than a second hk-state
// mapping table. Plane's default workflow has no review state, so In Review
// derives to In Progress.
var planeStateFromLinearName = map[string]string{
	"Backlog":     "Backlog",
	"In Progress": "In Progress",
	"In Review":   "In Progress",
	"Done":        "Done",
	"Canceled":    "Cancelled",
}

// ErrPlaneStateUndeclared fails the projection closed when a task state is
// absent from LinearTaskStateMapping or its Linear name has no declared Plane
// derivation. Mirroring never invents a mapping for an undeclared state.
var ErrPlaneStateUndeclared = errors.New("plane_state_undeclared")

// PlaneStateForTask derives the Plane state name for an hk task state.
// mutate=false carries the Mutate:false semantic of LinearTaskStateMapping
// (needs_decision): the remote board keeps the state it had before the task
// stalled, and the payload leaves State empty. Undeclared states error rather
// than guessing, so a second implicit mapping table cannot appear.
func PlaneStateForTask(hkState string) (name string, mutate bool, err error) {
	mapping, known := LinearTaskStateMapping[hkState]
	if !known {
		return "", false, fmt.Errorf("%w: hk task state %q is absent from LinearTaskStateMapping", ErrPlaneStateUndeclared, hkState)
	}
	if !mapping.Mutate {
		return "", false, nil
	}
	name, declared := planeStateFromLinearName[mapping.Name]
	if !declared {
		return "", false, fmt.Errorf("%w: Linear state name %q has no Plane derivation", ErrPlaneStateUndeclared, mapping.Name)
	}
	return name, true, nil
}

// PlanePriorityFor maps the hk integer priority onto Plane's closed priority
// vocabulary. hk orders its queue by priority DESC, so higher numbers are
// more urgent; zero or negative stays Plane's "none".
func PlanePriorityFor(priority int) string {
	switch {
	case priority >= 4:
		return "urgent"
	case priority == 3:
		return "high"
	case priority == 2:
		return "medium"
	case priority == 1:
		return "low"
	default:
		return "none"
	}
}

// PlaneTitleMaxRunes bounds the projected work-item name. Plane accepts ~255
// characters; the tighter cap keeps the title a one-line board entry and
// leaves room for the marker suffix.
const PlaneTitleMaxRunes = 140

// planeSensitiveTitlePatterns names the title content classes that must never
// leave hk: defect reproduction vectors, credential/key material and key
// file paths, and host or network identifiers. Repo-relative paths such as
// internal/store/plane.go intentionally do not match — only absolute or
// sensitive-rooted paths do.
var planeSensitiveTitlePatterns = []*regexp.Regexp{
	// IP literals (v4 with optional port; v6 groups or compressed runs).
	regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?\b`),
	regexp.MustCompile(`\b[0-9a-fA-F]{0,4}(?::[0-9a-fA-F]{0,4}){2,7}\b`),
	// URLs and qualified hostnames reveal hosts even without an IP literal.
	regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`),
	regexp.MustCompile(`\b[a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)*\.(?:internal|local|lan|home|corp|intranet|test)\b`),
	regexp.MustCompile(`\b(?:[a-zA-Z0-9-]+\.)+(?:prod|staging|internal|corp|lan|local)\.`),
	regexp.MustCompile(`\b[a-zA-Z0-9._-]+@(?:[a-zA-Z0-9-]+\.)+[a-zA-Z]{2,}\b`),
	// Absolute or sensitive-rooted filesystem paths and key material files.
	// The boundary before / or a leading dot is any non-word, non-slash char
	// (parens, =, :, quotes, brackets) so path(/home/x) and x=/home/x and
	// config(.env) cannot slip through while repo-relative a/b/c stays out.
	regexp.MustCompile(`(?:^|[^A-Za-z0-9/])/(?:home|Users|root|etc|var|usr|opt|srv|tmp|private|mnt|data|Volumes|boot|proc)/\S*`),
	regexp.MustCompile(`\b[A-Za-z]:\\(?:Users|Windows|ProgramData|Program Files)\b`),
	regexp.MustCompile(`~/(?:\.ssh|\.aws|\.gnupg|\.kube|\.docker|\.config|\.env)\S*`),
	regexp.MustCompile(`(?i)\b(?:id_rsa|id_dsa|id_ecdsa|id_ed25519|secrets?\.(?:ya?ml|json)|credentials|known_hosts|authorized_keys)\b`),
	regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9/])\.(?:env|env\.\w+|ssh|aws|gnupg|kube|docker|netrc|pgpass|npmrc|pypirc|htpasswd|pem|key)\b`),
	regexp.MustCompile(`(?i)\b\S+\.(?:pem|key|p12|pfx|jks|keystore|kubeconfig|asc|ppk)\b`),
	// Secret-shaped material: KEY=VALUE assignments with credential names,
	// provider token prefixes, PEM/JWT bodies, and SSH public keys.
	regexp.MustCompile(`(?i)\b\w*(?:key|secret|token|passwd|password|credential|bearer|authorization)\w*\s*[:=]\s*\S+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr|glpat|xox[bapors]|sk|pk)[-_][A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.`),
	regexp.MustCompile(`\b(?:ssh-rsa|ssh-ed25519|ecdsa-sha2-\S+)\s+[A-Za-z0-9+/=]{20,}\b`),
	// Defect reproduction vectors: shell pipes, substitution, destructive or
	// injection-shaped fragments embedded in a title.
	regexp.MustCompile(`\|\s*(?:sudo\s+)?(?:[bdckzf]?a?sh|python\d?|perl|ruby|node)\b`),
	regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^\n|;]*\|`),
	regexp.MustCompile(`(?i)\br\s?m\s+-[a-z]*[rf][a-z]*\b`),
	regexp.MustCompile(`(?i)\bsudo\s+\S`),
	regexp.MustCompile(`\$\(`),
	regexp.MustCompile("`"),
	regexp.MustCompile(`(?i)<\s*(script|iframe|img|svg)\b`),
	regexp.MustCompile(`(?i)javascript:`),
	regexp.MustCompile(`(?i)\bunion\s+(all\s+)?select\b`),
	regexp.MustCompile(`(?i)\bdrop\s+table\b`),
	regexp.MustCompile(`(?i)'\s*or\s+\d+\s*=\s*\d+`),
	regexp.MustCompile(`(?i)\b(?:chmod|chown)\s+[0-7]{3,4}\b`),
}

// PlaneTitleIsSensitive reports whether a title carries any content class
// that must be withheld from the external mirror.
func PlaneTitleIsSensitive(title string) bool {
	for _, pattern := range planeSensitiveTitlePatterns {
		if pattern.MatchString(title) {
			return true
		}
	}
	return false
}

// planeWithheldTitle is the neutral replacement for a sensitive title: the
// stable external reference plus the task kind, with no title content.
func planeWithheldTitle(taskID int64, kind string) string {
	return fmt.Sprintf("hk:task/%d %s task (title withheld)", taskID, kind)
}

// PlaneTaskTitle renders the mirrored work-item name: a sensitive title is
// replaced by the hk:task/<id> reference and a neutral summary, otherwise the
// title is truncated to PlaneTitleMaxRunes runes.
func PlaneTaskTitle(task Task) string {
	if PlaneTitleIsSensitive(task.Title) {
		return planeWithheldTitle(task.ID, task.Kind)
	}
	return truncatePlaneTitle(task.Title)
}

func truncatePlaneTitle(title string) string {
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) <= PlaneTitleMaxRunes {
		return title
	}
	runes := []rune(title)
	return strings.TrimSpace(string(runes[:PlaneTitleMaxRunes-1])) + "…"
}

// PlaneOutboxPayload is the complete projected snapshot of one task. It is
// the only shape the mirror writes: the allowlist is structural because this
// struct has no field that could carry a report path, note, refs entry, or
// description body. State is empty when the task state is Mutate:false —
// the remote work item then keeps its current state while other fields sync.
type PlaneOutboxPayload struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	State      string `json:"state,omitempty"`
	Lane       string `json:"lane"`
	Kind       string `json:"kind"`
	Priority   string `json:"priority"`
	Project    string `json:"project"`
}

// PlaneExternalID is the stable one-task-one-work-item key, formatted as
// hk:task/<id>. If hk ever grows a second serving instance the id pair
// becomes (instance, task_id); the string keeps the point of change to this
// one formatter.
func PlaneExternalID(taskID int64) string {
	return fmt.Sprintf("hk:task/%d", taskID)
}

// PlaneProjectionFor is the single point where a task becomes its mirrored
// payload — the drain and the reconciler must both call it so the projected
// fields can never diverge between write paths.
func PlaneProjectionFor(task Task) (PlaneOutboxPayload, error) {
	state, mutate, err := PlaneStateForTask(task.State)
	if err != nil {
		return PlaneOutboxPayload{}, err
	}
	payload := PlaneOutboxPayload{
		ExternalID: PlaneExternalID(task.ID),
		Name:       PlaneTaskTitle(task),
		Lane:       task.Lane,
		Kind:       task.Kind,
		Priority:   PlanePriorityFor(task.Priority),
	}
	if task.Project != nil {
		payload.Project = *task.Project
	}
	if mutate {
		payload.State = state
	}
	return payload, nil
}

type PlaneOutbox struct {
	ID            int64              `json:"id"`
	TaskID        int64              `json:"task_id"`
	Seq           int                `json:"seq"`
	Op            string             `json:"op"`
	Payload       PlaneOutboxPayload `json:"payload"`
	State         string             `json:"state"`
	Attempts      int                `json:"attempts"`
	NextAttemptAt time.Time          `json:"next_attempt_at"`
	LastError     string             `json:"last_error"`
	RemoteID      string             `json:"remote_id"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

// PlaneIssue records which remote work item a task is mirrored to. RemoteID
// is the Plane work item UUID; ProjectID is the Plane project that owns it,
// kept so a project remap can detect that the mirrored item must move.
type PlaneIssue struct {
	TaskID     int64     `json:"task_id"`
	WorkItemID string    `json:"work_item_id"`
	ExternalID string    `json:"external_id"`
	ProjectID  string    `json:"project_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type PlaneOutboxResult struct {
	State     string
	RetryAt   time.Time
	LastError string
	RemoteID  string
	ProjectID string
	// Unlink drops the task's plane_issues row. Only the remove op sets it:
	// the link table then holds live projection links only, and the task —
	// terminal by definition at that point — can never be reprojected by a
	// stale link.
	Unlink bool
}

type PlaneOutboxStatus struct {
	Pending   int    `json:"pending"`
	Failed    int    `json:"failed"`
	DryRun    int    `json:"dry_run"`
	LastError string `json:"last_error,omitempty"`
}

func (s *Store) EnablePlaneSync() { s.planeSync.Store(true) }

func (s *Store) PlaneSyncEnabled() bool { return s.planeSync.Load() }

func validPlaneOp(op string) bool {
	switch op {
	case PlaneOpWorkItemCreate, PlaneOpWorkItemUpdate, PlaneOpWorkItemRemove:
		return true
	default:
		return false
	}
}

func enqueuePlaneOutboxTx(ctx context.Context, tx pgx.Tx, taskID int64, op string, payload PlaneOutboxPayload) error {
	if !validPlaneOp(op) {
		return errors.New("invalid plane outbox operation")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `INSERT INTO plane_outbox(task_id,seq,op,payload,state,attempts,next_attempt_at,last_error,remote_id,created_at,updated_at)
		SELECT $1,COALESCE(MAX(seq),0)+1,$2,$3::jsonb,'pending',0,$4,'','',$4,$4 FROM plane_outbox WHERE task_id=$1`, taskID, op, string(raw), now)
	return err
}

// enqueuePlaneTaskCreate mirrors a newly created task. Projection failures
// fail closed: the create transaction aborts rather than committing a task
// row the mirror could never represent.
func enqueuePlaneTaskCreate(ctx context.Context, tx pgx.Tx, task Task) error {
	payload, err := PlaneProjectionFor(task)
	if err != nil {
		return err
	}
	return enqueuePlaneOutboxTx(ctx, tx, task.ID, PlaneOpWorkItemCreate, payload)
}

// PlaneTaskTerminal reports whether the task sits outside the mirror's
// selection — the external projection carries non-terminal hk tasks only, so
// merged and dropped items leave the board. Terminal is read off
// LinearTaskStateMapping, the sole mapping table, rather than redeclared.
func PlaneTaskTerminal(task Task) bool {
	mapping, known := LinearTaskStateMapping[task.State]
	return known && mapping.Terminal
}

// enqueuePlaneTaskUpdate mirrors every later change the projection covers —
// transitions, relanes, project changes and disposition moves. Terminal
// transitions enqueue a remove instead of an update: the projection selects
// non-terminal tasks only. Non-terminal payloads are always the full
// snapshot, so each op self-heals any earlier loss and the drain can adopt a
// missing remote item instead of duplicating it.
func enqueuePlaneTaskUpdate(ctx context.Context, tx pgx.Tx, task Task) error {
	if PlaneTaskTerminal(task) {
		payload := PlaneOutboxPayload{ExternalID: PlaneExternalID(task.ID)}
		if task.Project != nil {
			// The project scopes the drain's marker scan when no link row
			// exists; without it the scan would have no project to look in.
			payload.Project = *task.Project
		}
		return enqueuePlaneOutboxTx(ctx, tx, task.ID, PlaneOpWorkItemRemove, payload)
	}
	payload, err := PlaneProjectionFor(task)
	if err != nil {
		return err
	}
	return enqueuePlaneOutboxTx(ctx, tx, task.ID, PlaneOpWorkItemUpdate, payload)
}

// enqueuePlaneFieldUpdate mirrors a non-state field change (relane, project
// reclassify). Tasks already outside the projection — terminal rows, which
// SetTaskProject still accepts — enqueue nothing so a classification edit
// cannot resurrect them on the board.
func enqueuePlaneFieldUpdate(ctx context.Context, tx pgx.Tx, task Task) error {
	if PlaneTaskTerminal(task) {
		return nil
	}
	return enqueuePlaneTaskUpdate(ctx, tx, task)
}

// EnqueuePlaneUpdate re-projects one task through the outbox. The reconciler
// uses it to correct drift (hk wins) — including terminal tasks, for which
// the corrective op is a remove. It runs in its own transaction because
// reconcile passes hold no task row lock.
func (s *Store) EnqueuePlaneUpdate(ctx context.Context, taskID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var task Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1`, taskID), &task); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTaskNotFound
		}
		return err
	}
	if err = enqueuePlaneTaskUpdate(ctx, tx, task); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func scanPlaneOutbox(row interface{ Scan(...any) error }, item *PlaneOutbox) error {
	var raw []byte
	if err := row.Scan(&item.ID, &item.TaskID, &item.Seq, &item.Op, &raw, &item.State, &item.Attempts, &item.NextAttemptAt, &item.LastError, &item.RemoteID, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return err
	}
	return json.Unmarshal(raw, &item.Payload)
}

const planeOutboxColumns = `id,task_id,seq,op,payload,state,attempts,next_attempt_at,last_error,remote_id,created_at,updated_at`

// ProcessNextPlaneOutbox holds the selected row lock through processing. A
// single advisory-lock owner normally calls it, while SKIP LOCKED keeps the
// database boundary safe if a second caller is accidentally introduced.
// 'dryrun' is a terminal state like 'sent': the op was observed and printed
// but deliberately never reached the remote.
func (s *Store) ProcessNextPlaneOutbox(ctx context.Context, process func(context.Context, PlaneOutbox) (PlaneOutboxResult, error)) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var item PlaneOutbox
	err = scanPlaneOutbox(tx.QueryRow(ctx, `SELECT `+planeOutboxColumns+` FROM plane_outbox current
		WHERE state='pending' AND next_attempt_at<=now()
		AND NOT EXISTS (
			SELECT 1 FROM plane_outbox earlier
			WHERE earlier.task_id=current.task_id AND earlier.seq<current.seq
			AND earlier.state NOT IN ('sent','skipped','dryrun')
		)
		ORDER BY task_id ASC,seq ASC FOR UPDATE SKIP LOCKED LIMIT 1`), &item)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := process(ctx, item)
	if err != nil {
		return false, err
	}
	if result.State != "pending" && result.State != "sent" && result.State != "failed" && result.State != "skipped" && result.State != "dryrun" {
		return false, fmt.Errorf("invalid plane outbox result state %q", result.State)
	}
	if result.State == "pending" && result.RetryAt.IsZero() {
		return false, errors.New("pending plane outbox result requires retry time")
	}
	if result.State != "pending" {
		result.RetryAt = item.NextAttemptAt
	}
	now := time.Now().UTC()
	if result.Unlink {
		if _, err = tx.Exec(ctx, `DELETE FROM plane_issues WHERE task_id=$1`, item.TaskID); err != nil {
			return false, err
		}
	}
	if result.RemoteID != "" {
		projectID := result.ProjectID
		if projectID == "" {
			var existing PlaneIssue
			if found, err := func() (bool, error) {
				err := tx.QueryRow(ctx, `SELECT task_id,work_item_id,external_id,project_id,created_at,updated_at FROM plane_issues WHERE task_id=$1`, item.TaskID).Scan(&existing.TaskID, &existing.WorkItemID, &existing.ExternalID, &existing.ProjectID, &existing.CreatedAt, &existing.UpdatedAt)
				if errors.Is(err, pgx.ErrNoRows) {
					return false, nil
				}
				return err == nil, err
			}(); err != nil {
				return false, err
			} else if found {
				projectID = existing.ProjectID
			}
		}
		created := now
		if _, err = tx.Exec(ctx, `INSERT INTO plane_issues(task_id,work_item_id,external_id,project_id,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)
			ON CONFLICT(task_id) DO UPDATE SET work_item_id=EXCLUDED.work_item_id,external_id=EXCLUDED.external_id,project_id=EXCLUDED.project_id,updated_at=EXCLUDED.updated_at`, item.TaskID, result.RemoteID, PlaneExternalID(item.TaskID), projectID, created); err != nil {
			return false, err
		}
	}
	remoteID := result.RemoteID
	if remoteID == "" {
		remoteID = item.RemoteID
	}
	if _, err = tx.Exec(ctx, `UPDATE plane_outbox SET state=$2,attempts=attempts+1,next_attempt_at=$3,last_error=$4,remote_id=$5,updated_at=$6 WHERE id=$1`, item.ID, result.State, result.RetryAt, result.LastError, remoteID, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) GetPlaneIssue(ctx context.Context, taskID int64) (PlaneIssue, bool, error) {
	var issue PlaneIssue
	err := s.pool.QueryRow(ctx, `SELECT task_id,work_item_id,external_id,project_id,created_at,updated_at FROM plane_issues WHERE task_id=$1`, taskID).Scan(&issue.TaskID, &issue.WorkItemID, &issue.ExternalID, &issue.ProjectID, &issue.CreatedAt, &issue.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return issue, false, nil
	}
	return issue, err == nil, err
}

// ListPlaneIssues returns every task-to-work-item link, ordered by task, for
// reconcile passes and the exit plan's copy/compare step.
func (s *Store) ListPlaneIssues(ctx context.Context) ([]PlaneIssue, error) {
	rows, err := s.pool.Query(ctx, `SELECT task_id,work_item_id,external_id,project_id,created_at,updated_at FROM plane_issues ORDER BY task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []PlaneIssue{}
	for rows.Next() {
		var item PlaneIssue
		if err := rows.Scan(&item.TaskID, &item.WorkItemID, &item.ExternalID, &item.ProjectID, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ListPlaneOutbox(ctx context.Context, taskID int64) ([]PlaneOutbox, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+planeOutboxColumns+` FROM plane_outbox WHERE ($1=0 OR task_id=$1) ORDER BY task_id,seq`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []PlaneOutbox{}
	for rows.Next() {
		var item PlaneOutbox
		if err := scanPlaneOutbox(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetPlaneOutboxStatus(ctx context.Context) (PlaneOutboxStatus, error) {
	var status PlaneOutboxStatus
	err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE state='pending'),
		count(*) FILTER (WHERE state='failed'),
		count(*) FILTER (WHERE state='dryrun'),
		COALESCE((SELECT last_error FROM plane_outbox WHERE last_error<>'' ORDER BY updated_at DESC,id DESC LIMIT 1),'')
		FROM plane_outbox`).Scan(&status.Pending, &status.Failed, &status.DryRun, &status.LastError)
	return status, err
}

type PlaneDrainLease struct {
	conn *pgxpool.Conn
}

func (s *Store) TryPlaneDrainLease(ctx context.Context) (*PlaneDrainLease, bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, PlaneDrainAdvisoryLock).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &PlaneDrainLease{conn: conn}, true, nil
}

func (lease *PlaneDrainLease) Release(ctx context.Context) error {
	if lease == nil || lease.conn == nil {
		return nil
	}
	conn := lease.conn
	lease.conn = nil
	defer conn.Release()
	var released bool
	if err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, PlaneDrainAdvisoryLock).Scan(&released); err != nil {
		return err
	}
	if !released {
		return errors.New("plane drain advisory lock was not held")
	}
	return nil
}

package store

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkStatusCounts holds the count of tasks for one project across the state groups.
type WorkStatusCounts struct {
	Backlog               int `json:"backlog"`
	InWork                int `json:"in_work"`
	InReview              int `json:"in_review"`
	WaitingDeploy         int `json:"waiting_deploy"`
	MergedNoDeployProject int `json:"merged_no_deploy_project"`
	MergedBeforeDeploy    int `json:"merged_before_deploy"`
	TotalOpen             int `json:"total_open"`
	BundleParents         int `json:"bundle_parents"`
}

// WorkStatusProject is one project's name and its open task counts.
type WorkStatusProject struct {
	Project string           `json:"project"`
	Counts  WorkStatusCounts `json:"counts"`
}

// WorkStatusSummary is the console-wide aggregate across all projects.
type WorkStatusSummary struct {
	TotalOpen             int `json:"total_open"`
	Backlog               int `json:"backlog"`
	InWork                int `json:"in_work"`
	InReview              int `json:"in_review"`
	WaitingDeploy         int `json:"waiting_deploy"`
	MergedNoDeployProject int `json:"merged_no_deploy_project"`
	MergedBeforeDeploy    int `json:"merged_before_deploy"`
	BundleParents         int `json:"bundle_parents"`
}

// WorkStatusInWorkTask is one task currently being worked on (researching, developing, reviewing).
type WorkStatusInWorkTask struct {
	ID         int64      `json:"id"`
	Title      string     `json:"title"`
	Project    *string    `json:"project"`
	State      string     `json:"state"`
	JobID      string     `json:"job_id,omitempty"`
	JobState   string     `json:"job_state,omitempty"`
	Host       string     `json:"host,omitempty"`
	PR         string     `json:"pr,omitempty"`
	Since      *time.Time `json:"since,omitempty"`
	AgeSeconds int64      `json:"age_seconds"`
}

// WorkStatusBacklogTask is one task in the backlog, ordered by priority and age.
type WorkStatusBacklogTask struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Project     *string   `json:"project"`
	State       string    `json:"state"`
	Priority    int       `json:"priority"`
	CreatedAt   time.Time `json:"created_at"`
	ParentID    *int64    `json:"parent_id,omitempty"`
	BundleID    *int64    `json:"bundle_id,omitempty"`
	BundleTitle string    `json:"bundle_title,omitempty"`
}

// WorkStatusMovementDay records created and closed task counts for one KST day.
type WorkStatusMovementDay struct {
	Date    string `json:"date"` // "YYYY-MM-DD"
	Created int    `json:"created"`
	Closed  int    `json:"closed"`
}

// WorkStatus is the single snapshot document backing GET /ui/api/work-status.
type WorkStatus struct {
	GeneratedAt time.Time               `json:"generated_at"`
	Summary     WorkStatusSummary       `json:"summary"`
	Projects    []WorkStatusProject     `json:"projects"`
	InWork      []WorkStatusInWorkTask  `json:"in_work"`
	Backlog     []WorkStatusBacklogTask `json:"backlog"`
	// Queued is every open task whose spawn request has not ended (hk 4377).
	Queued   []WorkStatusQueuedTask  `json:"queued"`
	Movement []WorkStatusMovementDay `json:"movement"`
}

func isBundleParentTitle(title string) bool {
	return strings.HasPrefix(title, "[bundle ")
}

// WorkStatusRow is one task BuildWorkStatus classifies: an open task or a
// merged one. JobID, Host and PR are already the effective values (job
// table, then refs, then claimed_by or the linked change).
type WorkStatusRow struct {
	ID           int64
	Title        string
	Project      *string
	State        string
	Priority     int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ParentID     *int64
	BundleID     *int64
	BundleTitle  string
	JobID        string
	JobState     string
	Host         string
	PR           string
	InStateSince *time.Time
	MergedAt     *time.Time
}

// WorkStatusInputs is what BuildWorkStatus reads besides the rows: the last
// successful deploy per project, the configured projects, the queued spawns
// and the movement days.
type WorkStatusInputs struct {
	LastDeploy map[string]time.Time
	Projects   []string
	Queued     []WorkStatusQueuedTask
	Movement   []WorkStatusMovementDay
}

// WorkStatusStates are the task states the work status reads: every open
// state that is not icebox, plus merged.
var WorkStatusStates = []string{StateTriage, StateBacklog, StateUnstarted, StateResearching, StateDeveloping, StateReviewing, StateMerged}

// WorkStatusClosedStates are the states the movement counts as closed.
var WorkStatusClosedStates = []string{StateCompleted, StateDropped, StateDuplicate, StateDeployed}

// WorkStatusMovementStart is the first KST day of the movement window: today
// and the 7 days before it. It returns the day and its UTC instant.
func WorkStatusMovementStart(now time.Time) (time.Time, time.Time) {
	kst := time.FixedZone("KST", 9*3600)
	nowKST := now.In(kst)
	todayKST := time.Date(nowKST.Year(), nowKST.Month(), nowKST.Day(), 0, 0, 0, 0, kst)
	startKST := todayKST.AddDate(0, 0, -7)
	return startKST, startKST.UTC()
}

// WorkStatusMovementDays lays the per-day counts (keyed "YYYY-MM-DD" in KST)
// over the 8-day window.
func WorkStatusMovementDays(now time.Time, created, closed map[string]int) []WorkStatusMovementDay {
	startKST, _ := WorkStatusMovementStart(now)
	movement := make([]WorkStatusMovementDay, 0, 8)
	for i := 0; i <= 7; i++ {
		day := startKST.AddDate(0, 0, i).Format("2006-01-02")
		movement = append(movement, WorkStatusMovementDay{Date: day, Created: created[day], Closed: closed[day]})
	}
	return movement
}

// BuildWorkStatus classifies the rows into counts per project and state
// group, the in-work list and the backlog list. It is pure, so the console
// can build the same document from YouTrack's rows (hk 4430). The backlog
// keeps priority DESC, created ASC, id ASC; in-work is newest since first.
func BuildWorkStatus(now time.Time, rows []WorkStatusRow, in WorkStatusInputs) *WorkStatus {
	rows = append([]WorkStatusRow(nil), rows...)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	countsByProject := make(map[string]*WorkStatusCounts)
	projectNames := append([]string(nil), in.Projects...)
	for _, name := range in.Projects {
		countsByProject[name] = &WorkStatusCounts{}
	}
	inWork := []WorkStatusInWorkTask{}
	backlog := []WorkStatusBacklogTask{}
	var summary WorkStatusSummary
	for _, row := range rows {
		pName := ""
		if row.Project != nil {
			pName = *row.Project
		}
		if _, ok := countsByProject[pName]; !ok {
			countsByProject[pName] = &WorkStatusCounts{}
			projectNames = append(projectNames, pName)
		}
		pCounts := countsByProject[pName]
		if isBundleParentTitle(row.Title) {
			pCounts.BundleParents++
			summary.BundleParents++
			// Bundle parents are shown apart and not counted as work.
			continue
		}
		switch row.State {
		case StateTriage, StateBacklog, StateUnstarted:
			pCounts.Backlog++
			pCounts.TotalOpen++
			summary.Backlog++
			summary.TotalOpen++
			task := WorkStatusBacklogTask{ID: row.ID, Title: row.Title, Project: row.Project, State: row.State,
				Priority: row.Priority, CreatedAt: row.CreatedAt, ParentID: row.ParentID}
			if row.BundleID != nil && *row.BundleID > 0 {
				task.BundleID = row.BundleID
				task.BundleTitle = row.BundleTitle
			}
			backlog = append(backlog, task)
		case StateResearching, StateDeveloping, StateReviewing:
			if row.State == StateReviewing {
				pCounts.InReview++
				summary.InReview++
			} else {
				pCounts.InWork++
				summary.InWork++
			}
			pCounts.TotalOpen++
			summary.TotalOpen++
			since := row.InStateSince
			if since == nil {
				updated := row.UpdatedAt
				since = &updated
			}
			age := int64(now.Sub(*since).Seconds())
			if age < 0 {
				age = 0
			}
			inWork = append(inWork, WorkStatusInWorkTask{ID: row.ID, Title: row.Title, Project: row.Project, State: row.State,
				JobID: row.JobID, JobState: row.JobState, Host: row.Host, PR: row.PR, Since: since, AgeSeconds: age})
		case StateMerged:
			// Count a merged task as waiting for deploy only when its project has a deploy
			// record and it merged after that project's last successful deploy. The rest are
			// counted apart: no deploy record for the project, or merged before the last deploy.
			mergedAt := row.UpdatedAt
			if row.MergedAt != nil {
				mergedAt = *row.MergedAt
			}
			lastDeploy, hasDeploy := time.Time{}, false
			if row.Project != nil {
				lastDeploy, hasDeploy = in.LastDeploy[*row.Project]
			}
			switch {
			case !hasDeploy:
				pCounts.MergedNoDeployProject++
				summary.MergedNoDeployProject++
			case mergedAt.After(lastDeploy):
				pCounts.WaitingDeploy++
				pCounts.TotalOpen++
				summary.WaitingDeploy++
				summary.TotalOpen++
			default:
				// Merged before the project's last successful deploy: already live.
				pCounts.MergedBeforeDeploy++
				summary.MergedBeforeDeploy++
			}
		}
	}

	sort.Strings(projectNames)
	projects := make([]WorkStatusProject, 0, len(projectNames))
	for i, name := range projectNames {
		if i > 0 && name == projectNames[i-1] {
			continue
		}
		counts := countsByProject[name]
		if counts == nil {
			counts = &WorkStatusCounts{}
		}
		projects = append(projects, WorkStatusProject{Project: name, Counts: *counts})
	}
	// InWork list ordering: by since DESC (most recently updated/started first), then ID DESC
	sort.SliceStable(inWork, func(i, j int) bool {
		si, sj := inWork[i].Since, inWork[j].Since
		if si != nil && sj != nil && !si.Equal(*sj) {
			return si.After(*sj)
		}
		return inWork[i].ID > inWork[j].ID
	})
	queued := in.Queued
	if queued == nil {
		queued = []WorkStatusQueuedTask{}
	}
	return &WorkStatus{
		GeneratedAt: now.UTC(),
		Summary:     summary,
		Projects:    projects,
		InWork:      inWork,
		Backlog:     backlog,
		Queued:      queued,
		Movement:    in.Movement,
	}
}

// ReadWorkStatus collects open task counts by project and state group,
// in-progress work, backlog list with bundles, movement over today and
// the past 7 days, and honest deploy-waiting tasks in one consistent read.
func (s *Store) ReadWorkStatus(ctx context.Context, now time.Time) (*WorkStatus, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var in WorkStatusInputs
	if in.LastDeploy, err = readProjectLastSuccessfulDeploysTx(ctx, tx); err != nil {
		return nil, err
	}
	if in.Projects, err = readTaskProjectNamesTx(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `SELECT t.id, t.title, t.project, t.state, t.priority, t.refs, t.claimed_by,
		t.created_at, t.updated_at, t.parent_id,
		p.id AS bundle_id, COALESCE(p.title, '') AS bundle_title,
		COALESCE(j.job_id, '') AS job_id, COALESCE(j.host, '') AS host, COALESCE(j.state, '') AS job_state,
		se.at AS in_state_since,
		me.at AS merged_at,
		COALESCE(cp.repo, '') AS change_repo, COALESCE(cp.pr_number, 0) AS change_pr
		FROM tasks t
		LEFT JOIN tasks p ON p.id = t.parent_id
		LEFT JOIN LATERAL (
			SELECT job_id, host, state FROM jobs
			WHERE task_id = t.id
			ORDER BY (state IN ('claimed', 'spawned')) DESC, created_at DESC, job_id DESC
			LIMIT 1
		) j ON true
		LEFT JOIN LATERAL (
			SELECT at FROM task_events
			WHERE task_id = t.id AND kind = 'transition' AND "to" = t.state
			ORDER BY at DESC, id DESC
			LIMIT 1
		) se ON true
		LEFT JOIN LATERAL (
			SELECT at FROM task_events
			WHERE task_id = t.id AND kind = 'transition' AND "to" = 'merged'
			ORDER BY at DESC, id DESC
			LIMIT 1
		) me ON true
		LEFT JOIN LATERAL (
			SELECT c.repo, c.pr_number FROM change_tasks ct
			JOIN changes c ON c.id = ct.change_id
			WHERE ct.task_id = t.id
			ORDER BY (ct.relation = 'implements') DESC, (c.state = 'open') DESC, c.id DESC
			LIMIT 1
		) cp ON true
		WHERE t.state = ANY($1)
		ORDER BY t.priority DESC, t.created_at ASC, t.id ASC`, WorkStatusStates)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []WorkStatusRow
	for rows.Next() {
		var row WorkStatusRow
		var refsRaw []byte
		var claimedBy, changeRepo string
		var changePR int
		if err := rows.Scan(&row.ID, &row.Title, &row.Project, &row.State, &row.Priority, &refsRaw, &claimedBy,
			&row.CreatedAt, &row.UpdatedAt, &row.ParentID, &row.BundleID, &row.BundleTitle, &row.JobID, &row.Host, &row.JobState,
			&row.InStateSince, &row.MergedAt, &changeRepo, &changePR); err != nil {
			return nil, err
		}
		var refs TaskRefs
		if len(refsRaw) > 0 {
			_ = json.Unmarshal(refsRaw, &refs)
		}
		row.JobID, row.Host, row.PR = effectiveWorkRefs(row.JobID, row.Host, refs, claimedBy, changeRepo, changePR)
		list = append(list, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Movement: closed and created per day, today and the last 7 days (8 KST days total).
	_, startUTC := WorkStatusMovementStart(now)
	closedCounts, err := countByKSTDayTx(ctx, tx, `SELECT (e.at AT TIME ZONE 'Asia/Seoul')::date::text AS day, COUNT(DISTINCT e.task_id)
		FROM task_events e
		JOIN tasks t ON t.id = e.task_id
		WHERE e.kind = 'transition'
		  AND e."to" = ANY($2)
		  AND e.at >= $1
		  AND NOT (t.title LIKE '[bundle %')
		GROUP BY day`, startUTC, WorkStatusClosedStates)
	if err != nil {
		return nil, err
	}
	createdCounts, err := countByKSTDayTx(ctx, tx, `SELECT (t.created_at AT TIME ZONE 'Asia/Seoul')::date::text AS day, COUNT(*)
		FROM tasks t
		WHERE t.created_at >= $1
		  AND NOT (t.title LIKE '[bundle %')
		GROUP BY day`, startUTC)
	if err != nil {
		return nil, err
	}
	in.Movement = WorkStatusMovementDays(now, createdCounts, closedCounts)
	if in.Queued, err = readQueuedSpawnsTx(ctx, tx, true); err != nil {
		return nil, err
	}
	return BuildWorkStatus(now, list, in), nil
}

// effectiveWorkRefs picks the job, host and PR a work row shows: the job
// table first, then the task's refs, then claimed_by for the host and the
// newest linked change for the PR.
func effectiveWorkRefs(jobID, host string, refs TaskRefs, claimedBy, changeRepo string, changePR int) (string, string, string) {
	if jobID == "" {
		jobID = refs.JobID
	}
	if host == "" {
		host = claimedBy
	}
	pr := refs.PR
	if pr == "" && changePR > 0 {
		pr = changeURL(changeRepo, changePR)
	}
	return jobID, host, pr
}

func readTaskProjectNamesTx(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT name FROM task_projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func countByKSTDayTx(ctx context.Context, tx pgx.Tx, sql string, args ...any) (map[string]int, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var day string
		var count int
		if err := rows.Scan(&day, &count); err != nil {
			return nil, err
		}
		out[day] = count
	}
	return out, rows.Err()
}

// WorkStatusTaskOverlay is the hk-only part of one task on the work status
// page: who works on it (job, host), its PR and its bundle parent. When it
// entered a state is YouTrack's (its activities), not hk's.
type WorkStatusTaskOverlay struct {
	ParentID    *int64
	ParentTitle string
	JobID       string
	JobState    string
	Host        string
	PR          string
}

// WorkStatusOverlay is what the YouTrack-backed work status reads from hk
// (hk 4430): the deploy records, the configured projects, the queued spawns
// (hk 4377, every task whose request is in flight, whatever hk's copy of its
// state: YouTrack says whether it is open) and the per-task overlay. Movement
// and state times are the caller's, from YouTrack.
type WorkStatusOverlay struct {
	Inputs WorkStatusInputs
	Tasks  map[int64]WorkStatusTaskOverlay
}

// ReadWorkStatusOverlay reads the hk side of the work status for ids in one
// consistent read. A task id hk does not know has no overlay row.
func (s *Store) ReadWorkStatusOverlay(ctx context.Context, ids []int64) (WorkStatusOverlay, error) {
	out := WorkStatusOverlay{Tasks: map[int64]WorkStatusTaskOverlay{}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if out.Inputs.LastDeploy, err = readProjectLastSuccessfulDeploysTx(ctx, tx); err != nil {
		return out, err
	}
	if out.Inputs.Projects, err = readTaskProjectNamesTx(ctx, tx); err != nil {
		return out, err
	}
	if out.Inputs.Queued, err = readQueuedSpawnsTx(ctx, tx, false); err != nil {
		return out, err
	}
	if len(ids) > 0 {
		rows, err := tx.Query(ctx, `SELECT t.id, t.refs, t.claimed_by, t.parent_id, COALESCE(p.title, ''),
			COALESCE(j.job_id, ''), COALESCE(j.host, ''), COALESCE(j.state, ''),
			COALESCE(cp.repo, ''), COALESCE(cp.pr_number, 0)
			FROM tasks t
			LEFT JOIN tasks p ON p.id = t.parent_id
			LEFT JOIN LATERAL (
				SELECT job_id, host, state FROM jobs
				WHERE task_id = t.id
				ORDER BY (state IN ('claimed', 'spawned')) DESC, created_at DESC, job_id DESC
				LIMIT 1
			) j ON true
			LEFT JOIN LATERAL (
				SELECT c.repo, c.pr_number FROM change_tasks ct
				JOIN changes c ON c.id = ct.change_id
				WHERE ct.task_id = t.id
				ORDER BY (ct.relation = 'implements') DESC, (c.state = 'open') DESC, c.id DESC
				LIMIT 1
			) cp ON true
			WHERE t.id = ANY($1)`, ids)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var id int64
			var x WorkStatusTaskOverlay
			var refsRaw []byte
			var claimedBy, changeRepo string
			var changePR int
			if err := rows.Scan(&id, &refsRaw, &claimedBy, &x.ParentID, &x.ParentTitle, &x.JobID, &x.Host, &x.JobState, &changeRepo, &changePR); err != nil {
				rows.Close()
				return out, err
			}
			var refs TaskRefs
			if len(refsRaw) > 0 {
				_ = json.Unmarshal(refsRaw, &refs)
			}
			x.JobID, x.Host, x.PR = effectiveWorkRefs(x.JobID, x.Host, refs, claimedBy, changeRepo, changePR)
			out.Tasks[id] = x
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, err
		}
	}
	return out, tx.Commit(ctx)
}

// readProjectLastSuccessfulDeploysTx scans active deploy services and records
// to find the most recent successful deploy timestamp per project.
func readProjectLastSuccessfulDeploysTx(ctx context.Context, tx pgx.Tx) (map[string]time.Time, error) {
	return readProjectLastSuccessfulDeploys(ctx, tx)
}

func readProjectLastSuccessfulDeploys(ctx context.Context, tx pgx.Tx) (map[string]time.Time, error) {
	result := make(map[string]time.Time)

	// Fetch active deploy services
	sRows, err := tx.Query(ctx, `SELECT name, repo, record_prefixes FROM deploy_services WHERE active = true ORDER BY name`)
	if err != nil {
		return nil, err
	}
	type svcEntry struct {
		name     string
		repo     string
		prefixes []string
	}
	var services []svcEntry
	for sRows.Next() {
		var s svcEntry
		if err := sRows.Scan(&s.name, &s.repo, &s.prefixes); err != nil {
			sRows.Close()
			return nil, err
		}
		services = append(services, s)
	}
	sRows.Close()
	if err := sRows.Err(); err != nil {
		return nil, err
	}

	// A service's own name is its canonical key. A repo base name that equals
	// another active service's name must not overwrite that service's boundary:
	// DeployLagStatus and EnqueueDeployLagNotices read the map by service name.
	serviceNames := make(map[string]bool, len(services))
	for _, svc := range services {
		serviceNames[svc.name] = true
	}

	for _, svc := range services {
		var newestSuccess *time.Time
		for _, prefix := range svc.prefixes {
			// A prefix may be legacy-named while the document uses the canonical
			// configured service name (for example deploy/panewire/ for panewire-hub).
			alias := strings.TrimSuffix(prefix, "/")
			if i := strings.LastIndex(alias, "/"); i >= 0 {
				alias = alias[i+1:]
			}
			services := []string{svc.name}
			if alias != svc.name {
				services = append(services, alias)
			}
			t, err := scanPrefixLastSuccessDeployTx(ctx, tx, prefix, services)
			if err != nil {
				return nil, err
			}
			if t != nil && (newestSuccess == nil || t.After(*newestSuccess)) {
				newestSuccess = t
			}
		}
		if newestSuccess != nil {
			result[svc.name] = *newestSuccess
			// Map repo base name as well (e.g. mgh3326/panewire -> panewire)
			if idx := strings.LastIndex(svc.repo, "/"); idx >= 0 && idx+1 < len(svc.repo) {
				repoBase := svc.repo[idx+1:]
				if !serviceNames[repoBase] {
					result[repoBase] = *newestSuccess
				}
			}
		}
	}

	return result, nil
}

// lastSuccessPageSize is how many documents one page of the last-success scan
// reads. It bounds one query, not the scan: scanPrefixLastSuccessDeployTx
// keeps paging until it finds a success or runs out of documents.
const lastSuccessPageSize = 200

// scanPrefixLastSuccessDeployTx returns the newest successful deploy under one
// record prefix. It reads newest-key-first pages and stops at the first page
// that holds a success, so a long run of newer failed or invalid records
// cannot hide the last success: a service is reported as never deployed only
// when its whole prefix has no success.
func scanPrefixLastSuccessDeployTx(ctx context.Context, tx pgx.Tx, prefix string, services []string) (*time.Time, error) {
	after := ""
	for {
		newest, lastKey, n, err := scanPrefixLastSuccessPageTx(ctx, tx, prefix, services, after)
		if err != nil {
			return nil, err
		}
		if newest != nil || n < lastSuccessPageSize {
			return newest, nil
		}
		after = lastKey
	}
}

// scanPrefixLastSuccessPageTx reads one page of documents under prefix whose
// key sorts before after (all of them when after is empty), newest key first.
// It returns the newest success on the page, the last key read and the row
// count; the keys are unique, so the last key is a stable cursor.
func scanPrefixLastSuccessPageTx(ctx context.Context, tx pgx.Tx, prefix string, services []string, after string) (*time.Time, string, int, error) {
	rows, err := tx.Query(ctx, `SELECT d.key, d.sha256, d.created_at, p.record, p.decoded,
		CASE WHEN p.key IS NULL THEN d.body ELSE NULL END
		FROM documents d
		LEFT JOIN deploy_record_projection p ON p.key=d.key AND p.sha256=d.sha256
		WHERE strpos(d.key, $1) = 1 AND ($2::text = '' OR d.key < $2::text)
		ORDER BY d.key DESC
		LIMIT $3`, prefix, after, lastSuccessPageSize)
	if err != nil {
		return nil, "", 0, err
	}
	defer rows.Close()

	var newest *time.Time
	var lastKey string
	n := 0
	for rows.Next() {
		var key, sha256 string
		var createdAt time.Time
		var raw []byte
		var decoded *bool
		var body *string

		if err := rows.Scan(&key, &sha256, &createdAt, &raw, &decoded, &body); err != nil {
			return nil, "", 0, err
		}
		lastKey = key
		n++

		var rec *DeployRecord
		var deployedAt *time.Time
		var reasons []string

		if body != nil {
			for _, service := range services {
				rec, deployedAt, reasons = ClassifyDeployRecord(key, service, *body)
				if rec != nil && len(reasons) == 0 {
					break
				}
			}
		} else if raw != nil && decoded != nil && *decoded {
			var parsed DeployRecord
			if err := json.Unmarshal(raw, &parsed); err == nil {
				for _, service := range services {
					rec, deployedAt, reasons = classifyParsedDeployRecord(key, service, &parsed, true)
					if rec != nil && len(reasons) == 0 {
						break
					}
				}
			}
		}

		if len(reasons) > 0 || rec == nil {
			continue
		}
		if rec.Result != "success" {
			continue
		}

		t := deployedAt
		if t == nil {
			kt, ok := DeployRecordKeyTime(key)
			if ok {
				t = &kt
			} else {
				t = &createdAt
			}
		}

		if newest == nil || t.After(*newest) {
			newest = t
		}
	}

	return newest, lastKey, n, rows.Err()
}

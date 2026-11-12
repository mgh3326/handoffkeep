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
	Backlog              int `json:"backlog"`
	InWork               int `json:"in_work"`
	InReview             int `json:"in_review"`
	WaitingDeploy        int `json:"waiting_deploy"`
	MergedNoDeployRecord int `json:"merged_no_deploy_record"`
	TotalOpen            int `json:"total_open"`
	BundleParents        int `json:"bundle_parents"`
}

// WorkStatusProject is one project's name and its open task counts.
type WorkStatusProject struct {
	Project string           `json:"project"`
	Counts  WorkStatusCounts `json:"counts"`
}

// WorkStatusSummary is the console-wide aggregate across all projects.
type WorkStatusSummary struct {
	TotalOpen            int `json:"total_open"`
	Backlog              int `json:"backlog"`
	InWork               int `json:"in_work"`
	InReview             int `json:"in_review"`
	WaitingDeploy        int `json:"waiting_deploy"`
	MergedNoDeployRecord int `json:"merged_no_deploy_record"`
	BundleParents        int `json:"bundle_parents"`
}

// WorkStatusInWorkTask is one task currently being worked on (researching, developing, reviewing).
type WorkStatusInWorkTask struct {
	ID         int64      `json:"id"`
	Title      string     `json:"title"`
	Project    *string    `json:"project"`
	State      string     `json:"state"`
	JobID      string     `json:"job_id,omitempty"`
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
	Movement    []WorkStatusMovementDay `json:"movement"`
}

func isBundleParentTitle(title string) bool {
	return strings.HasPrefix(title, "[bundle ")
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

	// 1. Discover the last successful deploy timestamp per project.
	lastDeploy, err := readProjectLastSuccessfulDeploysTx(ctx, tx)
	if err != nil {
		return nil, err
	}

	// 2. Fetch known configured projects.
	pRows, err := tx.Query(ctx, `SELECT name FROM task_projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	configuredProjects := make(map[string]bool)
	var projectNames []string
	for pRows.Next() {
		var name string
		if err := pRows.Scan(&name); err != nil {
			pRows.Close()
			return nil, err
		}
		configuredProjects[name] = true
		projectNames = append(projectNames, name)
	}
	pRows.Close()
	if err := pRows.Err(); err != nil {
		return nil, err
	}

	// 3. Query all non-settled tasks plus merged tasks.
	// That is: state IN ('triage','backlog','unstarted','researching','developing','reviewing','merged')
	rows, err := tx.Query(ctx, `SELECT t.id, t.title, t.project, t.state, t.priority, t.refs, t.claimed_by,
		t.created_at, t.updated_at, t.parent_id,
		p.id AS bundle_id, COALESCE(p.title, '') AS bundle_title,
		COALESCE(j.job_id, '') AS job_id, COALESCE(j.host, '') AS host,
		se.at AS in_state_since,
		me.at AS merged_at
		FROM tasks t
		LEFT JOIN tasks p ON p.id = t.parent_id
		LEFT JOIN LATERAL (
			SELECT job_id, host FROM jobs
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
		WHERE t.state IN ('triage','backlog','unstarted','researching','developing','reviewing','merged')
		ORDER BY t.priority DESC, t.created_at ASC, t.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	countsByProject := make(map[string]*WorkStatusCounts)
	for name := range configuredProjects {
		countsByProject[name] = &WorkStatusCounts{}
	}

	var inWork []WorkStatusInWorkTask
	var backlog []WorkStatusBacklogTask
	var summary WorkStatusSummary

	for rows.Next() {
		var id int64
		var title, state string
		var project *string
		var priority int
		var refsRaw []byte
		var claimedBy string
		var createdAt, updatedAt time.Time
		var parentID, bundleID *int64
		var bundleTitle, jobID, host string
		var inStateSince, mergedAt *time.Time

		if err := rows.Scan(&id, &title, &project, &state, &priority, &refsRaw, &claimedBy,
			&createdAt, &updatedAt, &parentID, &bundleID, &bundleTitle, &jobID, &host,
			&inStateSince, &mergedAt); err != nil {
			return nil, err
		}

		var refs TaskRefs
		if len(refsRaw) > 0 {
			_ = json.Unmarshal(refsRaw, &refs)
		}

		pName := ""
		if project != nil {
			pName = *project
		}
		if _, ok := countsByProject[pName]; !ok {
			countsByProject[pName] = &WorkStatusCounts{}
			projectNames = append(projectNames, pName)
		}
		pCounts := countsByProject[pName]

		isBundle := isBundleParentTitle(title)

		if isBundle {
			pCounts.BundleParents++
			summary.BundleParents++
			// Bundle parents are shown apart and not counted as work.
			continue
		}

		switch state {
		case StateTriage, StateBacklog, StateUnstarted:
			pCounts.Backlog++
			pCounts.TotalOpen++
			summary.Backlog++
			summary.TotalOpen++

			bTask := WorkStatusBacklogTask{
				ID:        id,
				Title:     title,
				Project:   project,
				State:     state,
				Priority:  priority,
				CreatedAt: createdAt,
				ParentID:  parentID,
			}
			if bundleID != nil && *bundleID > 0 {
				bTask.BundleID = bundleID
				bTask.BundleTitle = bundleTitle
			}
			backlog = append(backlog, bTask)

		case StateResearching, StateDeveloping:
			pCounts.InWork++
			pCounts.TotalOpen++
			summary.InWork++
			summary.TotalOpen++

			since := inStateSince
			if since == nil {
				since = &updatedAt
			}
			ageSec := int64(now.Sub(*since).Seconds())
			if ageSec < 0 {
				ageSec = 0
			}

			// Priority order for job/host attribution: job table, task.refs, claimed_by
			effJobID := jobID
			if effJobID == "" && refs.JobID != "" {
				effJobID = refs.JobID
			}
			effHost := host
			if effHost == "" && claimedBy != "" {
				effHost = claimedBy
			}

			inWork = append(inWork, WorkStatusInWorkTask{
				ID:         id,
				Title:      title,
				Project:    project,
				State:      state,
				JobID:      effJobID,
				Host:       effHost,
				PR:         refs.PR,
				Since:      since,
				AgeSeconds: ageSec,
			})

		case StateReviewing:
			pCounts.InReview++
			pCounts.TotalOpen++
			summary.InReview++
			summary.TotalOpen++

			since := inStateSince
			if since == nil {
				since = &updatedAt
			}
			ageSec := int64(now.Sub(*since).Seconds())
			if ageSec < 0 {
				ageSec = 0
			}

			effJobID := jobID
			if effJobID == "" && refs.JobID != "" {
				effJobID = refs.JobID
			}
			effHost := host
			if effHost == "" && claimedBy != "" {
				effHost = claimedBy
			}

			inWork = append(inWork, WorkStatusInWorkTask{
				ID:         id,
				Title:      title,
				Project:    project,
				State:      state,
				JobID:      effJobID,
				Host:       effHost,
				PR:         refs.PR,
				Since:      since,
				AgeSeconds: ageSec,
			})

		case StateMerged:
			// Item 5: count merged tasks only when their project has a deploy record and
			// their change merged after that project's last successful deploy;
			// show the others as "머지됨 (배포 기록 없음)" with their count, not as waiting.
			effMergedAt := mergedAt
			if effMergedAt == nil {
				effMergedAt = &updatedAt
			}

			var isWaitingDeploy bool
			if project != nil {
				if lastDep, ok := lastDeploy[*project]; ok {
					if effMergedAt.After(lastDep) {
						isWaitingDeploy = true
					}
				}
			}

			if isWaitingDeploy {
				pCounts.WaitingDeploy++
				pCounts.TotalOpen++
				summary.WaitingDeploy++
				summary.TotalOpen++
			} else {
				pCounts.MergedNoDeployRecord++
				summary.MergedNoDeployRecord++
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 4. Movement: closed and created per day, today and the last 7 days (8 KST days total).
	kst := time.FixedZone("KST", 9*3600)
	nowKST := now.In(kst)
	todayKST := time.Date(nowKST.Year(), nowKST.Month(), nowKST.Day(), 0, 0, 0, 0, kst)
	startKST := todayKST.AddDate(0, 0, -7)
	startUTC := startKST.UTC()

	closedCounts := make(map[string]int)
	cRows, err := tx.Query(ctx, `SELECT (e.at AT TIME ZONE 'Asia/Seoul')::date::text AS day, COUNT(DISTINCT e.task_id)
		FROM task_events e
		JOIN tasks t ON t.id = e.task_id
		WHERE e.kind = 'transition'
		  AND e."to" IN ('completed','dropped','duplicate','deployed')
		  AND e.at >= $1
		  AND NOT (t.title LIKE '[bundle %')
		GROUP BY day`, startUTC)
	if err != nil {
		return nil, err
	}
	for cRows.Next() {
		var day string
		var count int
		if err := cRows.Scan(&day, &count); err != nil {
			cRows.Close()
			return nil, err
		}
		closedCounts[day] = count
	}
	cRows.Close()
	if err := cRows.Err(); err != nil {
		return nil, err
	}

	createdCounts := make(map[string]int)
	crRows, err := tx.Query(ctx, `SELECT (t.created_at AT TIME ZONE 'Asia/Seoul')::date::text AS day, COUNT(*)
		FROM tasks t
		WHERE t.created_at >= $1
		  AND NOT (t.title LIKE '[bundle %')
		GROUP BY day`, startUTC)
	if err != nil {
		return nil, err
	}
	for crRows.Next() {
		var day string
		var count int
		if err := crRows.Scan(&day, &count); err != nil {
			crRows.Close()
			return nil, err
		}
		createdCounts[day] = count
	}
	crRows.Close()
	if err := crRows.Err(); err != nil {
		return nil, err
	}

	var movement []WorkStatusMovementDay
	for i := 0; i <= 7; i++ {
		d := startKST.AddDate(0, 0, i)
		dStr := d.Format("2006-01-02")
		movement = append(movement, WorkStatusMovementDay{
			Date:    dStr,
			Created: createdCounts[dStr],
			Closed:  closedCounts[dStr],
		})
	}

	// 5. Assemble and sort projects list.
	sort.Strings(projectNames)
	// Deduplicate projectNames
	var uniqueProjects []string
	for i, name := range projectNames {
		if i == 0 || name != projectNames[i-1] {
			uniqueProjects = append(uniqueProjects, name)
		}
	}

	projects := make([]WorkStatusProject, 0, len(uniqueProjects))
	for _, name := range uniqueProjects {
		counts := countsByProject[name]
		if counts == nil {
			counts = &WorkStatusCounts{}
		}
		projects = append(projects, WorkStatusProject{
			Project: name,
			Counts:  *counts,
		})
	}

	// InWork list ordering: by since DESC (most recently updated/started first), then ID DESC
	sort.SliceStable(inWork, func(i, j int) bool {
		si, sj := inWork[i].Since, inWork[j].Since
		if si != nil && sj != nil && !si.Equal(*sj) {
			return si.After(*sj)
		}
		return inWork[i].ID > inWork[j].ID
	})

	return &WorkStatus{
		GeneratedAt: now.UTC(),
		Summary:     summary,
		Projects:    projects,
		InWork:      inWork,
		Backlog:     backlog,
		Movement:    movement,
	}, nil
}

// readProjectLastSuccessfulDeploysTx scans active deploy services and records
// to find the most recent successful deploy timestamp per project.
func readProjectLastSuccessfulDeploysTx(ctx context.Context, tx pgx.Tx) (map[string]time.Time, error) {
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

	for _, svc := range services {
		var newestSuccess *time.Time
		for _, prefix := range svc.prefixes {
			t, err := scanPrefixLastSuccessDeployTx(ctx, tx, prefix, svc.name)
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
				result[repoBase] = *newestSuccess
			}
		}
	}

	return result, nil
}

func scanPrefixLastSuccessDeployTx(ctx context.Context, tx pgx.Tx, prefix, service string) (*time.Time, error) {
	rows, err := tx.Query(ctx, `SELECT d.key, d.sha256, d.created_at, p.record, p.decoded,
		CASE WHEN p.key IS NULL THEN d.body ELSE NULL END
		FROM documents d
		LEFT JOIN deploy_record_projection p ON p.key=d.key AND p.sha256=d.sha256
		WHERE strpos(d.key, $1) = 1
		ORDER BY d.key DESC
		LIMIT 200`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var newest *time.Time
	for rows.Next() {
		var key, sha256 string
		var createdAt time.Time
		var raw []byte
		var decoded *bool
		var body *string

		if err := rows.Scan(&key, &sha256, &createdAt, &raw, &decoded, &body); err != nil {
			return nil, err
		}

		var rec *DeployRecord
		var deployedAt *time.Time
		var reasons []string

		if body != nil {
			rec, deployedAt, reasons = ClassifyDeployRecord(key, service, *body)
		} else if raw != nil && decoded != nil && *decoded {
			var parsed DeployRecord
			if err := json.Unmarshal(raw, &parsed); err == nil {
				rec, deployedAt, reasons = classifyParsedDeployRecord(key, service, &parsed, true)
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

	return newest, rows.Err()
}

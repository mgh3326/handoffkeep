package store

import "context"

// ActiveJobSummary is the console-safe context of a task's newest live job.
type ActiveJobSummary struct {
	Role    string  `json:"role"`
	Model   string  `json:"model"`
	Profile string  `json:"profile"`
	Host    string  `json:"host"`
	Harness *string `json:"harness"`
	Effort  *string `json:"effort"`
}

// ReadActiveJobs returns at most one live job per task, ordered by creation
// time and job ID so ties have a deterministic winner. It reads the page in
// one query without fetching terminal jobs or their payloads.
func (s *Store) ReadActiveJobs(ctx context.Context, taskIDs []int64) (map[int64]ActiveJobSummary, error) {
	result := make(map[int64]ActiveJobSummary, len(taskIDs))
	if len(taskIDs) == 0 {
		return result, nil
	}
	query := `SELECT DISTINCT ON (task_id) task_id, role, model, profile, host, harness, effort
 FROM jobs WHERE task_id = ANY($1) AND state IN ('claimed', 'spawned')
		ORDER BY task_id, created_at DESC, job_id DESC`
	args := []any{taskIDs}
	if s.dialect == mysqlDialect {
		query = `SELECT j.task_id,j.role,j.model,j.profile,j.host,j.harness,j.effort FROM jobs j
			WHERE j.task_id IN (` + placeholders(len(taskIDs)) + `) AND j.state IN ('claimed','spawned')
			AND NOT EXISTS (SELECT 1 FROM jobs newer WHERE newer.task_id=j.task_id AND newer.state IN ('claimed','spawned')
				AND (newer.created_at>j.created_at OR (newer.created_at=j.created_at AND newer.job_id>j.job_id)))
			ORDER BY j.task_id,j.created_at DESC,j.job_id DESC`
		args = make([]any, len(taskIDs))
		for i, id := range taskIDs {
			args[i] = id
		}
	}
	rows, err := s.db().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var job ActiveJobSummary
		if err := rows.Scan(&id, &job.Role, &job.Model, &job.Profile, &job.Host, &job.Harness, &job.Effort); err != nil {
			return nil, err
		}
		result[id] = job
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

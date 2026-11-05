package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type JobSkillsReport struct {
	Since                 time.Time        `json:"since"`
	JobsTotal             int              `json:"jobs_total"`
	JobsSkillsSupported   int              `json:"jobs_skills_supported"`
	JobsSkillsUnsupported int              `json:"jobs_skills_unsupported"`
	JobsSkillsUnknown     int              `json:"jobs_skills_unknown"`
	ByHarnessModel        []JobSkillsGroup `json:"by_harness_model"`
	NeverUsed             []JobSkillCount  `json:"never_used"`
	JobsNoSkill           JobNoSkillList   `json:"jobs_no_skill"`
}
type JobSkillCount struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Jobs   int    `json:"jobs"`
	Calls  int64  `json:"calls,omitempty"`
}
type JobSkillsGroup struct {
	Harness string              `json:"harness"`
	Model   string              `json:"model"`
	Jobs    int                 `json:"jobs"`
	Skills  []JobSkillAggregate `json:"skills"`
}
type JobSkillAggregate struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Jobs   int    `json:"jobs"`
	Calls  int64  `json:"calls"`
}
type JobNoSkill struct {
	JobID     string    `json:"job_id"`
	TaskID    *int64    `json:"task_id"`
	Harness   string    `json:"harness"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
}
type JobNoSkillList struct {
	Count     int          `json:"count"`
	Truncated bool         `json:"truncated"`
	Jobs      []JobNoSkill `json:"jobs"`
}

func (s *Store) JobSkillsReport(ctx context.Context, since time.Time, limit int) (JobSkillsReport, error) {
	out := JobSkillsReport{Since: since, ByHarnessModel: []JobSkillsGroup{}, NeverUsed: []JobSkillCount{}, JobsNoSkill: JobNoSkillList{Jobs: []JobNoSkill{}}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE skills_supported IS TRUE), count(*) FILTER (WHERE skills_supported IS FALSE), count(*) FILTER (WHERE skills_supported IS NULL) FROM jobs WHERE created_at >= $1`, since).Scan(&out.JobsTotal, &out.JobsSkillsSupported, &out.JobsSkillsUnsupported, &out.JobsSkillsUnknown)
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT COALESCE(harness,''),COALESCE(model,''),count(*) FROM jobs WHERE created_at >= $1 GROUP BY COALESCE(harness,''),COALESCE(model,'') ORDER BY 1,2`, since)
	if err != nil {
		return out, err
	}
	type key = [2]string
	groups := map[key]int{}
	for rows.Next() {
		var g JobSkillsGroup
		g.Skills = []JobSkillAggregate{}
		if err = rows.Scan(&g.Harness, &g.Model, &g.Jobs); err != nil {
			rows.Close()
			return out, err
		}
		groups[key{g.Harness, g.Model}] = len(out.ByHarnessModel)
		out.ByHarnessModel = append(out.ByHarnessModel, g)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT COALESCE(j.harness,''),COALESCE(j.model,''),x.skill->>'name',x.skill->>'source',count(*),sum((x.skill->>'calls')::bigint)::bigint AS calls FROM jobs j CROSS JOIN LATERAL jsonb_array_elements(COALESCE(j.skills,'[]'::jsonb)) AS x(skill) WHERE j.created_at >= $1 GROUP BY 1,2,3,4 ORDER BY 1,2,calls DESC,3,4`, since)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var harness, model string
		var skill JobSkillAggregate
		if err = rows.Scan(&harness, &model, &skill.Name, &skill.Source, &skill.Jobs, &skill.Calls); err != nil {
			rows.Close()
			return out, err
		}
		i := groups[key{harness, model}]
		out.ByHarnessModel[i].Skills = append(out.ByHarnessModel[i].Skills, skill)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT x.skill->>'name',x.skill->>'source',count(*) FROM jobs j CROSS JOIN LATERAL jsonb_array_elements(COALESCE(j.skills,'[]'::jsonb)) AS x(skill) WHERE j.created_at >= $1 GROUP BY 1,2 HAVING sum((x.skill->>'calls')::bigint)=0 ORDER BY 1,2`, since)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var skill JobSkillCount
		if err = rows.Scan(&skill.Name, &skill.Source, &skill.Jobs); err != nil {
			rows.Close()
			return out, err
		}
		out.NeverUsed = append(out.NeverUsed, skill)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	const noSkill = `created_at >= $1 AND skills_supported IS TRUE AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(skills,'[]'::jsonb)) AS x(skill) WHERE (x.skill->>'calls')::bigint > 0)`
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE `+noSkill, since).Scan(&out.JobsNoSkill.Count); err != nil {
		return out, err
	}
	rows, err = tx.Query(ctx, `SELECT job_id,task_id,COALESCE(harness,''),COALESCE(model,''),created_at FROM jobs WHERE `+noSkill+` ORDER BY created_at DESC,job_id DESC LIMIT $2`, since, limit)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var job JobNoSkill
		if err = rows.Scan(&job.JobID, &job.TaskID, &job.Harness, &job.Model, &job.CreatedAt); err != nil {
			rows.Close()
			return out, err
		}
		out.JobsNoSkill.Jobs = append(out.JobsNoSkill.Jobs, job)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	out.JobsNoSkill.Truncated = out.JobsNoSkill.Count > len(out.JobsNoSkill.Jobs)
	return out, tx.Commit(ctx)
}

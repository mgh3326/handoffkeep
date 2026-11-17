package store

import (
	"context"
	"encoding/json"
	"time"
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
	Name   string      `json:"name"`
	Source string      `json:"source"`
	Jobs   int         `json:"jobs"`
	Calls  json.Number `json:"calls"`
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
	tx, err := s.beginReportRead(ctx, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	countsSQL := `SELECT count(*), count(*) FILTER (WHERE skills_supported IS TRUE), count(*) FILTER (WHERE skills_supported IS FALSE), count(*) FILTER (WHERE skills_supported IS NULL) FROM jobs WHERE created_at >= $1`
	if s.dialect == mysqlDialect {
		countsSQL = `SELECT count(*), SUM(skills_supported IS TRUE), SUM(skills_supported IS FALSE), SUM(skills_supported IS NULL) FROM jobs WHERE created_at >= ?`
	}
	err = tx.QueryRow(ctx, countsSQL, since).Scan(&out.JobsTotal, &out.JobsSkillsSupported, &out.JobsSkillsUnsupported, &out.JobsSkillsUnknown)
	if err != nil {
		return out, err
	}
	groupsSQL := `SELECT COALESCE(harness,''),COALESCE(model,''),count(*) FROM jobs WHERE created_at >= $1 GROUP BY COALESCE(harness,''),COALESCE(model,'') ORDER BY 1,2`
	if s.dialect == mysqlDialect {
		groupsSQL = `SELECT COALESCE(harness,''),COALESCE(model,''),count(*) FROM jobs WHERE created_at >= ? GROUP BY COALESCE(harness,''),COALESCE(model,'') ORDER BY 1,2`
	}
	rows, err := tx.Query(ctx, groupsSQL, since)
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
	skillsSQL := `SELECT COALESCE(j.harness,''),COALESCE(j.model,''),x.skill->>'name',x.skill->>'source',count(*),sum((x.skill->>'calls')::bigint)::text AS calls FROM jobs j CROSS JOIN LATERAL jsonb_array_elements(COALESCE(j.skills,'[]'::jsonb)) AS x(skill) WHERE j.created_at >= $1 GROUP BY 1,2,3,4 ORDER BY 1,2,sum((x.skill->>'calls')::bigint) DESC,3,4`
	if s.dialect == mysqlDialect {
		skillsSQL = `SELECT COALESCE(j.harness,''),COALESCE(j.model,''),x.skill_name,x.skill_source,count(*),CAST(sum(x.skill_calls) AS CHAR) AS calls
			FROM jobs j JOIN JSON_TABLE(COALESCE(j.skills,JSON_ARRAY()),'$[*]' COLUMNS(skill_name VARCHAR(255) PATH '$.name' NULL ON EMPTY,skill_source VARCHAR(255) PATH '$.source' NULL ON EMPTY,skill_calls BIGINT PATH '$.calls' DEFAULT 0 ON EMPTY)) x
			WHERE j.created_at>=? GROUP BY 1,2,3,4 ORDER BY 1,2,sum(x.skill_calls) DESC,3,4`
	}
	rows, err = tx.Query(ctx, skillsSQL, since)
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
	neverUsedSQL := `SELECT x.skill->>'name',x.skill->>'source',count(*) FROM jobs j CROSS JOIN LATERAL jsonb_array_elements(COALESCE(j.skills,'[]'::jsonb)) AS x(skill) WHERE j.created_at >= $1 GROUP BY 1,2 HAVING sum((x.skill->>'calls')::bigint)=0 ORDER BY 1,2`
	if s.dialect == mysqlDialect {
		neverUsedSQL = `SELECT x.skill_name,x.skill_source,count(*) FROM jobs j JOIN JSON_TABLE(COALESCE(j.skills,JSON_ARRAY()),'$[*]' COLUMNS(skill_name VARCHAR(255) PATH '$.name' NULL ON EMPTY,skill_source VARCHAR(255) PATH '$.source' NULL ON EMPTY,skill_calls BIGINT PATH '$.calls' DEFAULT 0 ON EMPTY)) x WHERE j.created_at>=? GROUP BY 1,2 HAVING sum(x.skill_calls)=0 ORDER BY 1,2`
	}
	rows, err = tx.Query(ctx, neverUsedSQL, since)
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
	noSkill := `created_at >= $1 AND skills_supported IS TRUE AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(skills,'[]'::jsonb)) AS x(skill) WHERE (x.skill->>'calls')::bigint > 0)`
	noSkillLimit := `$2`
	if s.dialect == mysqlDialect {
		noSkill = `created_at>=? AND skills_supported IS TRUE AND NOT EXISTS (SELECT 1 FROM JSON_TABLE(COALESCE(skills,JSON_ARRAY()),'$[*]' COLUMNS(skill_calls BIGINT PATH '$.calls' DEFAULT 0 ON EMPTY)) x WHERE x.skill_calls>0)`
		noSkillLimit = `?`
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE `+noSkill, since).Scan(&out.JobsNoSkill.Count); err != nil {
		return out, err
	}
	rows, err = tx.Query(ctx, `SELECT job_id,task_id,COALESCE(harness,''),COALESCE(model,''),created_at FROM jobs WHERE `+noSkill+` ORDER BY created_at DESC,job_id DESC LIMIT `+noSkillLimit, since, limit)
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

package store

import (
	"context"
	"sort"
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
	rows, err := s.pool.Query(ctx, `SELECT job_id,task_id,COALESCE(harness,''),COALESCE(model,''),created_at,skills_supported,skills FROM jobs WHERE created_at >= $1 ORDER BY created_at DESC,job_id DESC`, since)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	type key = [2]string
	groups := map[key]*JobSkillsGroup{}
	totals := map[key]*JobSkillCount{}
	for rows.Next() {
		var job JobNoSkill
		var supported *bool
		var raw []byte
		if err = rows.Scan(&job.JobID, &job.TaskID, &job.Harness, &job.Model, &job.CreatedAt, &supported, &raw); err != nil {
			return out, err
		}
		out.JobsTotal++
		if supported == nil {
			out.JobsSkillsUnknown++
		} else if *supported {
			out.JobsSkillsSupported++
		} else {
			out.JobsSkillsUnsupported++
		}
		gkey := key{job.Harness, job.Model}
		g := groups[gkey]
		if g == nil {
			g = &JobSkillsGroup{Harness: job.Harness, Model: job.Model, Skills: []JobSkillAggregate{}}
			groups[gkey] = g
		}
		g.Jobs++
		var skills []JobSkill
		if _, err = scanJSONB(raw, &skills); err != nil {
			return out, err
		}
		var calls int64
		for _, skill := range skills {
			calls += skill.Calls
			k := key{skill.Name, skill.Source}
			t := totals[k]
			if t == nil {
				t = &JobSkillCount{Name: skill.Name, Source: skill.Source}
				totals[k] = t
			}
			t.Jobs++
			t.Calls += skill.Calls
			found := false
			for i := range g.Skills {
				v := &g.Skills[i]
				if v.Name == skill.Name && v.Source == skill.Source {
					v.Jobs++
					v.Calls += skill.Calls
					found = true
					break
				}
			}
			if !found {
				g.Skills = append(g.Skills, JobSkillAggregate{skill.Name, skill.Source, 1, skill.Calls})
			}
		}
		if supported != nil && *supported && calls == 0 {
			out.JobsNoSkill.Count++
			if len(out.JobsNoSkill.Jobs) < limit {
				out.JobsNoSkill.Jobs = append(out.JobsNoSkill.Jobs, job)
			}
		}
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	out.JobsNoSkill.Truncated = out.JobsNoSkill.Count > len(out.JobsNoSkill.Jobs)
	for _, g := range groups {
		sort.Slice(g.Skills, func(i, j int) bool {
			a, b := g.Skills[i], g.Skills[j]
			if a.Calls != b.Calls {
				return a.Calls > b.Calls
			}
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.Source < b.Source
		})
		out.ByHarnessModel = append(out.ByHarnessModel, *g)
	}
	sort.Slice(out.ByHarnessModel, func(i, j int) bool {
		a, b := out.ByHarnessModel[i], out.ByHarnessModel[j]
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		return a.Model < b.Model
	})
	for _, t := range totals {
		if t.Calls == 0 {
			out.NeverUsed = append(out.NeverUsed, *t)
		}
	}
	sort.Slice(out.NeverUsed, func(i, j int) bool {
		a, b := out.NeverUsed[i], out.NeverUsed[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Source < b.Source
	})
	return out, nil
}

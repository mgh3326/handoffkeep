package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

func jobContextClaim(t *testing.T, s *Store, id string, at *time.Time) Job {
	t.Helper()
	j, _, err := s.ClaimJob(t.Context(), Job{JobID: id, Host: "test-host", CreatedBy: "tester", ClaimedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func jobContextUpdate(t *testing.T, s *Store, id string, u JobUpdate) Job {
	t.Helper()
	j, err := s.UpdateJob(t.Context(), id, u)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func strptr(s string) *string { return &s }
func TestJobContextMigration(t *testing.T) {
	s := schemaStore(t, "job_context_migration")
	ctx := t.Context()
	var version int
	if err := s.pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version %d: %v", version, err)
	}
	var nullable string
	if err := s.pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='jobs' AND column_name='task_id'`).Scan(&nullable); err != nil || nullable != "YES" {
		t.Fatalf("nullable %q: %v", nullable, err)
	}
	jobContextClaim(t, s, "invalid-hash", nil)
	_, err := s.pool.Exec(ctx, `INSERT INTO jobs(job_id,task_id,host,state,role,label,family,profile,model,pool,owner_lane,parent_lane,parent_job,pane_id,worktree,report_doc,result,created_by,created_at,updated_at,report_sha256) SELECT 'invalid-hash-copy',task_id,host,state,role,label,family,profile,model,pool,owner_lane,parent_lane,parent_job,pane_id,worktree,report_doc,result,created_by,created_at,updated_at,'xyz' FROM jobs WHERE job_id='invalid-hash'`)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
		t.Fatalf("constraint: %v", err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='jobs' AND column_name=ANY($1) AND is_nullable='YES' AND column_default IS NULL`, []string{"input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens", "cache_write_tokens", "usage_source", "usage_recorded_at"}).Scan(&count); err != nil || count != 7 {
		t.Fatalf("usage columns %d: %v", count, err)
	}
}
func TestJobHarnessEffort(t *testing.T) {
	s := schemaStore(t, "job_context_harness")
	jobContextClaim(t, s, "job-harness", nil)
	j := jobContextUpdate(t, s, "job-harness", JobUpdate{Harness: strptr("claude-code"), Effort: strptr("high")})
	if j.Harness == nil || *j.Harness != "claude-code" || j.Effort == nil || *j.Effort != "high" {
		t.Fatalf("%+v", j)
	}
	if _, err := s.UpdateJob(t.Context(), j.JobID, JobUpdate{Harness: strptr("AKIAIOSFODNN7EXAMPLE")}); err == nil {
		t.Fatal("secret harness accepted")
	} else if _, ok := guard.SecretPattern(err); !ok {
		t.Fatalf("secret refusal: %v", err)
	}
	j, _, err := s.GetJob(t.Context(), j.JobID)
	if err != nil || j.Harness == nil || *j.Harness != "claude-code" {
		t.Fatalf("%+v %v", j, err)
	}
}
func TestJobStateTimes(t *testing.T) {
	s := schemaStore(t, "job_context_times")
	now := time.Now().UTC().Truncate(time.Microsecond)
	j := jobContextClaim(t, s, "job-times", &now)
	if j.ClaimedAt == nil || !j.ClaimedAt.Equal(now) {
		t.Fatal(j)
	}
	j = jobContextUpdate(t, s, j.JobID, JobUpdate{State: strptr("spawned"), At: &now})
	if j.SpawnedAt == nil || j.SpawnedReceivedAt == nil {
		t.Fatal(j)
	}
	receipt := *j.SpawnedReceivedAt
	j = jobContextUpdate(t, s, j.JobID, JobUpdate{State: strptr("spawned")})
	if !j.SpawnedReceivedAt.Equal(receipt) || !j.SpawnedAt.Equal(now) {
		t.Fatal(j)
	}
	j = jobContextUpdate(t, s, j.JobID, JobUpdate{ReadyAt: &now})
	if j.ReadyAt == nil || j.ReadyReceivedAt == nil {
		t.Fatal(j)
	}
	ready := *j.ReadyReceivedAt
	j = jobContextUpdate(t, s, j.JobID, JobUpdate{ReadyAt: &now})
	if !j.ReadyReceivedAt.Equal(ready) {
		t.Fatal(j)
	}
	j = jobContextUpdate(t, s, j.JobID, JobUpdate{State: strptr("completed"), At: &now})
	if j.EndedAt == nil || j.EndedReceivedAt == nil {
		t.Fatal(j)
	}
	if checkJobUpdate(JobUpdate{At: &now}) == nil {
		t.Fatal("at without state")
	}
	future := now.Add(2 * time.Hour)
	if checkJobUpdate(JobUpdate{At: &future, State: strptr("spawned")}) == nil {
		t.Fatal("future")
	}
}
func TestJobReportPinAndSummary(t *testing.T) {
	s := schemaStore(t, "job_context_report")
	jobContextClaim(t, s, "job-report", nil)
	hash := strings.Repeat("a", 64)
	if _, err := s.UpdateJob(t.Context(), "job-report", JobUpdate{ReportSHA256: &hash}); err == nil {
		t.Fatal("pin without report")
	}
	j := jobContextUpdate(t, s, "job-report", JobUpdate{ReportDoc: strptr("doc"), ReportSHA256: &hash})
	if j.ReportSHA256 == nil || *j.ReportSHA256 != hash {
		t.Fatal(j)
	}
	for _, value := range []string{`3`, `"` + strings.Repeat("a", 501) + `"`} {
		if checkJobUpdate(JobUpdate{State: strptr("completed"), Result: json.RawMessage(`{"summary":` + value + `}`)}) == nil {
			t.Fatal(value)
		}
	}
	if err := checkJobUpdate(JobUpdate{State: strptr("completed"), Result: json.RawMessage(`{"summary":"` + strings.Repeat("a", 500) + `"}`)}); err != nil {
		t.Fatal(err)
	}
	jobContextUpdate(t, s, "job-report", JobUpdate{State: strptr("spawned")})
	j = jobContextUpdate(t, s, "job-report", JobUpdate{State: strptr("completed"), Result: json.RawMessage(`{"summary":"` + strings.Repeat("a", 500) + `"}`)})
	if j.Summary == nil || len([]rune(j.Summary.Text)) != 500 {
		t.Fatal(j.Summary)
	}
}
func TestJobSkills(t *testing.T) {
	s := schemaStore(t, "job_context_skills")
	jobContextClaim(t, s, "job-skills", nil)
	jobContextUpdate(t, s, "job-skills", JobUpdate{State: strptr("spawned")})
	yes := true
	skills := []JobSkill{{Name: "one", Source: "test", Calls: 1}, {Name: "two", Source: "test"}}
	j := jobContextUpdate(t, s, "job-skills", JobUpdate{State: strptr("completed"), Skills: &skills, SkillsSupported: &yes})
	if len(j.Skills) != 2 {
		t.Fatal(j)
	}
	skills = skills[:1]
	j = jobContextUpdate(t, s, j.JobID, JobUpdate{Skills: &skills})
	if len(j.Skills) != 1 {
		t.Fatal(j)
	}
	if _, err := s.UpdateJob(t.Context(), j.JobID, JobUpdate{Role: strptr("x")}); !errors.Is(err, ErrJobStateConflict) {
		t.Fatal(err)
	}
	dup := []JobSkill{skills[0], skills[0]}
	if checkJobUpdate(JobUpdate{Skills: &dup}) == nil {
		t.Fatal("duplicate")
	}
	tooMany := make([]JobSkill, 101)
	if checkJobUpdate(JobUpdate{Skills: &tooMany}) == nil {
		t.Fatal("too many")
	}
	no := false
	if _, err := s.UpdateJob(t.Context(), j.JobID, JobUpdate{SkillsSupported: &no}); err == nil {
		t.Fatal("false with skills")
	}
}
func TestJobSummaryFallback(t *testing.T) {
	s := schemaStore(t, "job_context_summary")
	jobContextClaim(t, s, "job-lost", nil)
	j := jobContextUpdate(t, s, "job-lost", JobUpdate{State: strptr("lost"), EndReason: strptr("pane exited"), DiffStat: &JobDiffStat{2, 10, 3}})
	if j.Summary == nil || j.Summary.Text != "lost: pane exited; diff: 2 files, +10 -3" || j.Summary.Source != "server_fallback" {
		t.Fatal(j.Summary)
	}
	jobContextClaim(t, s, "job-done", nil)
	if _, err := s.UpdateJob(t.Context(), "job-done", JobUpdate{EndReason: strptr("invalid")}); err == nil {
		t.Fatal("nonterminal outcome")
	}
	j = jobContextUpdate(t, s, "job-done", JobUpdate{State: strptr("spawned")})
	if j.Summary != nil {
		t.Fatal(j.Summary)
	}
	j = jobContextUpdate(t, s, "job-done", JobUpdate{State: strptr("completed"), Result: json.RawMessage(`{"summary":"done"}`)})
	if j.Summary == nil || j.Summary.Source != "worker" {
		t.Fatal(j.Summary)
	}
}

func TestTasklessJob(t *testing.T) {
	s := schemaStore(t, "job_context_taskless")
	j := jobContextClaim(t, s, "no-task", nil)
	if j.TaskID != nil {
		t.Fatal(j)
	}
	j, created, err := s.ClaimJob(t.Context(), Job{JobID: j.JobID, Host: j.Host, CreatedBy: "tester"})
	if err != nil || created || j.TaskID != nil {
		t.Fatalf("%+v %v %v", j, created, err)
	}
	jobContextUpdate(t, s, j.JobID, JobUpdate{State: strptr("spawned"), Role: strptr("builder")})
	jobContextUpdate(t, s, j.JobID, JobUpdate{State: strptr("completed")})
	bad := int64(0)
	if _, _, err = s.ClaimJob(t.Context(), Job{JobID: "bad-task", TaskID: &bad, Host: "test-host", CreatedBy: "tester"}); err == nil {
		t.Fatal("zero task accepted")
	}
}
func TestJobSkillsReport(t *testing.T) {
	s := schemaStore(t, "job_context_report_skills")
	since := time.Now().UTC().Add(-time.Hour)
	yes, no := true, false
	zero := []JobSkill{{Name: "available", Source: "repo"}}
	used := []JobSkill{{Name: "available", Source: "repo"}, {Name: "called", Source: "repo", Calls: 3}}
	for _, x := range []struct {
		id, harness, model string
		supported          *bool
		skills             *[]JobSkill
	}{{"first", "h1", "m1", &yes, &zero}, {"second", "h2", "m2", &yes, &used}, {"third", "h2", "m2", &no, nil}, {"fourth", "h2", "m2", nil, nil}} {
		jobContextClaim(t, s, x.id, nil)
		jobContextUpdate(t, s, x.id, JobUpdate{Harness: &x.harness, Model: &x.model, SkillsSupported: x.supported, Skills: x.skills})
	}
	jobContextClaim(t, s, "outside", nil)
	if _, err := s.pool.Exec(t.Context(), `UPDATE jobs SET created_at=$1 WHERE job_id='outside'`, since.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	report, err := s.JobSkillsReport(t.Context(), since, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.JobsTotal != 4 || report.JobsSkillsSupported != 2 || report.JobsSkillsUnsupported != 1 || report.JobsSkillsUnknown != 1 || report.JobsNoSkill.Count != 1 || len(report.NeverUsed) != 1 || report.NeverUsed[0].Name != "available" || len(report.ByHarnessModel) != 2 {
		t.Fatalf("%+v", report)
	}
	if report.ByHarnessModel[0].Harness != "h1" || report.ByHarnessModel[0].Model != "m1" || report.ByHarnessModel[0].Jobs != 1 || len(report.ByHarnessModel[0].Skills) != 1 || report.ByHarnessModel[0].Skills[0].Calls != 0 || report.ByHarnessModel[1].Harness != "h2" || report.ByHarnessModel[1].Jobs != 3 || len(report.ByHarnessModel[1].Skills) != 2 || report.ByHarnessModel[1].Skills[0].Name != "called" || report.ByHarnessModel[1].Skills[0].Calls != 3 {
		t.Fatalf("groups: %+v", report.ByHarnessModel)
	}
}

func TestJobSkillsReportLimit(t *testing.T) {
	s := schemaStore(t, "job_skills_limit")
	since := time.Now().UTC().Add(-time.Hour)
	yes := true
	for _, id := range []string{"limit-a", "limit-b", "limit-c"} {
		jobContextClaim(t, s, id, nil)
		jobContextUpdate(t, s, id, JobUpdate{SkillsSupported: &yes})
	}
	report, err := s.JobSkillsReport(t.Context(), since, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.JobsTotal != 3 || report.JobsNoSkill.Count != 3 || !report.JobsNoSkill.Truncated || len(report.JobsNoSkill.Jobs) != 1 || len(report.ByHarnessModel) != 1 || report.ByHarnessModel[0].Jobs != 3 {
		t.Fatalf("limit changed totals: %+v", report)
	}
}

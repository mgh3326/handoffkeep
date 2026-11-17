package store

import (
	"strings"
	"testing"
	"time"
)

func init() {
	registerTaskAreaCase("changes/batch-and-read", changesBatchAndReadCase)
	registerTaskAreaCase("changes/source-event", changesSourceEventCase)
	registerTaskAreaCase("deploy/projection-and-services", deployProjectionAndServicesCase)
}

func changesBatchAndReadCase(t *testing.T, s *Store) {
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ref := ChangeRef{Repo: "example/app", PR: 4201}
	state := validState(now)
	batch := ChangeBatch{ChangeRef: ref, State: &state,
		Checks:   &ChangeChecksWrite{HeadSHA: testSHA1, ObservedAt: now, Source: "webhook", Checks: []ChangeCheckIn{{Name: "ci", State: "completed", Conclusion: "success"}}},
		Threads:  &ChangeThreadsWrite{ObservedAt: now, Source: "review", Threads: []ChangeThreadIn{{ThreadID: "NODE-1", AuthorKind: "human", Resolved: false, Excerpt: "review note"}}},
		Findings: &ChangeFindingsWrite{ReviewedHead: testSHA1, Round: 1, Pass: 1, Lens: "correctness", ObservedAt: now, Source: "review", Findings: []ChangeFindingIn{{ID: "NODE-1", Severity: "high", Blocking: true, Summary: "missing check", ThreadID: "NODE-1"}}},
		Gate:     &ChangeGateWrite{HeadSHA: testSHA1, State: "ready", DecidedAt: now, Source: "review"}}
	if _, err := s.ApplyChangeBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	view, found, err := s.GetChangeState(ctx, ref, ChangeReadOptions{Include: AllChangeSections})
	if err != nil || !found {
		t.Fatalf("GetChangeState found=%v err=%v", found, err)
	}
	if view.Gate == nil || view.Gate.State != "ready" || view.Checks == nil || len(*view.Checks) != 1 || view.Threads == nil || len(*view.Threads) != 1 || view.Findings == nil || len(*view.Findings) != 1 || (*view.Findings)[0].Status != "open" || view.Reviews == nil || len(*view.Reviews) != 1 {
		t.Fatalf("change view = %+v", view)
	}
	changes, err := s.ListChangesByPR(ctx, "https://github.com/example/app/pull/4201", nil, 10)
	if err != nil || len(changes) != 1 || changes[0].OpenThreads != 1 || changes[0].OpenFindings != 1 {
		t.Fatalf("change list=%+v err=%v", changes, err)
	}
	if _, err = s.SetChangeMergeFlow(ctx, changes[0].ID, ChangeMergeFlow{Priority: 4, HoldReason: "", DependsOnState: ""}); err != nil {
		t.Fatal(err)
	}
	var gateEvents int
	query := s.q(`SELECT COUNT(*) FROM change_gate_events WHERE change_id=$1`, `SELECT COUNT(*) FROM change_gate_events WHERE change_id=?`)
	if err = s.db().QueryRow(ctx, query, changes[0].ID).Scan(&gateEvents); err != nil || gateEvents != 1 {
		t.Fatalf("gate events=%d err=%v", gateEvents, err)
	}
}

func changesSourceEventCase(t *testing.T, s *Store) {
	ctx := t.Context()
	task, err := s.CreateTask(ctx, Task{Lane: "change-area", Title: "source event fixture", Kind: "implement", State: StateDeveloping, CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	pr := "https://github.com/example/app/pull/4202"
	result, err := s.ApplyTaskSourceEvent(ctx, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, PR: pr, TaskID: task.ID})
	if err != nil || !result.Applied || result.State != StateReviewing {
		t.Fatalf("ApplyTaskSourceEvent result=%+v err=%v", result, err)
	}
	changes, err := s.ListChangesByTask(ctx, task.ID, nil, 10)
	if err != nil || len(changes) != 1 || changes[0].Repo != "example/app" || changes[0].PRNumber != 4202 || len(changes[0].Tasks) != 1 {
		t.Fatalf("ListChangesByTask=%+v err=%v", changes, err)
	}
}

func deployProjectionAndServicesCase(t *testing.T, s *Store) {
	ctx := t.Context()
	service, err := s.PutDeployService(ctx, DeployService{Name: "mysql-area", Repo: "example/app", RecordPrefixes: []string{"deploy/mysql-area/"}})
	if err != nil || !service.Active || len(service.RecordPrefixes) != 1 {
		t.Fatalf("PutDeployService=%+v err=%v", service, err)
	}
	if _, err = s.PutDeployService(ctx, DeployService{Name: "mysql-area-child", Repo: "example/child", RecordPrefixes: []string{"deploy/mysql-area/nested/"}}); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("overlapping prefixes err=%v", err)
	}
	service, err = s.DeactivateDeployService(ctx, service.Name)
	if err != nil || service.Active {
		t.Fatalf("DeactivateDeployService=%+v err=%v", service, err)
	}
	body := []byte(`{"schema":"deploy-record/v0","service":"mysql-area","target":"main","result":"success","included_prs":["https://github.com/example/app/pull/4203"],"source":"test"}`)
	key := "deploy/mysql-area/20261117T120000Z"
	sha := strings.Repeat("a", 64)
	query := s.q(`INSERT INTO deploy_record_projection(key,sha256,record,decoded) VALUES($1,$2,$3,true)`, `INSERT INTO deploy_record_projection(key,sha256,record,decoded) VALUES(?,?,?,TRUE)`)
	if _, err = s.db().Exec(ctx, query, key, sha, body); err != nil {
		t.Fatal(err)
	}
	pages, err := s.ListDeployRecordsByPrefix(ctx, []string{"deploy/mysql-area/"}, 5)
	if err != nil || len(pages["deploy/mysql-area/"].Records) != 1 || pages["deploy/mysql-area/"].Records[0].Key != key || pages["deploy/mysql-area/"].Records[0].Record.Result != "success" {
		t.Fatalf("deploy record pages=%+v err=%v", pages, err)
	}
}

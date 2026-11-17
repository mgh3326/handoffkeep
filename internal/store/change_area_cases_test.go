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

	// A revert event can update a revert PR that already has an open changes
	// row. Its current head/base must be used before the upsert changes state.
	firstHead, secondHead := strings.Repeat("1", 40), strings.Repeat("2", 40)
	applyRecordChange := func(ev TaskSourceEvent) {
		t.Helper()
		tx, release, err := s.begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		defer tx.Rollback(ctx)
		at := time.Now().UTC()
		if err = srcEvRecordChangeTx(ctx, tx, task, ev, at, at); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	applyRecordChange(TaskSourceEvent{Source: SourceMergewakePR, PR: pr, HeadSHA: firstHead, BaseSHA: strings.Repeat("b", 40)})
	applyRecordChange(TaskSourceEvent{Source: SourceMergewakeRevert, PR: "https://github.com/example/app/pull/4204", RevertPR: pr, HeadSHA: secondHead, BaseSHA: strings.Repeat("c", 40)})
	var gotHead, gotBase string
	q := s.q(`SELECT head_sha,base_sha FROM changes WHERE repo=$1 AND pr_number=$2`, `SELECT head_sha,base_sha FROM changes WHERE repo=? AND pr_number=?`)
	if err = s.db().QueryRow(ctx, q, "example/app", 4202).Scan(&gotHead, &gotBase); err != nil {
		t.Fatal(err)
	}
	if gotHead != secondHead || gotBase != strings.Repeat("c", 40) {
		t.Fatalf("revert source event head/base = %q/%q, want %q/%q", gotHead, gotBase, secondHead, strings.Repeat("c", 40))
	}
	tx, release, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer tx.Rollback(ctx)
	if _, err = taskHoldsTx(ctx, tx, task.ID, pr); err != nil {
		t.Fatalf("taskHoldsTx after revert events: %v", err)
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
	pr := "https://github.com/example/app/pull/4203"
	body := []byte(`{"schema":"deploy-record/v0","service":"mysql-area","target":"main","result":"success","included_prs":["https://github.com/example/app/pull/4203"],"source":"test"}`)
	key := "deploy/mysql-area/20261117T120000Z"
	sha := strings.Repeat("a", 64)
	query := s.q(`INSERT INTO deploy_record_projection(key,sha256,record,decoded) VALUES($1,$2,$3,true)`, `INSERT INTO deploy_record_projection(key,sha256,record,decoded) VALUES(?,?,?,TRUE)`)
	if _, err = s.db().Exec(ctx, query, key, sha, body); err != nil {
		t.Fatal(err)
	}
	newerFailed := []byte(`{"schema":"deploy-record/v0","service":"mysql-area","target":"main","result":"failed","included_prs":["https://github.com/example/app/pull/4203"],"source":"test"}`)
	if _, err = s.db().Exec(ctx, query, "deploy/mysql-area/20261117T130000Z", strings.Repeat("b", 64), newerFailed); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db().Exec(ctx, query, "deploy/mysql-area/20261117T140000Z.invalid", strings.Repeat("c", 64), body); err != nil {
		t.Fatal(err)
	}
	tx, release, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer tx.Rollback(ctx)
	record, found, err := deployRecordForPRTx(ctx, tx, pr)
	if err != nil || !found || record.Key != key {
		t.Fatalf("deploy record selected=%q found=%v err=%v, want older successful %q", record.Key, found, err, key)
	}
	pages, err := s.ListDeployRecordsByPrefix(ctx, []string{"deploy/mysql-area/"}, 5)
	if err != nil || len(pages["deploy/mysql-area/"].Records) != 1 || pages["deploy/mysql-area/"].Records[0].Key != key || pages["deploy/mysql-area/"].Records[0].Record.Result != "success" {
		t.Fatalf("deploy record pages=%+v err=%v", pages, err)
	}
}

package store

import (
	"errors"
	"testing"
	"time"
)

func TestDecisionTabInvalid(t *testing.T) {
	var s *Store
	if _, err := s.ListDecisionTab(t.Context(), "invalid", time.Now().UTC(), 200); !errors.Is(err, ErrInvalidDecisionTab) {
		t.Fatalf("invalid tab: %v", err)
	}
}

func TestDecisionTabCountsAndTruncation(t *testing.T) {
	s := schemaStore(t, "decision_tab")
	ctx := t.Context()
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `INSERT INTO decision_requests(source,source_ref,question,status,due_at,created_at,updated_at)
 SELECT 'chat',n::text,'fixture','open',CASE WHEN n <= 2 THEN $1::timestamptz ELSE NULL END,$1,$1 FROM generate_series(1,201) n`, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ListDecisionTab(ctx, "open", now, 200)
	if err != nil {
		t.Fatal(err)
	}
	if got.Open != 201 || got.Overdue != 2 || !got.Truncated || len(got.Items) != 200 {
		t.Fatalf("counts=%+v", got)
	}
	if got.Items[0].ID >= got.Items[1].ID {
		t.Fatal("equal deadlines did not order by id")
	}
	overdue, err := s.ListDecisionTab(ctx, "overdue", now, 200)
	if err != nil || len(overdue.Items) != 2 {
		t.Fatalf("overdue=%+v err=%v", overdue, err)
	}
	for _, tab := range []string{"answered", "cleanup"} {
		out, err := s.ListDecisionTab(ctx, tab, now, 200)
		if err != nil || len(out.Items) != 0 {
			t.Fatalf("%s=%+v err=%v", tab, out, err)
		}
	}
}

func TestListDecisionAttempts(t *testing.T) {
	s := schemaStore(t, "decision_attempt_list")
	task := decisionTask(t, s, "backlog")
	ctx := t.Context()
	now := time.Now().UTC()
	for _, result := range []string{"pending_confirmation", "stale_revision"} {
		_, err := s.pool.Exec(ctx, `INSERT INTO decision_answer_attempts(channel,event_key,payload_hash,task_id,revision_request_id,actor,result,received_at) VALUES('session',$1,'fixture',$2,'dr-1-1','fixture',$1,$3)`, result, task.ID, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListDecisionAttempts(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Result != "stale_revision" || got[1].Result != "pending_confirmation" || got[0].ID <= got[1].ID {
		t.Fatalf("attempts=%+v", got)
	}
}

func TestDecisionTabAnsweredRequestStates(t *testing.T) {
	s := schemaStore(t, "decision_tab_states")
	ctx := t.Context()
	for _, kind := range []string{DecisionRequestAnswered, DecisionRequestDefaultApplied, DecisionRequestWithdrawn} {
		task := decisionTask(t, s, "backlog")
		asked, err := s.RecordDecisionRequest(ctx, task.ID, "fixture", decisionInput())
		if err != nil {
			t.Fatal(err)
		}
		in := DecisionResolveInput{RequestID: asked.Request.ID, Kind: kind, Option: "A", Text: "fixture"}
		if kind == DecisionRequestDefaultApplied {
			in.Option = ""
			in.Receipt = "fixture-receipt"
		}
		if kind == DecisionRequestWithdrawn {
			in.Option = ""
		}
		if _, err = s.ResolveDecisionRequest(ctx, task.ID, "fixture", in); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	out, err := s.ListDecisionTab(ctx, "answered", time.Now().UTC(), 200)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, item := range out.Items {
		got[item.RequestState] = true
	}
	for _, state := range []string{DecisionViewAnswered, DecisionViewDefaultApplied, DecisionViewWithdrawn} {
		if !got[state] {
			t.Fatalf("missing %s: %+v", state, out.Items)
		}
	}
}

func TestDecisionTabCleanup(t *testing.T) {
	s := schemaStore(t, "decision_tab_cleanup")
	ctx := t.Context()
	task := decisionTask(t, s, "dropped")
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `INSERT INTO decision_requests(source,source_ref,task_id,status,created_at,updated_at) VALUES('task_flag','fixture',$1,'open',$2,$2)`, task.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.ListDecisionTab(ctx, "cleanup", now, 200)
	if err != nil || len(out.Items) != 1 || out.Items[0].TaskID == nil || *out.Items[0].TaskID != task.ID {
		t.Fatalf("cleanup=%+v err=%v", out, err)
	}
}

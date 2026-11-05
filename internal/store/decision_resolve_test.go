package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

type attemptRow struct {
	ID         int64
	Channel    string
	EventKey   string
	Root       string
	Revision   int
	Provenance string
	Actor      string
	Result     string
}

func attemptsOf(t *testing.T, pool *pgxpool.Pool, taskID int64) []attemptRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id,channel,event_key,COALESCE(request_id,''),COALESCE(question_revision,0),COALESCE(provenance,''),actor,result FROM decision_answer_attempts WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		if err := rows.Scan(&a.ID, &a.Channel, &a.EventKey, &a.Root, &a.Revision, &a.Provenance, &a.Actor, &a.Result); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func resolutionsOf(t *testing.T, pool *pgxpool.Pool, taskID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM decision_resolutions WHERE task_id=$1`, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func resolutionProvenance(t *testing.T, pool *pgxpool.Pool, id int64) (provenance, channel, decidedBy string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT provenance,channel,decided_by FROM decision_resolutions WHERE resolution_id=$1`, id).Scan(&provenance, &channel, &decidedBy); err != nil {
		t.Fatal(err)
	}
	return provenance, channel, decidedBy
}

func answerA(request DecisionRequest, channel, actor string) ResolveDecisionInput {
	return ResolveDecisionInput{RequestID: request.ID, Kind: DecisionRequestAnswered, Option: "A", Channel: channel, Actor: actor}
}

func humanOnlyRequest(t *testing.T, s *Store, taskID int64) DecisionRequest {
	t.Helper()
	in := decisionInput()
	in.HumanOnly = true
	return assistantRecordWith(t, s, taskID, in)
}

// TestDecisionResolutionsMigration pins version 42: both tables, the two
// unique keys, an append-only attempts table, and the version record.
func TestDecisionResolutionsMigration(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	for _, table := range []string{"decision_answer_attempts", "decision_resolutions"} {
		var found bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass(format('%I.%I', current_schema(), $1::text)) IS NOT NULL`, table).Scan(&found); err != nil || !found {
			t.Fatalf("table %s found=%v err=%v", table, found, err)
		}
	}
	var version int
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("max version=%d err=%v want %d", version, err, SchemaVersion)
	}
	task := decisionTask(t, s, "in_progress")
	insert := `INSERT INTO decision_answer_attempts(channel,event_key,payload_hash,task_id,revision_request_id,actor,result,received_at) VALUES('cli','k1','h',$1,'dr-1-1','a','answered',now())`
	if _, err := pool.Exec(ctx, insert, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, task.ID); err == nil || !strings.Contains(err.Error(), "decision_answer_attempts_channel_event_key_key") {
		t.Fatalf("duplicate (channel,event_key): %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE decision_answer_attempts SET actor='b'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("update: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM decision_answer_attempts`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("delete: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO decision_answer_attempts(channel,event_key,payload_hash,task_id,revision_request_id,actor,result,received_at) VALUES('mail','k2','h',$1,'dr-1-1','a','answered',now())`, task.ID); err == nil {
		t.Fatal("unknown channel accepted")
	}
	request := assistantRecord(t, s, task.ID)
	out, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1"))
	if err != nil || out.ResolutionID == 0 {
		t.Fatalf("resolve=%+v err=%v", out, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO decision_resolutions(request_id,question_revision,revision_request_id,task_id,kind,provenance,channel,decided_by,recorded_by,attempt_id,committed_at)
		VALUES($1,1,$2,$3,'answered','operator_relay','cli','x','x',$4,now())`, request.RootID(), request.ID, task.ID, out.AttemptID)
	if err == nil || !strings.Contains(err.Error(), "decision_resolutions_request_id_question_revision_key") {
		t.Fatalf("second resolution for one revision: %v", err)
	}
}

func TestDecisionRequestRoot(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	first, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	if first.Request.Root != first.Request.ID {
		t.Fatalf("a new request is its own root: %+v", first.Request)
	}
	second := decisionInput()
	second.Question = "다시 묻는다"
	second.Supersedes = first.Request.ID
	replaced, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", second)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Request.Root != first.Request.ID {
		t.Fatalf("a supersede inherits the root: %+v", replaced.Request)
	}
	third := decisionInput()
	third.Question = "한 번 더"
	third.Supersedes = replaced.Request.ID
	again, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", third)
	if err != nil || again.Request.Root != first.Request.ID {
		t.Fatalf("second supersede root: %+v err=%v", again.Request, err)
	}
	if _, err = s.ResolveDecision(ctx, answerA(again.Request, DecisionChannelCLI, "director-1")); err != nil {
		t.Fatal(err)
	}
	fresh := decisionInput()
	fresh.Question = "새 질문"
	next, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", fresh)
	if err != nil || next.Request.Root != next.Request.ID || next.Request.Revision != 4 {
		t.Fatalf("a request after an answer is a new logical request: %+v err=%v", next.Request, err)
	}
	// A request recorded before Root existed has none and counts as its own root.
	if _, err = pool.Exec(ctx, `UPDATE tasks SET refs = refs #- '{decision_request,root}' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	legacy, _, err := s.GetTask(ctx, task.ID)
	if err != nil || legacy.Refs.DecisionRequest.Root != "" || legacy.Refs.DecisionRequest.RootID() != next.Request.ID {
		t.Fatalf("legacy request=%+v err=%v", legacy.Refs.DecisionRequest, err)
	}
	over := decisionInput()
	over.Question = "legacy 를 대체"
	over.Supersedes = next.Request.ID
	child, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", over)
	if err != nil || child.Request.Root != next.Request.ID {
		t.Fatalf("supersede of a legacy request: %+v err=%v", child.Request, err)
	}
}

// TestResolveDecisionResults has one subtest per row of the result table
// (hk 3061): each asserts the result, the attempt row and whether a
// resolution row exists.
func TestResolveDecisionResults(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	check := func(t *testing.T, taskID int64, attempts []string, resolutions int) {
		t.Helper()
		var got []string
		for _, a := range attemptsOf(t, pool, taskID) {
			got = append(got, a.Result)
		}
		if strings.Join(got, ",") != strings.Join(attempts, ",") {
			t.Fatalf("attempts=%v want %v", got, attempts)
		}
		if n := resolutionsOf(t, pool, taskID); n != resolutions {
			t.Fatalf("resolutions=%d want %d", n, resolutions)
		}
	}

	t.Run("answered", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		out, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1"))
		if err != nil || out.Result != DecisionResultAnswered || out.Duplicate || out.ResolutionID == 0 || out.AttemptID == 0 || out.Request.Status != DecisionRequestAnswered {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		check(t, task.ID, []string{"answered"}, 1)
	})

	t.Run("identical_resend", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		first, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1"))
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1"))
		if err != nil || again.Result != DecisionResultAnswered || !again.Duplicate || again.ResolutionID != first.ResolutionID {
			t.Fatalf("again=%+v err=%v", again, err)
		}
		check(t, task.ID, []string{"answered"}, 1)
		// The same answer from another actor is a duplicate of the stored
		// resolution: a new attempt, no second resolution.
		other, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-2"))
		if err != nil || other.Result != DecisionResultAnswered || !other.Duplicate || other.ResolutionID != first.ResolutionID {
			t.Fatalf("other=%+v err=%v", other, err)
		}
		check(t, task.ID, []string{"answered", "answered"}, 1)
	})

	t.Run("older_revision", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		first := assistantRecord(t, s, task.ID)
		second := decisionInput()
		second.Question = "다시"
		second.Supersedes = first.ID
		assistantRecordWith(t, s, task.ID, second)
		out, err := s.ResolveDecision(ctx, answerA(first, DecisionChannelCLI, "director-1"))
		if err != nil || out.Result != DecisionResultStaleRevision || !errors.Is(out.Refusal, ErrDecisionRequestStale) {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		check(t, task.ID, []string{"stale_revision"}, 0)
	})

	t.Run("not_in_chain", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		for _, id := range []string{decisionRequestID(task.ID, 9), decisionRequestID(task.ID+1000000, 1)} {
			in := answerA(request, DecisionChannelCLI, "director-1")
			in.TaskID, in.RequestID = task.ID, id
			out, err := s.ResolveDecision(ctx, in)
			if err != nil || out.Result != DecisionResultSuperseded || !errors.Is(out.Refusal, ErrDecisionRequestStale) {
				t.Fatalf("id %s out=%+v err=%v", id, out, err)
			}
		}
		check(t, task.ID, []string{"superseded", "superseded"}, 0)
	})

	t.Run("older_logical_request", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		first := assistantRecord(t, s, task.ID)
		if _, err := s.ResolveDecision(ctx, answerA(first, DecisionChannelCLI, "director-1")); err != nil {
			t.Fatal(err)
		}
		next := decisionInput()
		next.Question = "후속"
		assistantRecordWith(t, s, task.ID, next)
		other := answerA(first, DecisionChannelCLI, "director-3")
		out, err := s.ResolveDecision(ctx, other)
		if err != nil || out.Result != DecisionResultSuperseded || !errors.Is(out.Refusal, ErrDecisionRequestStale) {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		check(t, task.ID, []string{"answered", "superseded"}, 1)
	})

	t.Run("conflict", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		if _, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1")); err != nil {
			t.Fatal(err)
		}
		different := answerA(request, DecisionChannelCLI, "director-1")
		different.Option = "B"
		out, err := s.ResolveDecision(ctx, different)
		if err != nil || out.Result != DecisionResultConflict || !errors.Is(out.Refusal, ErrDecisionRequestResolved) {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		check(t, task.ID, []string{"answered", "conflict"}, 1)
	})

	t.Run("default_applied_is_expired", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		applied := ResolveDecisionInput{RequestID: request.ID, Kind: DecisionRequestDefaultApplied, Receipt: "hk:doc receipt/x", Channel: DecisionChannelCLI, Actor: "director-1"}
		if _, err := s.ResolveDecision(ctx, applied); err != nil {
			t.Fatal(err)
		}
		out, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1"))
		if err != nil || out.Result != DecisionResultExpired || !errors.Is(out.Refusal, ErrDecisionRequestResolved) {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		check(t, task.ID, []string{"answered", "expired"}, 1)
	})

	t.Run("withdrawn_is_superseded", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		withdrawn := ResolveDecisionInput{RequestID: request.ID, Kind: DecisionRequestWithdrawn, Text: "no longer needed", Channel: DecisionChannelCLI, Actor: "director-1"}
		if _, err := s.ResolveDecision(ctx, withdrawn); err != nil {
			t.Fatal(err)
		}
		out, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1"))
		if err != nil || out.Result != DecisionResultSuperseded || !errors.Is(out.Refusal, ErrDecisionRequestResolved) {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		check(t, task.ID, []string{"answered", "superseded"}, 1)
	})

	t.Run("human_only_judgement_refused", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := humanOnlyRequest(t, s, task.ID)
		out, err := s.ResolveDecision(ctx, ResolveDecisionInput{RequestID: request.ID, Option: "A", Channel: DecisionChannelAssistant, Actor: "berry-mcp"})
		if !errors.Is(err, ErrDecisionRequestHumanOnly) || out.AttemptID == 0 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		check(t, task.ID, []string{"refused"}, 0)
		got, _, _ := s.GetTask(ctx, task.ID)
		if got.Refs.DecisionRequest.Status != DecisionRequestOpen {
			t.Fatalf("status=%s", got.Refs.DecisionRequest.Status)
		}
	})

	t.Run("human_only_assistant_quote_pending", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := humanOnlyRequest(t, s, task.ID)
		out, err := s.ResolveDecision(ctx, ResolveDecisionInput{
			RequestID: request.ID, Option: "A", Channel: DecisionChannelAssistant, Actor: "berry-mcp",
			Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: "A로 진행", DecisionRef: request.ID,
		})
		if err != nil || out.Result != DecisionResultPendingConfirmation || out.Refusal != nil || out.ResolutionID != 0 || out.Request.Status != DecisionRequestOpen {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		check(t, task.ID, []string{"pending_confirmation"}, 0)
		if n := len(outboxRows(t, s)); n != 0 {
			t.Fatalf("a pending answer left %d outbox rows", n)
		}
	})

	t.Run("human_only_cli_other_client_pending", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := humanOnlyRequest(t, s, task.ID)
		out, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1"))
		if err != nil || out.Result != DecisionResultPendingConfirmation || out.Request.Status != DecisionRequestOpen {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		check(t, task.ID, []string{"pending_confirmation"}, 0)
		// The adapter maps the same attempt to its sentinel.
		if _, err = s.ResolveDecisionRequest(ctx, task.ID, "director-1", DecisionResolveInput{RequestID: request.ID, Kind: DecisionRequestAnswered, Option: "A", Responder: "x"}); !errors.Is(err, ErrDecisionPendingConfirmation) {
			t.Fatalf("adapter err=%v", err)
		}
	})

	t.Run("validation_writes_nothing", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		notAnOption := answerA(request, DecisionChannelCLI, "director-1")
		notAnOption.Option = "F"
		if _, err := s.ResolveDecision(ctx, notAnOption); !errors.Is(err, ErrInvalidDecisionRequest) {
			t.Fatalf("option not in request: %v", err)
		}
		mismatch := ResolveDecisionInput{RequestID: request.ID, Option: "A", Channel: DecisionChannelAssistant, Actor: "berry-mcp", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: "A", DecisionRef: "dr-1-1"}
		if _, err := s.ResolveDecision(ctx, mismatch); !errors.Is(err, ErrProvenanceRefMismatch) {
			t.Fatalf("ref mismatch: %v", err)
		}
		badClaim := ResolveDecisionInput{RequestID: request.ID, Option: "A", Channel: DecisionChannelAssistant, Actor: "berry-mcp", Provenance: "operator_direct"}
		if _, err := s.ResolveDecision(ctx, badClaim); !errors.Is(err, ErrInvalidProvenance) {
			t.Fatalf("bad claim: %v", err)
		}
		check(t, task.ID, nil, 0)
	})
}

func TestResolveDecisionEventKey(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()

	t.Run("same_key_same_payload_replays", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		in := answerA(request, DecisionChannelCLI, "director-1")
		in.EventKey = "click-1"
		first, err := s.ResolveDecision(ctx, in)
		if err != nil || first.Duplicate {
			t.Fatalf("first=%+v err=%v", first, err)
		}
		again, err := s.ResolveDecision(ctx, in)
		if err != nil || !again.Duplicate || again.Result != DecisionResultAnswered || again.ResolutionID != first.ResolutionID || again.AttemptID != first.AttemptID {
			t.Fatalf("again=%+v err=%v", again, err)
		}
		if rows := attemptsOf(t, pool, task.ID); len(rows) != 1 || rows[0].EventKey != "click-1" {
			t.Fatalf("attempts=%+v", rows)
		}
	})

	t.Run("absent_key_is_the_payload_hash", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		if _, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1")); err != nil {
			t.Fatal(err)
		}
		if rows := attemptsOf(t, pool, task.ID); len(rows) != 1 || !strings.HasPrefix(rows[0].EventKey, "auto:") {
			t.Fatalf("attempts=%+v", rows)
		}
	})

	t.Run("same_key_other_payload_conflicts", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		in := answerA(request, DecisionChannelCLI, "director-1")
		in.EventKey = "click-2"
		if _, err := s.ResolveDecision(ctx, in); err != nil {
			t.Fatal(err)
		}
		in.Option = "B"
		if _, err := s.ResolveDecision(ctx, in); !errors.Is(err, ErrDecisionEventKeyConflict) {
			t.Fatalf("err=%v", err)
		}
		if rows := attemptsOf(t, pool, task.ID); len(rows) != 1 {
			t.Fatalf("attempts=%+v", rows)
		}
		if n := resolutionsOf(t, pool, task.ID); n != 1 {
			t.Fatalf("resolutions=%d", n)
		}
	})

	t.Run("replay_after_newer_revision", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		in := answerA(request, DecisionChannelCLI, "director-1")
		in.EventKey = "click-3"
		first, err := s.ResolveDecision(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		next := decisionInput()
		next.Question = "후속 질문"
		assistantRecordWith(t, s, task.ID, next)
		again, err := s.ResolveDecision(ctx, in)
		if err != nil || again.Result != DecisionResultAnswered || !again.Duplicate || again.ResolutionID != first.ResolutionID || again.Request.ID != request.ID {
			t.Fatalf("again=%+v err=%v", again, err)
		}
		if rows := attemptsOf(t, pool, task.ID); len(rows) != 1 {
			t.Fatalf("attempts=%+v", rows)
		}
	})

	// R2: the same key and payload answers what the first delivery got. A
	// later confirmation of a pending_confirmation attempt is a new attempt
	// under its own key, and does not rewrite the pending one.
	t.Run("replay_pending_after_confirmation", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := humanOnlyRequest(t, s, task.ID)
		relay := answerA(request, DecisionChannelCLI, "director-1")
		relay.EventKey = "relay-1"
		first, err := s.ResolveDecision(ctx, relay)
		if err != nil || first.Result != DecisionResultPendingConfirmation || first.Duplicate || first.Request.Status != DecisionRequestOpen {
			t.Fatalf("first=%+v err=%v", first, err)
		}
		before, err := s.ResolveDecision(ctx, relay)
		if err != nil || before.Result != DecisionResultPendingConfirmation || !before.Duplicate || before.AttemptID != first.AttemptID || before.Request.Status != DecisionRequestOpen {
			t.Fatalf("replay before confirmation=%+v err=%v", before, err)
		}
		confirm := ResolveDecisionInput{RequestID: request.ID, Kind: DecisionRequestAnswered, Option: "A", Channel: DecisionChannelConsole, Actor: "operator@example.com", Verified: true, EventKey: "confirm-1"}
		confirmed, err := s.ResolveDecision(ctx, confirm)
		if err != nil || confirmed.Result != DecisionResultAnswered || confirmed.Duplicate || confirmed.AttemptID == first.AttemptID || confirmed.ResolutionID == 0 {
			t.Fatalf("confirmed=%+v err=%v", confirmed, err)
		}
		after, err := s.ResolveDecision(ctx, relay)
		if err != nil {
			t.Fatal(err)
		}
		if after.Result != DecisionResultPendingConfirmation || after.Refusal != nil || !after.Duplicate || after.AttemptID != first.AttemptID || after.ResolutionID != 0 {
			t.Fatalf("replay after confirmation=%+v", after)
		}
		if after.Request.ID != request.ID || after.Request.Status != DecisionRequestOpen || after.Request.Resolution != nil {
			t.Fatalf("replay after confirmation reports the request as %+v, want the open request the first delivery saw", after.Request)
		}
		again, err := s.ResolveDecision(ctx, confirm)
		if err != nil || again.Result != DecisionResultAnswered || !again.Duplicate || again.AttemptID != confirmed.AttemptID || again.ResolutionID != confirmed.ResolutionID {
			t.Fatalf("confirmation replay=%+v err=%v", again, err)
		}
		rows := attemptsOf(t, pool, task.ID)
		if len(rows) != 2 || rows[0].Result != DecisionResultPendingConfirmation || rows[1].Result != DecisionResultAnswered {
			t.Fatalf("attempts=%+v", rows)
		}
		if n := resolutionsOf(t, pool, task.ID); n != 1 {
			t.Fatalf("resolutions=%d", n)
		}
	})

	t.Run("replay_of_a_duplicate_answer_keeps_the_resolution", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		first, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1"))
		if err != nil || first.ResolutionID == 0 {
			t.Fatalf("first=%+v err=%v", first, err)
		}
		resend := answerA(request, DecisionChannelCLI, "director-2")
		resend.EventKey = "resend-1"
		dup, err := s.ResolveDecision(ctx, resend)
		if err != nil || !dup.Duplicate || dup.ResolutionID != first.ResolutionID || dup.AttemptID == first.AttemptID {
			t.Fatalf("dup=%+v err=%v", dup, err)
		}
		again, err := s.ResolveDecision(ctx, resend)
		if err != nil || again.Result != DecisionResultAnswered || !again.Duplicate || again.AttemptID != dup.AttemptID || again.ResolutionID != first.ResolutionID {
			t.Fatalf("again=%+v err=%v", again, err)
		}
		if rows := attemptsOf(t, pool, task.ID); len(rows) != 2 {
			t.Fatalf("attempts=%+v", rows)
		}
	})

	// Every refused result replays with the same result, sentinel and
	// attempt, and writes no second attempt.
	t.Run("replay_of_each_result", func(t *testing.T) {
		cases := []struct {
			name   string
			result string
			refuse error
			setup  func(t *testing.T, taskID int64) ResolveDecisionInput
		}{
			{"stale_revision", DecisionResultStaleRevision, ErrDecisionRequestStale, func(t *testing.T, taskID int64) ResolveDecisionInput {
				first := assistantRecord(t, s, taskID)
				second := decisionInput()
				second.Question = "다시"
				second.Supersedes = first.ID
				assistantRecordWith(t, s, taskID, second)
				return answerA(first, DecisionChannelCLI, "director-1")
			}},
			{"superseded", DecisionResultSuperseded, ErrDecisionRequestStale, func(t *testing.T, taskID int64) ResolveDecisionInput {
				request := assistantRecord(t, s, taskID)
				in := answerA(request, DecisionChannelCLI, "director-1")
				in.TaskID, in.RequestID = taskID, decisionRequestID(taskID, 9)
				return in
			}},
			{"withdrawn_superseded", DecisionResultSuperseded, ErrDecisionRequestResolved, func(t *testing.T, taskID int64) ResolveDecisionInput {
				request := assistantRecord(t, s, taskID)
				withdrawn := ResolveDecisionInput{RequestID: request.ID, Kind: DecisionRequestWithdrawn, Text: "no longer needed", Channel: DecisionChannelCLI, Actor: "director-1"}
				if _, err := s.ResolveDecision(ctx, withdrawn); err != nil {
					t.Fatal(err)
				}
				return answerA(request, DecisionChannelCLI, "director-1")
			}},
			{"conflict", DecisionResultConflict, ErrDecisionRequestResolved, func(t *testing.T, taskID int64) ResolveDecisionInput {
				request := assistantRecord(t, s, taskID)
				if _, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1")); err != nil {
					t.Fatal(err)
				}
				different := answerA(request, DecisionChannelCLI, "director-1")
				different.Option = "B"
				return different
			}},
			{"expired", DecisionResultExpired, ErrDecisionRequestResolved, func(t *testing.T, taskID int64) ResolveDecisionInput {
				request := assistantRecord(t, s, taskID)
				applied := ResolveDecisionInput{RequestID: request.ID, Kind: DecisionRequestDefaultApplied, Receipt: "hk:doc receipt/x", Channel: DecisionChannelCLI, Actor: "director-1"}
				if _, err := s.ResolveDecision(ctx, applied); err != nil {
					t.Fatal(err)
				}
				return answerA(request, DecisionChannelCLI, "director-1")
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				task := decisionTask(t, s, "in_progress")
				in := tc.setup(t, task.ID)
				in.EventKey = "each-" + tc.name
				before := len(attemptsOf(t, pool, task.ID))
				first, err := s.ResolveDecision(ctx, in)
				if err != nil || first.Result != tc.result || !errors.Is(first.Refusal, tc.refuse) {
					t.Fatalf("first=%+v err=%v", first, err)
				}
				again, err := s.ResolveDecision(ctx, in)
				if err != nil || again.Result != tc.result || !errors.Is(again.Refusal, tc.refuse) || !again.Duplicate || again.AttemptID != first.AttemptID || again.ResolutionID != first.ResolutionID {
					t.Fatalf("again=%+v err=%v", again, err)
				}
				if rows := attemptsOf(t, pool, task.ID); len(rows) != before+1 {
					t.Fatalf("attempts=%+v", rows)
				}
			})
		}
	})

	t.Run("invalid_key", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		for _, key := range []string{"has space", strings.Repeat("k", DecisionEventKeyMaxBytes+1), "한글"} {
			in := answerA(request, DecisionChannelCLI, "director-1")
			in.EventKey = key
			if _, err := s.ResolveDecision(ctx, in); !errors.Is(err, ErrInvalidDecisionRequest) {
				t.Fatalf("key %q: %v", key, err)
			}
		}
	})

	t.Run("concurrent_answers_leave_one_resolution", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		options := []string{"A", "B", "C", "A"}
		results := make([]ResolveDecisionOutcome, len(options))
		errs := make([]error, len(options))
		var wg sync.WaitGroup
		for i, option := range options {
			wg.Add(1)
			go func() {
				defer wg.Done()
				in := answerA(request, DecisionChannelCLI, "director-1")
				in.Option = option
				results[i], errs[i] = s.ResolveDecision(ctx, in)
			}()
		}
		wg.Wait()
		answered := 0
		for i, out := range results {
			if errs[i] != nil {
				t.Fatalf("call %d: %v", i, errs[i])
			}
			switch {
			case out.Result == DecisionResultAnswered && !out.Duplicate:
				answered++
			case out.Result == DecisionResultConflict, out.Result == DecisionResultAnswered && out.Duplicate:
			default:
				t.Fatalf("call %d: %+v", i, out)
			}
		}
		if answered != 1 {
			t.Fatalf("answered=%d results=%+v", answered, results)
		}
		if n := resolutionsOf(t, pool, task.ID); n != 1 {
			t.Fatalf("resolutions=%d", n)
		}
	})
}

func TestResolveDecisionProvenance(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	resolved := func(t *testing.T, in ResolveDecisionInput) (provenance, channel, decidedBy string) {
		t.Helper()
		out, err := s.ResolveDecision(ctx, in)
		if err != nil || out.Result != DecisionResultAnswered || out.ResolutionID == 0 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		return resolutionProvenance(t, pool, out.ResolutionID)
	}
	newRequest := func(t *testing.T) (Task, DecisionRequest) {
		task := decisionTask(t, s, "in_progress")
		return task, assistantRecord(t, s, task.ID)
	}

	t.Run("console_verified", func(t *testing.T) {
		_, request := newRequest(t)
		in := answerA(request, DecisionChannelConsole, "operator:robin@example.com")
		in.Verified = true
		if p, c, d := resolved(t, in); p != DecisionProvenanceOperatorDirect || c != "console" || d != "operator" {
			t.Fatalf("provenance=%s channel=%s decided_by=%s", p, c, d)
		}
	})
	t.Run("cli_operator_client", func(t *testing.T) {
		_, request := newRequest(t)
		in := answerA(request, DecisionChannelCLI, "operator")
		in.Verified = true
		if p, _, _ := resolved(t, in); p != DecisionProvenanceOperatorDirect {
			t.Fatalf("provenance=%s", p)
		}
	})
	t.Run("cli_other_client_is_relay", func(t *testing.T) {
		_, request := newRequest(t)
		if p, _, _ := resolved(t, answerA(request, DecisionChannelCLI, "operator-desk")); p != DecisionProvenanceOperatorRelay {
			t.Fatalf("provenance=%s", p)
		}
	})
	t.Run("assistant_quote_is_relay", func(t *testing.T) {
		_, request := newRequest(t)
		in := ResolveDecisionInput{RequestID: request.ID, Option: "A", Channel: DecisionChannelAssistant, Actor: "berry-mcp", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: "A로 해", DecisionRef: request.ID}
		if p, _, d := resolved(t, in); p != DecisionProvenanceOperatorRelay || d != DecisionDecidedByOperator {
			t.Fatalf("provenance=%s decided_by=%s", p, d)
		}
	})
	t.Run("assistant_without_quote_is_judgement", func(t *testing.T) {
		_, request := newRequest(t)
		in := ResolveDecisionInput{RequestID: request.ID, Option: "A", Channel: DecisionChannelAssistant, Actor: "berry-mcp"}
		if p, _, d := resolved(t, in); p != DecisionProvenanceAssistantJudgement || d != DecisionDecidedByAssistant {
			t.Fatalf("provenance=%s decided_by=%s", p, d)
		}
	})
	t.Run("body_claiming_operator_direct", func(t *testing.T) {
		task, request := newRequest(t)
		for _, channel := range []string{DecisionChannelAssistant, DecisionChannelCLI, DecisionChannelConsole} {
			in := answerA(request, channel, "someone")
			in.Provenance = DecisionProvenanceOperatorDirect
			if _, err := s.ResolveDecision(ctx, in); !errors.Is(err, ErrInvalidProvenance) {
				t.Fatalf("channel %s: %v", channel, err)
			}
		}
		verified := answerA(request, DecisionChannelAssistant, "berry-mcp")
		verified.Verified = true
		if _, err := s.ResolveDecision(ctx, verified); !errors.Is(err, ErrInvalidDecisionRequest) {
			t.Fatalf("a verified assistant call: %v", err)
		}
		if rows := attemptsOf(t, pool, task.ID); len(rows) != 0 {
			t.Fatalf("attempts=%+v", rows)
		}
	})
	t.Run("human_only_relay_waits_for_a_verified_answer", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := humanOnlyRequest(t, s, task.ID)
		pending, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "operator-desk"))
		if err != nil || pending.Result != DecisionResultPendingConfirmation {
			t.Fatalf("pending=%+v err=%v", pending, err)
		}
		got, _, _ := s.GetTask(ctx, task.ID)
		if got.Refs.DecisionRequest.Status != DecisionRequestOpen || resolutionsOf(t, pool, task.ID) != 0 {
			t.Fatalf("request after a relay: %+v", got.Refs.DecisionRequest)
		}
		direct := answerA(request, DecisionChannelCLI, "operator")
		direct.Verified = true
		if p, _, _ := resolved(t, direct); p != DecisionProvenanceOperatorDirect {
			t.Fatalf("provenance=%s", p)
		}
		var results []string
		for _, a := range attemptsOf(t, pool, task.ID) {
			results = append(results, a.Result)
		}
		if strings.Join(results, ",") != "pending_confirmation,answered" {
			t.Fatalf("attempts=%v", results)
		}
	})
}

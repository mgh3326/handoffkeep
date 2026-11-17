package store

import (
	"errors"
	"testing"
	"time"
)

func init() {
	registerTaskAreaCase("decisions/chat-inbox-roundtrip", func(t *testing.T, s *Store) {
		ctx := t.Context()
		id := "Q-20261118-97"
		q, created, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: id, Lane: "decision-area-case", Body: "Which route?"})
		if err != nil || !created || q.Revision != 1 || q.State != "pending" {
			t.Fatalf("first question = %+v created=%v err=%v", q, created, err)
		}
		q, created, err = s.UpsertChatQuestion(ctx, ChatQuestion{ID: id, Lane: "decision-area-case", Body: "Which route now?"})
		if err != nil || created || q.Revision != 2 || q.Body != "Which route now?" {
			t.Fatalf("revised question = %+v created=%v err=%v", q, created, err)
		}
		got, found, err := s.GetChatQuestion(ctx, id)
		if err != nil || !found || got.Revision != 2 {
			t.Fatalf("GetChatQuestion = %+v found=%v err=%v", got, found, err)
		}
		questions, err := s.ListChatQuestions(ctx, "decision-area-case", "pending", "", 10)
		if err != nil || len(questions) != 1 || questions[0].ID != id {
			t.Fatalf("ListChatQuestions = %+v err=%v", questions, err)
		}

		post := ChatMessagePost{Message: ChatMessage{Author: "operator", Body: "Use the north route.", SourceChannel: "test"}, Questions: []ChatQuestion{q}}
		post.Message.OriginEventID = "decisions-area-case"
		message, created, err := s.PostChatMessage(ctx, post)
		if err != nil || !created || message.ID < 1 || len(message.QuestionRelations) != 1 || message.QuestionRelations[0].RelationKind != "posted" {
			t.Fatalf("PostChatMessage = %+v created=%v err=%v", message, created, err)
		}
		replay, created, err := s.PostChatMessage(ctx, post)
		if err != nil || created || replay.ID != message.ID {
			t.Fatalf("replayed PostChatMessage = %+v created=%v err=%v", replay, created, err)
		}
		if _, err := s.MarkChatMessageDelivered(ctx, message.ID); err != nil {
			t.Fatalf("MarkChatMessageDelivered: %v", err)
		}
		messages, err := s.ListChatMessages(ctx, "operator", false, 0, 10)
		if err != nil || len(messages) != 1 || messages[0].ID != message.ID || messages[0].RelayState != "delivered" {
			t.Fatalf("ListChatMessages = %+v err=%v", messages, err)
		}

		var requestID int64
		if err := s.db().QueryRow(ctx, s.q(`SELECT id FROM decision_requests WHERE source='chat' AND source_ref=$1`, `SELECT id FROM decision_requests WHERE source='chat' AND source_ref=?`), id).Scan(&requestID); err != nil {
			t.Fatal(err)
		}
		records, err := s.ListDecisionRecords(ctx, DecisionRecordFilter{Source: DecisionSourceChat, Limit: 10}, time.Now().UTC())
		if err != nil || len(records) != 1 || records[0].ID != requestID {
			t.Fatalf("ListDecisionRecords = %+v err=%v", records, err)
		}
		record, found, err := s.GetDecisionRecord(ctx, requestID, time.Now().UTC())
		if err != nil || !found || record.SourceRef != id {
			t.Fatalf("GetDecisionRecord = %+v found=%v err=%v", record, found, err)
		}
		notice, found, err := s.GetDecisionNotice(ctx, requestID)
		if err != nil || !found || notice.RequestID != requestID || notice.State != DecisionDeliveryPending {
			t.Fatalf("GetDecisionNotice = %+v found=%v err=%v", notice, found, err)
		}
		hygiene, err := s.DecisionDeliveryHygiene(ctx, time.Now().UTC())
		if err != nil || hygiene.Destinations[DecisionDestinationDiscord].Pending < 1 {
			t.Fatalf("DecisionDeliveryHygiene = %+v err=%v", hygiene, err)
		}
		until := time.Now().UTC().Add(time.Hour)
		written, err := s.ApplyInbox(ctx, []string{"decision:request:" + itoa(requestID)}, InboxActionSnooze, &until, "area-case", "chat-inbox-area-case")
		if err != nil || len(written.Items) != 1 || written.Items[0].Err != nil {
			t.Fatalf("ApplyInbox = %+v err=%v", written, err)
		}
		states, err := s.InboxStates(ctx, []string{"decision:request:" + itoa(requestID)})
		if err != nil || states["decision:request:"+itoa(requestID)].SnoozedUntil == nil {
			t.Fatalf("InboxStates = %+v err=%v", states, err)
		}
		undone, err := s.UndoInbox(ctx, "chat-inbox-area-case", "area-case")
		if err != nil || len(undone) != 1 || undone[0].Err != nil {
			t.Fatalf("UndoInbox = %+v err=%v", undone, err)
		}
		states, err = s.InboxStates(ctx, []string{"decision:request:" + itoa(requestID)})
		if err != nil {
			t.Fatal(err)
		}
		if state, ok := states["decision:request:"+itoa(requestID)]; ok && state.SnoozedUntil != nil {
			t.Fatalf("undo left inbox snoozed: %+v", state)
		}
		if _, err := s.TransitionChatQuestion(ctx, id, "resolved", "area-case"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.TransitionChatQuestion(ctx, id, "withdrawn", "area-case"); !errors.Is(err, ErrChatQuestionConflict) {
			t.Fatalf("second transition err=%v, want ErrChatQuestionConflict", err)
		}
	})
}

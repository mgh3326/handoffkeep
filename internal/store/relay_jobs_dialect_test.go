package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func init() {
	registerTaskAreaCase("relay-jobs/claim-and-update", func(t *testing.T, s *Store) {
		task, err := s.CreateTask(t.Context(), Task{Lane: "relay-jobs", Title: "Dialect job fixture", Kind: "implement", Priority: 5, CreatedBy: "dialect-test"})
		if err != nil {
			t.Fatalf("CreateTask() = %v", err)
		}
		job, created, err := s.ClaimJob(t.Context(), Job{JobID: "dialect-job", TaskID: &task.ID, Host: "test-host", CreatedBy: "dialect-test"})
		if err != nil || !created {
			t.Fatalf("ClaimJob() = (%+v, %t, %v)", job, created, err)
		}
		state := "spawned"
		now := time.Now().UTC()
		job, err = s.UpdateJob(t.Context(), job.JobID, JobUpdate{State: &state, At: &now})
		if err != nil || job.State != state {
			t.Fatalf("UpdateJob(spawned) = (%+v, %v)", job, err)
		}
		state = "completed"
		job, err = s.UpdateJob(t.Context(), job.JobID, JobUpdate{State: &state, At: &now})
		if err != nil || job.State != state || job.EndedAt == nil {
			t.Fatalf("UpdateJob() = (%+v, %v)", job, err)
		}
		got, found, err := s.GetJob(t.Context(), job.JobID)
		if err != nil || !found || got.State != state {
			t.Fatalf("GetJob() = (%+v, %t, %v)", got, found, err)
		}
		jobs, err := s.ListJobsByTask(t.Context(), task.ID, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("ListJobsByTask() = (%+v, %v)", jobs, err)
		}
		active, err := s.ReadActiveJobs(t.Context(), nil)
		if err != nil || active == nil {
			t.Fatalf("empty ReadActiveJobs() = (%+v, %v)", active, err)
		}
		job, _, err = s.ClaimJob(t.Context(), Job{JobID: "dialect-job-skills", TaskID: &task.ID, Host: "test-host", CreatedBy: "dialect-test"})
		if err != nil {
			t.Fatal(err)
		}
		supported := true
		skills := []JobSkill{{Name: "capability", Source: "repo", Calls: 2}}
		if _, err = s.UpdateJob(t.Context(), job.JobID, JobUpdate{SkillsSupported: &supported, Skills: &skills}); err != nil {
			t.Fatalf("UpdateJob(skills) = %v", err)
		}
		report, err := s.JobSkillsReport(t.Context(), time.Now().UTC().Add(-time.Hour), 10)
		if err != nil || report.JobsTotal != 2 || report.JobsSkillsSupported != 1 || len(report.ByHarnessModel) != 1 || len(report.ByHarnessModel[0].Skills) != 1 || report.ByHarnessModel[0].Skills[0].Calls != "2" {
			t.Fatalf("JobSkillsReport() = (%+v, %v)", report, err)
		}
		emptyReport, err := s.JobSkillsReport(t.Context(), time.Now().UTC().Add(24*time.Hour), 10)
		if err != nil || emptyReport.JobsTotal != 0 || emptyReport.JobsSkillsSupported != 0 || emptyReport.JobsSkillsUnsupported != 0 || emptyReport.JobsSkillsUnknown != 0 {
			t.Fatalf("empty-window JobSkillsReport() = (%+v, %v)", emptyReport, err)
		}
		active, err = s.ReadActiveJobs(t.Context(), []int64{task.ID})
		if err != nil || active[task.ID].Role == "" {
			t.Fatalf("ReadActiveJobs() = (%+v, %v)", active, err)
		}
	})

	registerTaskAreaCase("relay-jobs/events-and-placements", func(t *testing.T, s *Store) {
		event := RelayEvent{Kind: "lane.event", OwnerLane: "dialect-lane", EventID: "dialect-event", Text: "ready"}
		stored, created, err := s.AppendRelayEvent(t.Context(), event)
		if err != nil || !created || stored.Attempts != 0 {
			t.Fatalf("AppendRelayEvent() = (%+v, %t, %v)", stored, created, err)
		}
		stored, created, err = s.AppendRelayEvent(t.Context(), event)
		if err != nil || created || stored.Attempts != 1 {
			t.Fatalf("duplicate AppendRelayEvent() = (%+v, %t, %v)", stored, created, err)
		}
		page, err := s.ListRelayEventsPage(t.Context(), event.OwnerLane, "lane.event", true, 0, 10)
		if err != nil || len(page) != 1 {
			t.Fatalf("ListRelayEventsPage() = (%+v, %v)", page, err)
		}
		delivered, err := s.MarkRelayEventDelivered(t.Context(), stored.ID, "test-host", "pane-1")
		if err != nil || delivered.DeliveredAt == nil {
			t.Fatalf("MarkRelayEventDelivered() = (%+v, %v)", delivered, err)
		}

		placement := PlacementDecision{JobID: "dialect-job", Attempt: 1, DecidedAt: time.Now().UTC(), DecidedOn: "test-host", Source: "auto", Ranking: "[]", StateSource: "test", Outcome: "failed", Candidates: []PlacementCandidate{{Host: "candidate-host", Outcome: "refused", Reason: "busy"}}}
		first, created, err := s.RecordPlacement(t.Context(), placement)
		if err != nil || !created {
			t.Fatalf("RecordPlacement() = (%+v, %t, %v)", first, created, err)
		}
		second, created, err := s.RecordPlacement(t.Context(), placement)
		if err != nil || created || second.ID != first.ID {
			t.Fatalf("duplicate RecordPlacement() = (%+v, %t, %v)", second, created, err)
		}
		fleet, err := s.ListFleetCapacityPlacements(t.Context(), "", "candidate-host", time.Now().UTC().Add(-time.Hour), 10)
		if err != nil || len(fleet) != 1 || fleet[0].ID != first.ID {
			t.Fatalf("candidate-only ListFleetCapacityPlacements() = (%+v, %v)", fleet, err)
		}

		open, created, err := s.AppendRelayEvent(t.Context(), RelayEvent{Kind: "lane.event", OwnerLane: "decision-lane", EventID: "decision-event", Text: "[decision-needed] question"})
		if err != nil || !created {
			t.Fatalf("AppendRelayEvent(decision) = (%+v, %t, %v)", open, created, err)
		}
		decisions, err := s.ListOpenLaneDecisions(t.Context(), 10)
		if err != nil || len(decisions) != 1 || decisions[0].ID != open.ID {
			t.Fatalf("ListOpenLaneDecisions() = (%+v, %v)", decisions, err)
		}
		_, _, err = s.AppendRelayEvent(t.Context(), RelayEvent{Kind: "lane.event", OwnerLane: "decision-lane", EventID: "answer-event", Text: "[decision-answered] #" + fmt.Sprint(open.ID) + ": answered"})
		if err != nil {
			t.Fatalf("AppendRelayEvent(answer) = %v", err)
		}
		decisions, err = s.ListOpenLaneDecisions(t.Context(), 10)
		if err != nil || len(decisions) != 0 {
			t.Fatalf("answered lane decisions = (%+v, %v)", decisions, err)
		}
	})

	registerTaskAreaCase("relay-jobs/attachment-outbox-ledger", func(t *testing.T, s *Store) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		sha := strings.Repeat("a", 64)
		if _, _, err := s.PutAttachment(t.Context(), Attachment{SHA256: sha, SizeBytes: 3, MIME: "text/plain", OriginalName: "dialect.txt", CreatedBy: "dialect-test", RefKind: "none"}, 100, 10); err != nil {
			t.Fatalf("PutAttachment() = %v", err)
		}
		listed, err := s.ListAttachments(t.Context(), "", "", 10)
		if err != nil || len(listed) != 1 || listed[0].SHA256 != sha {
			t.Fatalf("unfiltered ListAttachments() = (%+v, %v)", listed, err)
		}
		upload := AttachmentUpload{ID: strings.Repeat("b", 32), StagingKey: "staging/dialect", Name: "dialect.txt", MIME: "text/plain", SizeBytes: 4, SHA256: sha, RefKind: "none", CreatedBy: "dialect-test", CreatedAt: now, ExpiresAt: now.Add(time.Hour), UpdatedAt: now}
		if err := s.ReserveAttachmentUpload(t.Context(), upload, 100, 100, 100); err != nil {
			t.Fatalf("ReserveAttachmentUpload() = %v", err)
		}

		tx, release, err := s.begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = insertNotificationOutboxTx(t.Context(), tx, OutboxKindDotsReport, "dialect-lane", "dialect-outbox", "ready", now); err != nil {
			_ = tx.Rollback(t.Context())
			release()
			t.Fatalf("insertNotificationOutbox() = %v", err)
		}
		if err = tx.Commit(t.Context()); err != nil {
			release()
			t.Fatal(err)
		}
		release()
		unsent, err := s.ListUnsentNotifications(t.Context(), 10, now.Add(-time.Minute))
		if err != nil || len(unsent) != 1 || unsent[0].EventID != "dialect-outbox" {
			t.Fatalf("ListUnsentNotifications() = (%+v, %v)", unsent, err)
		}
		if _, err = s.MarkNotificationApplied(t.Context(), "dialect-outbox", "sent"); !errors.Is(err, ErrOutboxNotReceived) {
			t.Fatalf("applied before sent with identity 'sent' = %v", err)
		}
		if _, err = s.MarkNotificationSent(t.Context(), "dialect-outbox", 7); err != nil {
			t.Fatalf("MarkNotificationSent() = %v", err)
		}
		applied, err := s.MarkNotificationApplied(t.Context(), "dialect-outbox", "sent")
		if err != nil || applied.AppliedAt == nil || applied.AppliedBy == nil || *applied.AppliedBy != "sent" {
			t.Fatalf("MarkNotificationApplied() = (%+v, %v)", applied, err)
		}

		if _, err = s.db().Exec(t.Context(), s.q(`UPDATE sync_state SET max_sync_id=9,pruned_through=8 WHERE singleton`, `UPDATE sync_state SET max_sync_id=9,pruned_through=8 WHERE singleton=1`)); err != nil {
			t.Fatal(err)
		}
		cursor, err := s.AckLedger(t.Context(), "first-ack", 5, "dialect-test", now)
		if err != nil || cursor.AckedSyncID != 9 {
			t.Fatalf("first AckLedger below watermark = (%+v, %v)", cursor, err)
		}
	})
}

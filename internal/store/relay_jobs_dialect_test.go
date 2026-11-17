package store

import (
	"testing"
	"time"
)

func init() {
	registerTaskAreaCase("relay-jobs/claim-and-update", func(t *testing.T, s *Store) {
		job, created, err := s.ClaimJob(t.Context(), Job{JobID: "dialect-job", Host: "test-host", CreatedBy: "dialect-test"})
		if err != nil || !created {
			t.Fatalf("ClaimJob() = (%+v, %t, %v)", job, created, err)
		}
		state := "completed"
		job, err = s.UpdateJob(t.Context(), job.JobID, JobUpdate{State: &state})
		if err != nil || job.State != state || job.EndedAt == nil {
			t.Fatalf("UpdateJob() = (%+v, %v)", job, err)
		}
		got, found, err := s.GetJob(t.Context(), job.JobID)
		if err != nil || !found || got.State != state {
			t.Fatalf("GetJob() = (%+v, %t, %v)", got, found, err)
		}
		jobs, err := s.ListJobsByTask(t.Context(), 1, 10)
		if err != nil || jobs == nil {
			t.Fatalf("ListJobsByTask() = (%+v, %v)", jobs, err)
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

		placement := PlacementDecision{JobID: "dialect-job", Attempt: 1, DecidedAt: time.Now().UTC(), DecidedOn: "test-host", Source: "auto", Ranking: "[]", StateSource: "test", Outcome: "failed", Candidates: []PlacementCandidate{}}
		first, created, err := s.RecordPlacement(t.Context(), placement)
		if err != nil || !created {
			t.Fatalf("RecordPlacement() = (%+v, %t, %v)", first, created, err)
		}
		second, created, err := s.RecordPlacement(t.Context(), placement)
		if err != nil || created || second.ID != first.ID {
			t.Fatalf("duplicate RecordPlacement() = (%+v, %t, %v)", second, created, err)
		}
	})
}

package tests

import (
	"context"
	"strconv"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Task-447: a lane decision closes only when a later same-lane lane.event has
// the exact text prefix "[decision-answered] #<question event id>:". Any other
// [decision-answered] row — generic, wrong id, missing colon, or foreign lane —
// must leave the question open.
func TestLaneDecisionClosesOnlyOnExactID(t *testing.T) {
	s := uiStore(t)

	openIDs := func() map[int64]bool {
		t.Helper()
		open, err := s.ListOpenLaneDecisions(t.Context(), 1000)
		if err != nil {
			t.Fatal(err)
		}
		ids := map[int64]bool{}
		for _, event := range open {
			ids[event.ID] = true
		}
		return ids
	}
	answer := func(lane string, id int64) {
		t.Helper()
		seedRelay(t, s, lane, "lane.event", "", "[decision-answered] #"+strconv.FormatInt(id, 10)+": resolved", "", "")
	}
	question := func(lane, text string) store.RelayEvent {
		t.Helper()
		return seedRelay(t, s, lane, "lane.event", "", "[decision-needed] "+text, "", "")
	}
	// Cleanup closes the questions the subtests intentionally leave open so the
	// shared database does not leak cards into later tests' decision pages.
	var leftovers []store.RelayEvent
	t.Cleanup(func() {
		for index, q := range leftovers {
			if _, _, err := s.AppendRelayEvent(context.Background(), store.RelayEvent{
				Kind: "lane.event", JobID: "t447-cleanup", OwnerLane: q.OwnerLane,
				Text:    "[decision-answered] #" + strconv.FormatInt(q.ID, 10) + ": cleanup",
				EventID: "t447-cleanup-" + strconv.Itoa(index),
			}); err != nil {
				t.Logf("exact-id cleanup: %v", err)
			}
		}
	})

	t.Run("sibling stays open", func(t *testing.T) {
		lane := uiLane(t, "t447-pair")
		first := question(lane, "first")
		second := question(lane, "second")
		answer(lane, first.ID)
		ids := openIDs()
		if ids[first.ID] {
			t.Fatalf("exactly answered decision %d still open", first.ID)
		}
		if !ids[second.ID] {
			t.Fatalf("unanswered sibling %d hidden by another question's answer", second.ID)
		}
		leftovers = append(leftovers, second)
	})

	t.Run("generic answer closes nothing", func(t *testing.T) {
		lane := uiLane(t, "t447-generic")
		q := question(lane, "generic check")
		seedRelay(t, s, lane, "lane.event", "", "[decision-answered] answer without any id", "", "")
		if openIDs()[q.ID] != true {
			t.Fatalf("generic [decision-answered] closed question %d", q.ID)
		}
		leftovers = append(leftovers, q)
	})

	t.Run("different event id does not close", func(t *testing.T) {
		lane := uiLane(t, "t447-wrong")
		q := question(lane, "wrong id")
		answer(lane, q.ID+1)
		if !openIDs()[q.ID] {
			t.Fatalf("answer for a different event id closed question %d", q.ID)
		}
		leftovers = append(leftovers, q)
	})

	t.Run("id without colon boundary does not close", func(t *testing.T) {
		lane := uiLane(t, "t447-boundary")
		q := question(lane, "boundary")
		seedRelay(t, s, lane, "lane.event", "", "[decision-answered] #"+strconv.FormatInt(q.ID, 10), "", "")
		seedRelay(t, s, lane, "lane.event", "", "[decision-answered] #"+strconv.FormatInt(q.ID, 10)+"9: not a match", "", "")
		if !openIDs()[q.ID] {
			t.Fatalf("answer without the exact #<id>: boundary closed question %d", q.ID)
		}
		leftovers = append(leftovers, q)
	})

	t.Run("other lane answer does not close", func(t *testing.T) {
		lane := uiLane(t, "t447-foreign")
		q := question(lane, "foreign lane")
		answer(uiLane(t, "t447-elsewhere"), q.ID)
		if !openIDs()[q.ID] {
			t.Fatalf("another lane's answer closed question %d", q.ID)
		}
		leftovers = append(leftovers, q)
	})
}

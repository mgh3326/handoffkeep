package fleetmetrics

import (
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// t0 is the fixture window start; every fixture time is an offset from it.
var t0 = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

func at(h float64) time.Time { return t0.Add(time.Duration(h * float64(time.Hour))) }
func atp(h float64) *time.Time {
	x := at(h)
	return &x
}

// ev is one task transition at hour h.
func ev(h float64, from, to string, refs *store.TaskRefs, note ...string) store.TaskEvent {
	e := store.TaskEvent{Kind: "transition", From: from, To: to, By: "mac-personal", Refs: refs, At: at(h)}
	if len(note) > 0 {
		e.Note = note[0]
	}
	return e
}

func task(id int64, state string, created float64, refs store.TaskRefs, events ...store.TaskEvent) store.Task {
	for i := range events {
		events[i].TaskID = id
		events[i].ID = id*100 + int64(i)
	}
	if events == nil {
		events = []store.TaskEvent{}
	}
	updated := at(created)
	if n := len(events); n > 0 {
		updated = events[n-1].At
	}
	return store.Task{ID: id, Lane: "director-1", Title: "fixture", Kind: "implement", State: state, Refs: refs, CreatedAt: at(created), UpdatedAt: updated, Events: events}
}

// merged is the normal-path transition chain claimed → in_progress →
// verifying → merged.
func mergedChain(claim, verify, merge float64, refs *store.TaskRefs) []store.TaskEvent {
	return []store.TaskEvent{
		ev(claim, "backlog", "claimed", refs),
		ev(claim, "claimed", "in_progress", refs),
		ev(verify, "in_progress", "verifying", refs),
		ev(merge, "verifying", "merged", refs),
	}
}

func jev(kind string, h float64) JobEvent {
	return JobEvent{Kind: kind, At: atp(h), AtSource: "payload"}
}

func snap(since, until float64) Snapshot {
	return Snapshot{Schema: SnapshotSchema, CollectedAt: at(until), Since: at(since), Until: at(until), PRs: map[string]PRFact{}, Contains: map[string]bool{}, TaskComments: map[int64][]Comment{}}
}

func findCoverage(cs []Coverage, prefix string) Coverage {
	for _, c := range cs {
		if len(c.What) >= len(prefix) && c.What[:len(prefix)] == prefix {
			return c
		}
	}
	return Coverage{What: "NOT FOUND: " + prefix}
}

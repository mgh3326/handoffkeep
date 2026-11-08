package youtrack

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The reads and writes the issue text and the DEC mirror add to the store.
// They sit beside fakeStore's core so each file reads on its own.

func changeKey(repo string, pr int) string { return fmt.Sprintf("%s#%d", repo, pr) }

func (f *fakeStore) ListTaskEvents(_ context.Context, taskID, afterID int64, limit int) ([]store.TaskEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.TaskEvent{}
	for _, e := range f.events[taskID] {
		if e.ID > afterID && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) ListTasksFiltered(_ context.Context, q store.TaskQuery) ([]store.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, 0, len(f.stalled))
	for id := range f.stalled {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []store.Task{}
	for _, id := range ids {
		if task, ok := f.tasks[id]; ok && q.Stalled {
			task.Stalled = true
			out = append(out, task)
		}
	}
	return out, nil
}

func (f *fakeStore) GetDocument(_ context.Context, key string) (store.Document, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, ok := f.docs[key]
	return doc, ok, nil
}

func (f *fakeStore) ListChangesByTask(_ context.Context, taskID int64, _ int) ([]store.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Change(nil), f.changes[taskID]...), nil
}

func (f *fakeStore) GetChangeState(_ context.Context, ref store.ChangeRef, _ store.ChangeReadOptions) (store.ChangeView, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	view, ok := f.changeStates[changeKey(ref.Repo, ref.PR)]
	return view, ok, nil
}

func (f *fakeStore) ListDecisionRecords(_ context.Context, filter store.DecisionRecordFilter, _ time.Time) ([]store.DecisionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.DecisionRecord{}
	for _, d := range f.decisions {
		if d.Status == filter.Status {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeStore) GetYouTrackDecisionMirror(_ context.Context, decisionID int64) (store.YouTrackDecisionMirror, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.decisionMirrors[decisionID]
	return x, ok, nil
}

func (f *fakeStore) ListYouTrackDecisionMirrors(_ context.Context, ids []int64) (map[int64]store.YouTrackDecisionMirror, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]store.YouTrackDecisionMirror{}
	for _, id := range ids {
		if x, ok := f.decisionMirrors[id]; ok {
			out[id] = x
		}
	}
	return out, nil
}

func (f *fakeStore) PutYouTrackDecisionMirror(_ context.Context, x store.YouTrackDecisionMirror) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisionMirrors[x.DecisionID] = x
	return nil
}

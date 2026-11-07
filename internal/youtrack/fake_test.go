package youtrack

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// fakeStore is the service's store seam. It keeps the same observable rules
// the SQL store enforces where the sync cares: a task that is not claimable
// refuses a move into a started state, merged needs closure evidence, and an
// activity id is applied once.
type fakeStore struct {
	mu           sync.Mutex
	tasks        map[int64]store.Task
	links        map[int64]store.YouTrackLink
	comments     map[int64][]store.TaskComment
	caches       map[int64]store.YouTrackFieldCache
	translations map[int64]store.Translation
	projects     map[string]bool
	cursor       store.YouTrackCursor
	applied      map[string]store.YouTrackApplyResult
	pending      map[string]store.YouTrackPendingRevert
	pushed       map[int64]bool
	advanced     []string
	writes       int
	nextTaskID   int64
	nextComment  int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tasks:        map[int64]store.Task{},
		links:        map[int64]store.YouTrackLink{},
		comments:     map[int64][]store.TaskComment{},
		caches:       map[int64]store.YouTrackFieldCache{},
		translations: map[int64]store.Translation{},
		projects:     map[string]bool{"handoffkeep": true, "other": true},
		applied:      map[string]store.YouTrackApplyResult{},
		pending:      map[string]store.YouTrackPendingRevert{},
		pushed:       map[int64]bool{},
		nextTaskID:   100,
		nextComment:  200,
	}
}

func (f *fakeStore) addTask(lane, title, state string, priority int) store.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextTaskID++
	now := time.Now().UTC().Truncate(time.Microsecond)
	project := "handoffkeep"
	task := store.Task{ID: f.nextTaskID, Lane: lane, Title: title, State: state, Priority: priority, Kind: "implement", CreatedBy: "test", CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Project: &project}
	f.tasks[task.ID] = task
	return task
}

func (f *fakeStore) link(taskID int64, issueID, readable string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	pushed := f.tasks[taskID].UpdatedAt
	f.links[taskID] = store.YouTrackLink{TaskID: taskID, IssueID: issueID, ReadableID: readable, PushedTaskUpdatedAt: &pushed, CreatedAt: now, UpdatedAt: now}
}

func (f *fakeStore) GetTask(_ context.Context, id int64) (store.Task, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, ok := f.tasks[id]
	return task, ok, nil
}

func (f *fakeStore) ListTaskComments(_ context.Context, taskID, afterID int64, limit int) ([]store.TaskComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.TaskComment{}
	for _, c := range f.comments[taskID] {
		if c.ID > afterID && len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) ListTranslations(_ context.Context, _ string, sources []store.TranslationSource) ([]store.Translation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.Translation{}
	for _, source := range sources {
		if t, ok := f.translations[taskIDFromEntity(source.EntityID)]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func taskIDFromEntity(entity string) int64 {
	var id int64
	fmt.Sscan(entity, &id)
	return id
}

func (f *fakeStore) TaskProjectExists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projects[name], nil
}

func (f *fakeStore) GetYouTrackLink(_ context.Context, taskID int64) (store.YouTrackLink, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	link, ok := f.links[taskID]
	return link, ok, nil
}

func (f *fakeStore) FindYouTrackLinkByReadableID(_ context.Context, readableID string) (store.YouTrackLink, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, link := range f.links {
		if link.ReadableID == readableID {
			return link, true, nil
		}
	}
	return store.YouTrackLink{}, false, nil
}

func (f *fakeStore) ListYouTrackLinks(_ context.Context, taskIDs []int64) (map[int64]store.YouTrackLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]store.YouTrackLink{}
	for _, id := range taskIDs {
		if link, ok := f.links[id]; ok {
			out[id] = link
		}
	}
	return out, nil
}

func (f *fakeStore) LinkYouTrackIssue(_ context.Context, taskID int64, issueID, readableID string, at time.Time) (store.YouTrackLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	link := store.YouTrackLink{TaskID: taskID, IssueID: issueID, ReadableID: readableID, CreatedAt: at, UpdatedAt: at}
	f.links[taskID] = link
	f.writes++
	return link, nil
}

func (f *fakeStore) MarkYouTrackTaskPushed(_ context.Context, taskID int64, updatedAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	link := f.links[taskID]
	link.PushedTaskUpdatedAt = &updatedAt
	f.links[taskID] = link
	f.pushed[taskID] = true
	return nil
}

func (f *fakeStore) TouchYouTrackTaskPushed(_ context.Context, taskID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if task, ok := f.tasks[taskID]; ok {
		at := task.UpdatedAt
		link := f.links[taskID]
		link.PushedTaskUpdatedAt = &at
		f.links[taskID] = link
	}
	return nil
}

func (f *fakeStore) GetYouTrackFieldCache(_ context.Context, taskID int64) (store.YouTrackFieldCache, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cache, ok := f.caches[taskID]
	return cache, ok, nil
}

func (f *fakeStore) GetYouTrackFieldCaches(_ context.Context, taskIDs []int64) (map[int64]store.YouTrackFieldCache, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]store.YouTrackFieldCache{}
	for _, id := range taskIDs {
		if cache, ok := f.caches[id]; ok {
			out[id] = cache
		}
	}
	return out, nil
}

func (f *fakeStore) PutYouTrackFieldCache(_ context.Context, x store.YouTrackFieldCache) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caches[x.TaskID] = x
	f.writes++
	return nil
}

func (f *fakeStore) SetYouTrackCachedPriority(_ context.Context, taskID int64, priority int, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cache := f.caches[taskID]
	cache.TaskID, cache.Priority, cache.FetchedAt, cache.Stale = taskID, priority, at, false
	if cache.Lane == "" {
		cache.Lane = f.tasks[taskID].Lane
	}
	f.caches[taskID] = cache
	return nil
}

func (f *fakeStore) SetYouTrackCachedLane(_ context.Context, taskID int64, lane string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cache := f.caches[taskID]
	cache.TaskID, cache.Lane, cache.FetchedAt, cache.Stale = taskID, lane, at, false
	f.caches[taskID] = cache
	return nil
}

func (f *fakeStore) MarkYouTrackFieldCacheStale(_ context.Context, taskID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cache := f.caches[taskID]
	cache.Stale = true
	f.caches[taskID] = cache
	return nil
}

func (f *fakeStore) RefreshYouTrackFields(_ context.Context, _ time.Duration, _ int) ([]store.YouTrackLink, error) {
	return nil, nil
}

func (f *fakeStore) DueYouTrackPushTasks(_ context.Context, limit int) ([]store.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, 0, len(f.tasks))
	for id := range f.tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []store.Task{}
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		task := f.tasks[id]
		link, linked := f.links[id]
		if !linked && IsSettled(task.State) {
			continue
		}
		if linked && link.PushedTaskUpdatedAt != nil && !task.UpdatedAt.After(*link.PushedTaskUpdatedAt) {
			continue
		}
		out = append(out, task)
	}
	return out, nil
}

func (f *fakeStore) UnpushedYouTrackComments(_ context.Context, taskID, afterID int64, limit int) ([]store.TaskComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.TaskComment{}
	for _, c := range f.comments[taskID] {
		if c.ID > afterID && c.Author != store.YouTrackOwnedActor && len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) LinkYouTrackComment(_ context.Context, _ int64, _ int64, _ string, _ time.Time) error {
	return nil
}

func (f *fakeStore) FindYouTrackCommentLinkByYouTrackID(_ context.Context, _ string) (store.YouTrackCommentLink, bool, error) {
	return store.YouTrackCommentLink{}, false, nil
}

func (f *fakeStore) GetYouTrackCursor(_ context.Context) (store.YouTrackCursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursor, nil
}

func (f *fakeStore) AdvanceYouTrackCursor(_ context.Context, cursor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursor.Cursor = cursor
	f.advanced = append(f.advanced, cursor)
	return nil
}

func (f *fakeStore) ApplyYouTrackActivity(_ context.Context, in store.YouTrackActivity) (store.YouTrackApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.applied[in.ID]; ok {
		return store.YouTrackApplyResult{Applied: false, Outcome: store.YouTrackOutcomeDuplicate}, nil
	}
	if in.Skip != "" {
		result := store.YouTrackApplyResult{Applied: true, Outcome: store.YouTrackOutcomeSkipped}
		f.applied[in.ID] = result
		return result, nil
	}
	if in.NewIssue != nil {
		f.nextTaskID++
		now := time.Now().UTC()
		project := in.NewIssue.Project
		task := store.Task{ID: f.nextTaskID, Lane: in.NewIssue.Lane, Title: in.NewIssue.Title, State: store.StateTriage, Kind: in.NewIssue.Kind, CreatedBy: store.YouTrackOwnedActor, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Project: &project}
		f.tasks[task.ID] = task
		f.links[task.ID] = store.YouTrackLink{TaskID: task.ID, IssueID: in.NewIssue.IssueID, ReadableID: in.NewIssue.ReadableID, CreatedAt: now, UpdatedAt: now}
		result := store.YouTrackApplyResult{Applied: true, Outcome: store.YouTrackOutcomeApplied}
		f.applied[in.ID] = result
		f.writes++
		return result, nil
	}
	task := f.tasks[in.TaskID]
	result := store.YouTrackApplyResult{Applied: true, Outcome: store.YouTrackOutcomeApplied}
	if in.State != "" && in.State != task.State {
		refusal := ""
		switch {
		case IsSettled(task.State) && in.State != store.StateUnstarted:
			refusal = "정리된 작업은 착수 전으로만 되돌릴 수 있습니다"
		case (in.State == store.StateDeveloping || in.State == store.StateResearching || in.State == store.StateReviewing) && task.ClaimedBy == "":
			refusal = "작업을 시작하려면 claim과 job 연결이 필요합니다"
		case in.State == store.StateMerged && task.Refs.PR == "":
			refusal = "종료 근거가 없어 hk가 상태를 바꾸지 않았습니다"
		}
		if refusal != "" {
			result = store.YouTrackApplyResult{Applied: true, Outcome: store.YouTrackOutcomeRejected, Refusal: refusal, RevertField: "state", RevertValue: task.State}
			f.pending[in.ID] = store.YouTrackPendingRevert{ActivityID: in.ID, IssueID: in.IssueID, RevertField: "state", RevertValue: task.State, Refusal: refusal}
		} else {
			task.State = in.State
			task.UpdatedAt = time.Now().UTC()
		}
	}
	if in.Priority != nil && *in.Priority != task.Priority {
		task.Priority = *in.Priority
		cache := f.caches[in.TaskID]
		cache.TaskID, cache.Priority, cache.Lane = in.TaskID, *in.Priority, task.Lane
		f.caches[in.TaskID] = cache
	}
	if in.Lane != "" && in.Lane != task.Lane {
		task.Lane = in.Lane
		cache := f.caches[in.TaskID]
		cache.TaskID, cache.Priority, cache.Lane = in.TaskID, task.Priority, in.Lane
		f.caches[in.TaskID] = cache
	}
	if in.Comment != "" {
		f.nextComment++
		f.comments[in.TaskID] = append(f.comments[in.TaskID], store.TaskComment{ID: f.nextComment, TaskID: in.TaskID, Body: in.Comment, Author: store.YouTrackOwnedActor, CreatedAt: time.Now().UTC()})
	}
	f.tasks[in.TaskID] = task
	f.applied[in.ID] = result
	f.writes++
	return result, nil
}

func IsSettled(state string) bool {
	switch state {
	case store.StateMerged, store.StateDeployed, store.StateCompleted, store.StateDropped, store.StateDuplicate:
		return true
	}
	return false
}

func (f *fakeStore) PendingYouTrackReverts(_ context.Context, limit int) ([]store.YouTrackPendingRevert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.YouTrackPendingRevert{}
	for _, p := range f.pending {
		if len(out) >= limit {
			break
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeStore) MarkYouTrackRevertDone(_ context.Context, activityID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pending, activityID)
	return nil
}

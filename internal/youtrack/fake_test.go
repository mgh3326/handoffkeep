package youtrack

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// fakeStore is the service's store seam. It keeps the same observable rules
// the SQL store enforces where the sync cares: PG is the write authority, a
// task that is not claimable refuses a move into a started state, merged needs
// closure evidence, an activity id is applied once, and the outbox keeps one
// pending row per field.
type fakeStore struct {
	mu           sync.Mutex
	tasks        map[int64]store.Task
	mirrors      map[int64]store.YouTrackMirror
	issueOwner   map[string]int64
	comments     map[int64][]store.TaskComment
	caches       map[int64]store.YouTrackFieldCache
	translations map[int64]store.Translation
	projects     map[string]store.YouTrackProject
	taskProjects map[string]bool
	outbox       map[int64]store.YouTrackOutboxItem
	outboxNextID int64
	outboxDue    map[int64]bool
	cursor       store.YouTrackCursor
	applied      map[string]store.YouTrackApplyResult
	applyErr     map[string]error
	pending      map[string]store.YouTrackPendingRevert
	pushed       map[int64]bool
	advanced     []string
	writes       int
	projectIDs   int
	nextTaskID   int64
	nextComment  int64

	// The reads the issue text and the decision mirror make.
	stalled         map[int64]bool
	events          map[int64][]store.TaskEvent
	docs            map[string]store.Document
	changes         map[int64][]store.Change
	changeStates    map[string]store.ChangeView
	decisions       []store.DecisionRecord
	decisionMirrors map[int64]store.YouTrackDecisionMirror
	commentLinks    map[int64]string
	docArticles     map[string]store.YouTrackDocArticle
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tasks:        map[int64]store.Task{},
		mirrors:      map[int64]store.YouTrackMirror{},
		issueOwner:   map[string]int64{},
		comments:     map[int64][]store.TaskComment{},
		caches:       map[int64]store.YouTrackFieldCache{},
		translations: map[int64]store.Translation{},
		projects: map[string]store.YouTrackProject{
			"handoffkeep": {HKProject: "handoffkeep", ShortName: "HK"},
			"panewire":    {HKProject: "panewire", ShortName: "PW"},
			"other":       {HKProject: "other", ShortName: "OTHER"},
		},
		taskProjects: map[string]bool{"handoffkeep": true, "panewire": true, "other": true},
		outbox:       map[int64]store.YouTrackOutboxItem{},
		outboxNextID: 1,
		outboxDue:    map[int64]bool{},
		applied:      map[string]store.YouTrackApplyResult{},
		applyErr:     map[string]error{},
		pending:      map[string]store.YouTrackPendingRevert{},
		pushed:       map[int64]bool{},
		nextTaskID:   100,
		nextComment:  200,

		stalled:         map[int64]bool{},
		events:          map[int64][]store.TaskEvent{},
		docs:            map[string]store.Document{},
		changes:         map[int64][]store.Change{},
		changeStates:    map[string]store.ChangeView{},
		decisionMirrors: map[int64]store.YouTrackDecisionMirror{},
		commentLinks:    map[int64]string{},
		docArticles:     map[string]store.YouTrackDocArticle{},
	}
}

func (f *fakeStore) addTask(project, lane, title, state string, priority int) store.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addTaskLocked(project, lane, title, state, priority)
}

func (f *fakeStore) addTaskLocked(project, lane, title, state string, priority int) store.Task {
	f.nextTaskID++
	now := time.Now().UTC().Truncate(time.Microsecond)
	p := project
	task := store.Task{ID: f.nextTaskID, Lane: lane, Title: title, State: state, Priority: priority, Kind: "implement", CreatedBy: "test", CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Project: &p}
	f.tasks[task.ID] = task
	return task
}

func (f *fakeStore) mirror(taskID int64, issueID, readable string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	f.mirrors[taskID] = store.YouTrackMirror{TaskID: taskID, IssueID: issueID, DisplayRef: readable, Tenant: "tenant", Role: "mirror", SyncedAt: &now, CreatedAt: now}
	f.issueOwner[issueID] = taskID
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
		id, _ := strconv.ParseInt(source.EntityID, 10, 64)
		if t, ok := f.translations[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeStore) TaskProjectExists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.taskProjects[name], nil
}

func (f *fakeStore) GetYouTrackMirror(_ context.Context, taskID int64) (store.YouTrackMirror, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mirror, ok := f.mirrors[taskID]
	return mirror, ok, nil
}

func (f *fakeStore) FindYouTrackMirrorByIssueID(_ context.Context, issueID string) (store.YouTrackMirror, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	taskID, ok := f.issueOwner[issueID]
	if !ok {
		return store.YouTrackMirror{}, false, nil
	}
	mirror, ok := f.mirrors[taskID]
	return mirror, ok, nil
}

func (f *fakeStore) ListYouTrackMirrors(_ context.Context, taskIDs []int64) (map[int64]store.YouTrackMirror, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]store.YouTrackMirror{}
	for _, id := range taskIDs {
		if mirror, ok := f.mirrors[id]; ok {
			out[id] = mirror
		}
	}
	return out, nil
}

func (f *fakeStore) UpsertYouTrackMirror(_ context.Context, in store.YouTrackMirrorInput) (store.YouTrackMirror, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if owner, ok := f.issueOwner[in.IssueID]; ok && owner != in.TaskID {
		return store.YouTrackMirror{}, store.ErrYouTrackMirrorConflict
	}
	now := time.Now().UTC()
	var synced *time.Time
	if !in.SyncedAt.IsZero() {
		at := in.SyncedAt.UTC()
		synced = &at
	}
	f.mirrors[in.TaskID] = store.YouTrackMirror{TaskID: in.TaskID, IssueID: in.IssueID, DisplayRef: in.DisplayRef, URL: in.URL, Tenant: in.Tenant, Role: in.Role, SyncedAt: synced, CreatedAt: now}
	f.issueOwner[in.IssueID] = in.TaskID
	f.writes++
	return f.mirrors[in.TaskID], nil
}

func (f *fakeStore) MarkYouTrackTaskSynced(_ context.Context, taskID int64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	mirror := f.mirrors[taskID]
	mirror.SyncedAt = &at
	f.mirrors[taskID] = mirror
	f.pushed[taskID] = true
	return nil
}

func (f *fakeStore) UpdateYouTrackMirrorDisplay(_ context.Context, taskID int64, displayRef, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	mirror, ok := f.mirrors[taskID]
	if !ok {
		return nil
	}
	mirror.DisplayRef = displayRef
	mirror.URL = url
	f.mirrors[taskID] = mirror
	f.writes++
	return nil
}

func (f *fakeStore) TouchYouTrackTaskSynced(_ context.Context, taskID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if task, ok := f.tasks[taskID]; ok {
		at := task.UpdatedAt
		mirror := f.mirrors[taskID]
		mirror.SyncedAt = &at
		f.mirrors[taskID] = mirror
	}
	return nil
}

func (f *fakeStore) ListYouTrackProjects(_ context.Context) ([]store.YouTrackProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.YouTrackProject{}
	for _, p := range f.projects {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeStore) YouTrackProjectFor(_ context.Context, hkProject string) (store.YouTrackProject, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.projects[hkProject]; ok {
		return p, true, nil
	}
	p, ok := f.projects["other"]
	return p, ok, nil
}

func (f *fakeStore) YouTrackProjectByShortName(_ context.Context, shortName string) (store.YouTrackProject, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.projects {
		if p.ShortName == shortName {
			return p, true, nil
		}
	}
	return store.YouTrackProject{}, false, nil
}

func (f *fakeStore) PutYouTrackProject(_ context.Context, p store.YouTrackProject) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects[p.HKProject] = p
	return nil
}

func (f *fakeStore) SetYouTrackProjectID(_ context.Context, hkProject, projectID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[hkProject]
	p.ProjectID = projectID
	f.projects[hkProject] = p
	f.projectIDs++
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
	if x.Source == "" {
		x.Source = store.YouTrackSourceYouTrack
	}
	f.caches[x.TaskID] = x
	f.writes++
	return nil
}

func (f *fakeStore) SetYouTrackCachedPriority(_ context.Context, taskID int64, priority int, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cache := f.caches[taskID]
	cache.TaskID, cache.Priority, cache.Source, cache.FetchedAt, cache.Stale = taskID, priority, store.YouTrackSourceHK, at, false
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
	cache.TaskID, cache.Lane, cache.Source, cache.FetchedAt, cache.Stale = taskID, lane, store.YouTrackSourceHK, at, false
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

func (f *fakeStore) RefreshYouTrackFields(_ context.Context, limit int) ([]store.YouTrackMirror, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, 0, len(f.mirrors))
	for id := range f.mirrors {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []store.YouTrackMirror{}
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		out = append(out, f.mirrors[id])
	}
	return out, nil
}

func (f *fakeStore) EnqueueYouTrack(_ context.Context, taskID int64, op, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, item := range f.outbox {
		if item.TaskID == taskID && item.Op == op && f.outboxDue[id] {
			item.Value = value
			item.Attempts = 0
			f.outbox[id] = item
			return nil
		}
	}
	id := f.outboxNextID
	f.outboxNextID++
	f.outbox[id] = store.YouTrackOutboxItem{ID: id, TaskID: taskID, Op: op, Value: value}
	f.outboxDue[id] = true
	return nil
}

func (f *fakeStore) DueYouTrackOutbox(_ context.Context, limit int) ([]store.YouTrackOutboxItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, 0, len(f.outbox))
	for id := range f.outbox {
		if f.outboxDue[id] {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []store.YouTrackOutboxItem{}
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		out = append(out, f.outbox[id])
	}
	return out, nil
}

func (f *fakeStore) MarkYouTrackOutboxDone(_ context.Context, id int64, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if item, ok := f.outbox[id]; ok && item.Value == value {
		f.outboxDue[id] = false
	}
	return nil
}

func (f *fakeStore) RescheduleYouTrackOutbox(_ context.Context, id int64, value string, attempts int, next time.Time, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := f.outbox[id]
	if item.Value != value {
		return nil
	}
	item.Attempts = attempts
	f.outbox[id] = item
	f.outboxDue[id] = !next.After(time.Now())
	return nil
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
		mirror, mirrored := f.mirrors[id]
		if !mirrored && IsSettled(task.State) {
			continue
		}
		if mirrored && mirror.SyncedAt != nil && !task.UpdatedAt.After(*mirror.SyncedAt) && !inFlight(task.State) {
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
		if _, linked := f.commentLinks[c.ID]; linked {
			continue
		}
		if c.ID > afterID && c.Author != store.YouTrackOwnedActor && len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) LinkYouTrackComment(_ context.Context, _ int64, hkCommentID int64, youtrackCommentID string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if youtrackCommentID == "" {
		return store.ErrYouTrackActivityInput
	}
	if _, ok := f.commentLinks[hkCommentID]; !ok {
		f.commentLinks[hkCommentID] = youtrackCommentID
	}
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
	if err, ok := f.applyErr[in.ID]; ok && in.Skip == "" {
		return store.YouTrackApplyResult{}, err
	}
	if _, ok := f.applied[in.ID]; ok {
		return store.YouTrackApplyResult{Applied: false, Outcome: store.YouTrackOutcomeDuplicate}, nil
	}
	if in.Skip != "" {
		result := store.YouTrackApplyResult{Applied: true, Outcome: store.YouTrackOutcomeSkipped}
		f.applied[in.ID] = result
		return result, nil
	}
	if in.NewIssue != nil {
		task := f.addTaskLocked(in.NewIssue.Project, in.NewIssue.Lane, in.NewIssue.Title, store.StateTriage, in.NewIssue.Priority)
		task.Kind = in.NewIssue.Kind
		task.CreatedBy = store.YouTrackOwnedActor
		f.tasks[task.ID] = task
		now := time.Now().UTC()
		f.mirrors[task.ID] = store.YouTrackMirror{TaskID: task.ID, IssueID: in.NewIssue.IssueID, DisplayRef: in.NewIssue.ReadableID, Tenant: in.NewIssue.Tenant, Role: "origin", SyncedAt: &now, CreatedAt: now}
		f.issueOwner[in.NewIssue.IssueID] = task.ID
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
		cache.TaskID, cache.Priority, cache.Lane, cache.Source = in.TaskID, *in.Priority, task.Lane, store.YouTrackSourceYouTrack
		f.caches[in.TaskID] = cache
		f.closeOutboxLocked(in.TaskID, store.YouTrackOpPriority)
	}
	if in.Lane != "" && in.Lane != task.Lane {
		task.Lane = in.Lane
		cache := f.caches[in.TaskID]
		cache.TaskID, cache.Priority, cache.Lane, cache.Source = in.TaskID, task.Priority, in.Lane, store.YouTrackSourceYouTrack
		f.caches[in.TaskID] = cache
		f.closeOutboxLocked(in.TaskID, store.YouTrackOpLane)
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

// closeOutboxLocked drops the pending export for a field when the operator's
// newer YouTrack value supersedes it. The caller holds f.mu.
func (f *fakeStore) closeOutboxLocked(taskID int64, op string) {
	for id, item := range f.outbox {
		if item.TaskID == taskID && item.Op == op {
			f.outboxDue[id] = false
		}
	}
}

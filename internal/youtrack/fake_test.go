package youtrack

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// fakeStore is the search seam: the tasks, mirror rows, projects, documents
// and article ledger that the YouTrack search half reads. Each search test
// installs its own DocumentKeyExists, YouTrackTaskHits and YouTrackDocHits.
type fakeStore struct {
	mu          sync.Mutex
	tasks       map[int64]store.Task
	mirrors     map[int64]store.YouTrackMirror
	issueOwner  map[string]int64
	projects    map[string]store.YouTrackProject
	docs        map[string]store.Document
	docArticles map[string]store.YouTrackDocArticle
	nextTaskID  int64
}

var docTestTime = time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)

func newFakeStore() *fakeStore {
	return &fakeStore{
		tasks:      map[int64]store.Task{},
		mirrors:    map[int64]store.YouTrackMirror{},
		issueOwner: map[string]int64{},
		projects: map[string]store.YouTrackProject{
			"handoffkeep": {HKProject: "handoffkeep", ShortName: "HK"},
			"panewire":    {HKProject: "panewire", ShortName: "PW"},
			"other":       {HKProject: "other", ShortName: "OTHER"},
		},
		docs:        map[string]store.Document{},
		docArticles: map[string]store.YouTrackDocArticle{},
		nextTaskID:  100,
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

func (f *fakeStore) ListYouTrackProjects(_ context.Context) ([]store.YouTrackProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.YouTrackProject{}
	for _, p := range f.projects {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeStore) PutYouTrackProject(_ context.Context, p store.YouTrackProject) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects[p.HKProject] = p
	return nil
}

func (f *fakeStore) UpsertYouTrackDocArticle(_ context.Context, x store.YouTrackDocArticle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if x.DocumentKey == "" {
		x.DocumentKey = x.DocKey
	}
	if x.PartIndex == 0 {
		x.PartIndex = 1
	}
	if x.PartCount == 0 {
		x.PartCount = 1
	}
	f.docArticles[x.DocKey] = x
	return nil
}

func docSHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", sum)
}

func (f *fakeStore) putDoc(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[key] = store.Document{Key: key, Kind: "other", Body: body, SHA256: docSHA(body), CreatedBy: "test", CreatedAt: docTestTime, UpdatedAt: docTestTime}
}

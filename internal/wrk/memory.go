package wrk

import (
	"context"
	"sort"
	"sync"
)

// Memory is a store-free Backend with the same contract as the PostgreSQL
// journal: one global mutex stands in for the lane and job locks, and a
// unit's staged records become visible only when its function returns nil.
// It backs local contract tests and the in-process CLI test server; the
// production server always uses internal/store.
type Memory struct {
	mu       sync.Mutex
	journals map[string][]Record
	contents map[string]map[int64][]byte
	tasks    map[int64]string
	attempts []Attempt
}

// NewMemory returns an empty journal store.
func NewMemory() *Memory {
	return &Memory{journals: map[string][]Record{}, contents: map[string]map[int64][]byte{}, tasks: map[int64]string{}}
}

// AddTask registers a legacy task id that central claims may bind.
func (m *Memory) AddTask(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[id]; !ok {
		m.tasks[id] = ""
	}
}

// TaskBinding returns the job a task is bound to ("" when unbound).
func (m *Memory) TaskBinding(id int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

// Seed inserts already-formed records (and their content) verbatim, as a
// test fixture of prior history. It does not run the rules.
func (m *Memory) Seed(rec Record, content []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journals[rec.JobID] = append(m.journals[rec.JobID], rec)
	sort.Slice(m.journals[rec.JobID], func(i, j int) bool { return m.journals[rec.JobID][i].Seq < m.journals[rec.JobID][j].Seq })
	if content != nil {
		if m.contents[rec.JobID] == nil {
			m.contents[rec.JobID] = map[int64][]byte{}
		}
		m.contents[rec.JobID][rec.Seq] = content
	}
}

// Attempts returns a copy of the audit rows.
func (m *Memory) Attempts() []Attempt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Attempt(nil), m.attempts...)
}

// Records returns a copy of one job's committed journal.
func (m *Memory) Records(jobID string) []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Record(nil), m.journals[jobID]...)
}

// Content returns stored bytes for a record.
func (m *Memory) Content(jobID string, seq int64) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.contents[jobID][seq]
	return b, ok
}

type memTx struct {
	m       *Memory
	staged  []Record
	content map[string]map[int64][]byte
	tasks   map[int64]string
	write   bool
}

func (t *memTx) Journal(jobID string) (*Journal, error) {
	j := &Journal{JobID: jobID, Imports: map[int64][]byte{}}
	j.Records = append(j.Records, t.m.journals[jobID]...)
	for _, r := range t.staged {
		if r.JobID == jobID {
			j.Records = append(j.Records, r)
		}
	}
	for _, r := range j.Records {
		if r.Kind == "import" {
			if b, ok := t.m.contents[jobID][r.Seq]; ok {
				j.Imports[r.Seq] = b
			} else if b, ok := t.content[jobID][r.Seq]; ok {
				j.Imports[r.Seq] = b
			}
		}
	}
	return j, nil
}

func (t *memTx) CanonicalClaims() ([]Record, error) {
	latest := map[string]Record{}
	add := func(r Record) {
		if r.Kind == "claim" {
			if cur, ok := latest[r.JobID]; !ok || r.Seq > cur.Seq {
				latest[r.JobID] = r
			}
		}
	}
	for _, recs := range t.m.journals {
		for _, r := range recs {
			add(r)
		}
	}
	for _, r := range t.staged {
		add(r)
	}
	out := make([]Record, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out, nil
}

func (t *memTx) ContentExists(jobID string, seq int64) (bool, error) {
	if _, ok := t.m.contents[jobID][seq]; ok {
		return true, nil
	}
	_, ok := t.content[jobID][seq]
	return ok, nil
}

func (t *memTx) BindTask(taskID int64, jobID, release string, releaseTask int64) error {
	cur, ok := t.tasks[taskID]
	if !ok {
		return Fail(ClaimConflict)
	}
	if cur != "" && cur != jobID && cur != release {
		return Fail(ClaimConflict)
	}
	if releaseTask != 0 && releaseTask != taskID && t.tasks[releaseTask] == release {
		t.tasks[releaseTask] = ""
	}
	t.tasks[taskID] = jobID
	return nil
}

func (t *memTx) Append(rec Record, content []byte) error {
	t.staged = append(t.staged, rec)
	if content != nil {
		if t.content[rec.JobID] == nil {
			t.content[rec.JobID] = map[int64][]byte{}
		}
		t.content[rec.JobID][rec.Seq] = content
	}
	return nil
}

// Update implements Backend.
func (m *Memory) Update(ctx context.Context, lane bool, jobIDs []string, fn func(Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tasks := make(map[int64]string, len(m.tasks))
	for k, v := range m.tasks {
		tasks[k] = v
	}
	tx := &memTx{m: m, content: map[string]map[int64][]byte{}, tasks: tasks, write: true}
	if err := fn(tx); err != nil {
		return err
	}
	for _, r := range tx.staged {
		m.journals[r.JobID] = append(m.journals[r.JobID], r)
	}
	for job, bySeq := range tx.content {
		if m.contents[job] == nil {
			m.contents[job] = map[int64][]byte{}
		}
		for seq, b := range bySeq {
			m.contents[job][seq] = b
		}
	}
	m.tasks = tasks
	return nil
}

// View implements Backend.
func (m *Memory) View(ctx context.Context, fn func(Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fn(&memTx{m: m, content: map[string]map[int64][]byte{}, tasks: m.tasks})
}

// RecordAttempt implements Backend.
func (m *Memory) RecordAttempt(ctx context.Context, a Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts = append(m.attempts, a)
	return nil
}

func (t *memTx) Content(jobID string, seq int64) ([]byte, bool, error) {
	if b, ok := t.m.contents[jobID][seq]; ok {
		return b, true, nil
	}
	b, ok := t.content[jobID][seq]
	return b, ok, nil
}

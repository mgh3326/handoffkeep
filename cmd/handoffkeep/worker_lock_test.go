package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/hubforward"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

type observedWorkerLocker struct {
	*store.Store
	attempts chan bool
}

func (l observedWorkerLocker) TryWorkerLock(ctx context.Context, id store.WorkerLock) (*store.WorkerLease, bool, error) {
	lease, held, err := l.Store.TryWorkerLock(ctx, id)
	l.attempts <- held
	return lease, held, err
}

type countedWorker struct {
	started    chan int
	number     int
	active     *atomic.Int32
	duplicates *atomic.Int32
	effect     func(context.Context) error
	errors     chan error
}

func (w countedWorker) Run(ctx context.Context) {
	if w.active.Add(1) != 1 {
		w.duplicates.Add(1)
	}
	w.errors <- w.effect(ctx)
	w.started <- w.number
	<-ctx.Done()
	w.active.Add(-1)
}

func TestSingletonServeWorkers(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   store.WorkerLock
	}{
		{"hub forwarder", store.HubForwardWorkerLock},
		{"linear board", store.LinearBoardWorkerLock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, url := testdb.Schema(t, "singleton_worker")
			st, err := store.Open(t.Context(), url)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			var posts atomic.Int32
			var effect func(context.Context) error
			var check func()
			if tc.id == store.HubForwardWorkerLock {
				_, err := st.RecordDotsFollowup(t.Context(), store.DotsIssue{Identifier: "ZZZ-1", Title: "t", URL: "u", IssueUUID: "0a1b2c3d-0000-4000-8000-0000000000f1"}, "0a1b2c3d-0000")
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					posts.Add(1)
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"id":1}`)
				}))
				defer server.Close()
				effect = func(ctx context.Context) error {
					f := hubforward.Forwarder{Store: st, URL: server.URL, Token: "test"}
					_, failed, err := f.Once(ctx)
					if err != nil {
						return err
					}
					if failed != 0 {
						return fmt.Errorf("forward failed")
					}
					return nil
				}
				check = func() {
					if got := posts.Load(); got != 1 {
						t.Fatalf("posts=%d", got)
					}
				}
			} else {
				st.EnableLinearBoard()
				project := "experiment"
				task, err := st.CreateTask(t.Context(), store.Task{Lane: "singleton", Title: "board task", Kind: "implement", Priority: 2, CreatedBy: "test", Project: &project})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = st.ClaimTask(t.Context(), task.ID, "lead", "singleton-job", ""); err != nil {
					t.Fatal(err)
				}
				before, err := st.ListLinearOutbox(t.Context(), task.ID)
				if err != nil {
					t.Fatal(err)
				}
				effect = st.ReconcileLinearBoard
				check = func() {
					rows, err := st.ListLinearOutbox(t.Context(), task.ID)
					if err != nil || len(rows) != len(before)+1 {
						t.Fatalf("outbox=%d before=%d err=%v", len(rows), len(before), err)
					}
				}
			}
			locker := observedWorkerLocker{Store: st, attempts: make(chan bool, 100)}
			started := make(chan int, 2)
			errs := make(chan error, 2)
			var active, duplicates atomic.Int32
			ctx1, cancel1 := context.WithCancel(t.Context())
			defer cancel1()
			ctx2, cancel2 := context.WithCancel(t.Context())
			defer cancel2()
			done := make(chan struct{}, 2)
			for i, ctx := range []context.Context{ctx1, ctx2} {
				w := singletonWorker{store: locker, id: tc.id, interval: 10 * time.Millisecond, worker: countedWorker{started: started, number: i, active: &active, duplicates: &duplicates, effect: effect, errors: errs}}
				go func() { w.Run(ctx); done <- struct{}{} }()
			}
			var first int
			select {
			case first = <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("worker did not start")
			}
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			// Both workers have attempted the real Postgres lock while the winner runs.
			for acquired, refused := false, false; !acquired || !refused; {
				select {
				case held := <-locker.attempts:
					if held {
						acquired = true
					} else {
						refused = true
					}
				case <-time.After(5 * time.Second):
					t.Fatal("workers did not contend")
				}
			}
			if got := active.Load(); got != 1 || duplicates.Load() != 0 {
				t.Fatalf("active=%d duplicates=%d", got, duplicates.Load())
			}
			check()
			if first == 0 {
				cancel1()
			} else {
				cancel2()
			}
			select {
			case second := <-started:
				if second == first {
					t.Fatal("same worker took over")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("standby did not take over")
			}
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			cancel1()
			cancel2()
			<-done
			<-done
			if duplicates.Load() != 0 {
				t.Fatalf("duplicate concurrent effects: %d", duplicates.Load())
			}
		})
	}
}

type cancellationWorker struct{ started, stopped chan struct{} }

func (w cancellationWorker) Run(ctx context.Context) {
	close(w.started)
	<-ctx.Done()
	close(w.stopped)
}

func TestSingletonWorkerStopsOnLockSessionLoss(t *testing.T) {
	_, url := testdb.Schema(t, "worker_lock_loss")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, stopped, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	w := singletonWorker{store: st, id: store.LinearBoardWorkerLock, worker: cancellationWorker{started: started, stopped: stopped}}
	go func() { w.Run(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not start")
	}
	conn := testdb.Conn(t)
	var pid int
	err = conn.QueryRow(t.Context(), `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted AND classid=($1::bigint >> 32)::oid AND objid=($1::bigint & 4294967295)::oid`, int64(store.LinearBoardWorkerLock)).Scan(&pid)
	if err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := conn.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminated=%v err=%v", terminated, err)
	}
	select {
	case <-stopped:
	case <-time.After(6 * time.Second):
		t.Fatal("worker did not stop after lock loss")
	}
	cancel()
	<-done
}

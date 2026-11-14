package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

type fakeRequeuer struct {
	mu      sync.Mutex
	tenants []string
	limits  []int
	failed  bool
	called  chan struct{}
}

func (f *fakeRequeuer) RequeueYTMovedComments(_ context.Context, tenant string, limit int) ([]store.YTRequeued, error) {
	f.mu.Lock()
	f.tenants = append(f.tenants, tenant)
	f.limits = append(f.limits, limit)
	f.mu.Unlock()
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
	if f.failed {
		return nil, errors.New("boom")
	}
	return []store.YTRequeued{{CommentID: 1, From: 2}}, nil
}

// hk 4569: the requeue pass runs for the worker's tenant with the pass limit;
// a failed pass is logged and the worker goes on; a worker without one does
// nothing.
func TestYTWorkerRequeuePass(t *testing.T) {
	f := &fakeRequeuer{}
	w := ytCommentsWorker{requeue: f, tenant: "tenant.example"}
	w.requeueMoved(t.Context())
	f.failed = true
	w.requeueMoved(t.Context())
	if len(f.tenants) != 2 || f.tenants[0] != "tenant.example" || f.limits[0] != ytCommentsRequeueLimit {
		t.Fatalf("passes tenants=%v limits=%v", f.tenants, f.limits)
	}
	ytCommentsWorker{}.requeueMoved(t.Context())
}

// The worker built in mode on runs the requeue pass in its loop; one built in
// mode canary has none (only mode on queues comments behind a create).
func TestYTCommentsWorkerRunsTheRequeue(t *testing.T) {
	_, dbURL := testdb.Schema(t, "ytrq_worker")
	ctx := t.Context()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	const tenant = "requeue.youtrack.cloud"
	remote := ytwritetest.NewFake()
	canary, err := newYTCommentsWorker(st, remote, tenant, store.YTCommentsCanary, false, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if canary.requeue != nil {
		t.Fatalf("mode canary worker has a requeue pass: %T", canary.requeue)
	}
	w, err := newYTCommentsWorker(st, remote, tenant, store.YTCommentsOn, false, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := w.requeue.(*store.Store); !ok {
		t.Fatalf("mode on worker's requeue pass is %T, want the store", w.requeue)
	}
	f := &fakeRequeuer{called: make(chan struct{}, 1)}
	w.requeue = f
	w.interval = 10 * time.Millisecond
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(runCtx)
	}()
	select {
	case <-f.called:
	case <-time.After(10 * time.Second):
		t.Error("the running worker never ran the requeue pass")
	}
	stop()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tenants) == 0 || f.tenants[0] != tenant {
		t.Fatalf("requeue tenants %v, want %q", f.tenants, tenant)
	}
}

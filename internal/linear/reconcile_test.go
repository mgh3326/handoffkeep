package linear

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestLinearReconcileWritesIdempotentReport(t *testing.T) {
	st, _ := isolatedLinearStore(t)
	fake := newFakeLinear(t)
	fake.issueExists = true
	task := createDrainTask(t, st, store.TaskRefs{Linear: &store.TaskLinear{
		Sync: true, Tier: "T3", Grade: "S+", Labels: []string{"connector"},
	}})
	fixedNow := time.Date(2026, 9, 9, 6, 0, 0, 0, time.UTC)
	reconciler := &Reconciler{
		Client: fake.client(t),
		ListTasks: func(ctx context.Context) ([]store.Task, error) {
			return st.ListTasks(ctx, "", "", "", 1000)
		},
		OutboxStatus: st.GetLinearOutboxStatus,
		WriteDocument: func(ctx context.Context, document store.Document) (store.Document, bool, error) {
			document.CreatedBy = "reconcile-test"
			return st.PutDocument(ctx, document)
		},
		Now: func() time.Time { return fixedNow },
	}
	first, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.Key != "report/linear/reconcile/2026-09-09" || !strings.Contains(first.Body, "Drifts:") {
		t.Fatalf("first report=%+v", first)
	}
	firstDocument, found, err := st.GetDocument(t.Context(), first.Key)
	if err != nil || !found {
		t.Fatalf("first document found=%t err=%v", found, err)
	}
	second, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	secondDocument, found, err := st.GetDocument(t.Context(), second.Key)
	if err != nil || !found {
		t.Fatalf("second document found=%t err=%v", found, err)
	}
	if second.Changed || first.Body != second.Body || firstDocument.SHA256 != secondDocument.SHA256 {
		t.Fatalf("idempotency first_changed=%t second_changed=%t sha=%s/%s", first.Changed, second.Changed, firstDocument.SHA256, secondDocument.SHA256)
	}
	if fake.count("HKIssueSearch") != 2 || fake.count("HKIssueCreate") != 0 || fake.count("HKIssueUpdate") != 0 || fake.count("HKCommentCreate") != 0 || fake.count("HKIssueArchive") != 0 {
		t.Fatalf("reconcile operation counts=%v", fake.counts)
	}
	if task.ID != 1 {
		t.Fatalf("isolated fixture task id=%d", task.ID)
	}
}

func TestLinearReconcileWorkerUsesInjectableThirtyMinuteInterval(t *testing.T) {
	if ReconcileInterval != 30*time.Minute {
		t.Fatalf("ReconcileInterval=%s", ReconcileInterval)
	}
	fake := newFakeLinear(t)
	fake.issueExists = true
	reconciler := &Reconciler{
		Client: fake.client(t),
		ListTasks: func(context.Context) ([]store.Task, error) {
			return []store.Task{{
				ID: 1, Lane: "builder-lane", Title: "Mirror connector contract", State: "backlog",
				Refs: store.TaskRefs{Linear: &store.TaskLinear{Sync: true}},
			}}, nil
		},
		DryRun:   true,
		Interval: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconciler.Run(ctx)
	}()
	deadline := time.Now().Add(time.Second)
	for fake.count("HKIssueSearch") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconcile worker did not stop")
	}
	if fake.count("HKIssueSearch") < 2 {
		t.Fatalf("reconcile calls=%d", fake.count("HKIssueSearch"))
	}
}

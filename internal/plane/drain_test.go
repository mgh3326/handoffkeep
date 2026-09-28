package plane

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func isolatedPlaneStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	baseURL := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if baseURL == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for Plane drain tests")
	}
	admin, err := pgx.Connect(t.Context(), baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	schema := fmt.Sprintf("plane_drain_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	scopedURL := parsed.String()
	st, err := store.Open(t.Context(), scopedURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st, scopedURL
}

func testMapping() ProjectMapping {
	return ProjectMapping{Map: map[string]string{"experiment": "EXP"}, Default: "HK"}
}

func createPlaneTask(t *testing.T, st *store.Store) store.Task {
	t.Helper()
	project := "experiment"
	task, err := st.CreateTask(t.Context(), store.Task{
		Lane:      "builder-lane",
		Title:     "Mirror connector contract",
		Kind:      "implement",
		Priority:  2,
		CreatedBy: "fixture-client",
		Project:   &project,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func startTestDrain(t *testing.T, drain *Drain, retry time.Duration) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	drain.PollInterval = 5 * time.Millisecond
	drain.Jitter = func(time.Duration) time.Duration { return retry }
	go func() {
		defer close(done)
		drain.Run(ctx)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("drain did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func waitOutbox(t *testing.T, st *store.Store, taskID int64, predicate func([]store.PlaneOutbox) bool) []store.PlaneOutbox {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := st.ListPlaneOutbox(t.Context(), taskID)
		if err != nil {
			t.Fatal(err)
		}
		if predicate(rows) {
			return rows
		}
		time.Sleep(5 * time.Millisecond)
	}
	rows, err := st.ListPlaneOutbox(t.Context(), taskID)
	t.Fatalf("outbox condition timed out: rows=%+v err=%v", rows, err)
	return nil
}

func linkPlaneIssue(t *testing.T, scopedURL string, taskID int64, workItemID, projectID string) {
	t.Helper()
	connection, err := pgx.Connect(t.Context(), scopedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(t.Context(), `INSERT INTO plane_issues(task_id,work_item_id,external_id,project_id,created_at,updated_at) VALUES($1,$2,$3,$4,now(),now())`, taskID, workItemID, store.PlaneExternalID(taskID), projectID); err != nil {
		t.Fatal(err)
	}
}

// TestPlaneDryRunNeverWrites is the pilot's central guarantee: with DryRun
// set the drain walks the whole outbox — create, update and remove ops —
// marks every row 'dryrun' and issues zero remote calls, even with a fully
// configured client present.
func TestPlaneDryRunNeverWrites(t *testing.T) {
	st, _ := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	task := createPlaneTask(t, st)
	if _, err := st.ClaimTask(t.Context(), task.ID, "builder", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(t.Context(), task.ID, "in_progress", "builder", "started", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(t.Context(), task.ID, "dropped", "builder", "abandoned", nil, ""); err != nil {
		t.Fatal(err)
	}
	stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping(), DryRun: true}, 5*time.Millisecond)
	rows := waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		for _, row := range rows {
			if row.State != "dryrun" {
				return false
			}
		}
		return len(rows) == 4
	})
	stop()
	for _, operation := range []string{"issue_create", "issue_update", "issue_delete", "issue_list", "issue_get", "project_list", "state_list", "label_list"} {
		if fake.count(operation) != 0 {
			t.Fatalf("dry-run made %d %s calls", fake.count(operation), operation)
		}
	}
	ops := []string{rows[0].Op, rows[1].Op, rows[2].Op, rows[3].Op}
	if ops[0] != store.PlaneOpWorkItemCreate || ops[3] != store.PlaneOpWorkItemRemove {
		t.Fatalf("op order=%v", ops)
	}
	if fake.issueCount() != 0 {
		t.Fatalf("dry-run created remote items: %d", fake.issueCount())
	}
	if _, found, err := st.GetPlaneIssue(t.Context(), task.ID); err != nil || found {
		t.Fatalf("dry-run linked a remote item: found=%t err=%v", found, err)
	}
}

// TestPlaneCreateAdoptsMarkerAfterLostResponse drops the create response
// after the remote accepted it; the restarted drain must find the marker and
// adopt the work item instead of creating a duplicate.
func TestPlaneCreateAdoptsMarkerAfterLostResponse(t *testing.T) {
	st, _ := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	fake.failures["issue_create"] = []string{"drop"}
	task := createPlaneTask(t, st)
	stopFirst := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 400*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		return len(rows) == 1 && rows[0].State == "pending" && rows[0].Attempts == 1
	})
	stopFirst()
	stopSecond := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		return len(rows) == 1 && (rows[0].State == "sent" || rows[0].State == "skipped")
	})
	stopSecond()
	if fake.count("issue_create") != 1 || fake.issueCount() != 1 {
		t.Fatalf("issue_create calls=%d remote items=%d", fake.count("issue_create"), fake.issueCount())
	}
	if _, found, err := st.GetPlaneIssue(t.Context(), task.ID); err != nil || !found {
		t.Fatalf("adopted issue found=%t err=%v", found, err)
	}
}

// TestPlaneUpdateIsHkWins writes a divergent remote work item, then proves
// the drain overwrites every projected field with the hk snapshot.
func TestPlaneUpdateIsHkWins(t *testing.T) {
	st, scopedURL := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	task := createPlaneTask(t, st)
	if _, err := st.ClaimTask(t.Context(), task.ID, "builder", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(t.Context(), task.ID, "in_progress", "builder", "started", nil, ""); err != nil {
		t.Fatal(err)
	}
	linkPlaneIssue(t, scopedURL, task.ID, "wi-seed", "proj-exp")
	fake.seed(Issue{
		ID:              "wi-seed",
		Name:            "operator-renamed title",
		DescriptionHTML: "<p>Mirrored read-only from hk; hk is the source of truth. Reference: " + store.PlaneExternalID(task.ID) + "</p>",
		State:           "st-done",
		Priority:        "urgent",
		Labels:          []string{"lb-unrelated"},
		Project:         "proj-exp",
	})
	stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		for _, row := range rows {
			if row.State == "pending" {
				return false
			}
		}
		return len(rows) == 3
	})
	stop()
	remote := fake.get("wi-seed")
	if remote == nil {
		t.Fatal("remote issue vanished")
	}
	if remote.Name != "Mirror connector contract" || remote.State != "st-progress" || remote.Priority != "medium" {
		t.Fatalf("hk-wins update did not overwrite: %+v", remote)
	}
}

// TestPlaneTerminalRemoveDeletesMirror runs a task to merged and proves the
// remove op deletes the remote work item and drops the link row.
func TestPlaneTerminalRemoveDeletesMirror(t *testing.T) {
	st, _ := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	task := createPlaneTask(t, st)
	stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		return len(rows) == 1 && rows[0].State == "sent"
	})
	if fake.issueCount() != 1 {
		t.Fatalf("remote items=%d", fake.issueCount())
	}
	if _, err := st.ClaimTask(t.Context(), task.ID, "builder", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(t.Context(), task.ID, "in_progress", "builder", "started", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(t.Context(), task.ID, "dropped", "builder", "abandoned", nil, ""); err != nil {
		t.Fatal(err)
	}
	rows := waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		return len(rows) == 4 && rows[3].Op == store.PlaneOpWorkItemRemove && rows[3].State == "sent"
	})
	stop()
	_ = rows
	if fake.count("issue_delete") != 1 || fake.issueCount() != 0 {
		t.Fatalf("delete calls=%d remote items=%d", fake.count("issue_delete"), fake.issueCount())
	}
	if _, found, err := st.GetPlaneIssue(t.Context(), task.ID); err != nil || found {
		t.Fatalf("link row survived remove: found=%t err=%v", found, err)
	}
}

// TestPlaneDrainAdvisoryLockSingleOwner runs two drains against one store and
// proves the advisory lock admits exactly one writer, then releases.
func TestPlaneDrainAdvisoryLockSingleOwner(t *testing.T) {
	st, scopedURL := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	stopFirst := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
	stopSecond := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
	connection, err := pgx.Connect(t.Context(), scopedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for {
		var holders int
		if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND granted AND classid=0 AND objid=$1`, store.PlaneDrainAdvisoryLock).Scan(&holders); err != nil {
			t.Fatal(err)
		}
		if holders == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("advisory lock holders=%d", holders)
		}
		time.Sleep(5 * time.Millisecond)
	}
	task := createPlaneTask(t, st)
	waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		return len(rows) == 1 && rows[0].State == "sent"
	})
	if fake.count("issue_create") != 1 {
		t.Fatalf("issue_create calls=%d", fake.count("issue_create"))
	}
	stopFirst()
	stopSecond()
	var acquired bool
	deadline = time.Now().Add(5 * time.Second)
	for {
		if err := connection.QueryRow(t.Context(), `SELECT pg_try_advisory_lock($1)`, store.PlaneDrainAdvisoryLock).Scan(&acquired); err != nil {
			t.Fatal(err)
		}
		if acquired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reacquire=%t after stopping both drains", acquired)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := connection.Exec(t.Context(), `SELECT pg_advisory_unlock($1)`, store.PlaneDrainAdvisoryLock); err != nil {
		t.Fatal(err)
	}
}

// TestPlaneProjectMoveReplacesItem mirrors a task whose hk project was
// remapped: the work item is recreated under the mapped project and the
// stale one deleted — hk wins even across Plane's immutable parent.
func TestPlaneProjectMoveReplacesItem(t *testing.T) {
	st, scopedURL := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	task := createPlaneTask(t, st)
	linkPlaneIssue(t, scopedURL, task.ID, "wi-old", "proj-hk")
	fake.seed(Issue{
		ID:              "wi-old",
		Name:            "Mirror connector contract",
		DescriptionHTML: "<p>Mirrored read-only from hk; hk is the source of truth. Reference: " + store.PlaneExternalID(task.ID) + "</p>",
		State:           "st-backlog",
		Priority:        "medium",
		Project:         "proj-hk",
	})
	stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		return len(rows) == 1 && rows[0].State == "sent"
	})
	stop()
	if fake.get("wi-old") != nil {
		t.Fatal("stale work item survived the project move")
	}
	link, found, err := st.GetPlaneIssue(t.Context(), task.ID)
	if err != nil || !found || link.ProjectID != "proj-exp" || link.WorkItemID == "wi-old" {
		t.Fatalf("link after move=%+v found=%t err=%v", link, found, err)
	}
	if fake.count("issue_delete") != 1 || fake.count("issue_create") != 1 {
		t.Fatalf("move calls create=%d delete=%d", fake.count("issue_create"), fake.count("issue_delete"))
	}
}

// TestPlaneMoveDeleteFailureLeavesNoDuplicate proves a move that fails on
// the stale-item delete does not stack a duplicate on every retry: the
// delete happens before the replacement create, so a delete failure changes
// nothing remotely and the retry converges to exactly one item.
func TestPlaneMoveDeleteFailureLeavesNoDuplicate(t *testing.T) {
	st, scopedURL := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	fake.failures["issue_delete"] = []string{"500"}
	task := createPlaneTask(t, st)
	linkPlaneIssue(t, scopedURL, task.ID, "wi-old", "proj-hk")
	fake.seed(Issue{
		ID:              "wi-old",
		Name:            "Mirror connector contract",
		DescriptionHTML: "<p>Reference: " + store.PlaneExternalID(task.ID) + "</p>",
		State:           "st-backlog",
		Project:         "proj-hk",
	})
	stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		return len(rows) == 1 && rows[0].State == "sent"
	})
	stop()
	if fake.issueCount() != 1 {
		t.Fatalf("issue count after move retry=%d want 1", fake.issueCount())
	}
	link, found, err := st.GetPlaneIssue(t.Context(), task.ID)
	if err != nil || !found || link.ProjectID != "proj-exp" || link.WorkItemID == "wi-old" {
		t.Fatalf("link after move retry=%+v found=%t err=%v", link, found, err)
	}
	if fake.count("issue_create") != 1 {
		t.Fatalf("create calls=%d — a retried move must not stack duplicates", fake.count("issue_create"))
	}
}

// TestPlaneMoveCreateFailureConverges covers the other half-move: the stale
// item is deleted but the replacement create fails. The retry must heal
// through the missing-remote path (marker scan, then create) into exactly
// one item in the mapped project.
func TestPlaneMoveCreateFailureConverges(t *testing.T) {
	st, scopedURL := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	fake.failures["issue_create"] = []string{"500"}
	task := createPlaneTask(t, st)
	linkPlaneIssue(t, scopedURL, task.ID, "wi-old", "proj-hk")
	fake.seed(Issue{
		ID:              "wi-old",
		Name:            "Mirror connector contract",
		DescriptionHTML: "<p>Reference: " + store.PlaneExternalID(task.ID) + "</p>",
		State:           "st-backlog",
		Project:         "proj-hk",
	})
	stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		return len(rows) == 1 && rows[0].State == "sent"
	})
	stop()
	if fake.issueCount() != 1 || fake.get("wi-old") != nil {
		t.Fatalf("after half-move retry: items=%d stale_present=%t", fake.issueCount(), fake.get("wi-old") != nil)
	}
	link, found, err := st.GetPlaneIssue(t.Context(), task.ID)
	if err != nil || !found || link.ProjectID != "proj-exp" || link.WorkItemID == "wi-old" {
		t.Fatalf("link after half-move=%+v found=%t err=%v", link, found, err)
	}
}

// TestPlaneMoveDroppedCreateResponseAdoptsOnRetry is the lost-response
// variant of a project move: the stale item is deleted, the replacement
// create lands remotely but its response is dropped. The retry must find
// that item by marker and adopt it — a second create is a BLOCKER-class
// duplicate.
func TestPlaneMoveDroppedCreateResponseAdoptsOnRetry(t *testing.T) {
	st, scopedURL := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	fake.failures["issue_create"] = []string{"drop"}
	task := createPlaneTask(t, st)
	linkPlaneIssue(t, scopedURL, task.ID, "wi-old", "proj-hk")
	fake.seed(Issue{
		ID:              "wi-old",
		Name:            "Mirror connector contract",
		DescriptionHTML: "<p>Reference: " + store.PlaneExternalID(task.ID) + "</p>",
		State:           "st-backlog",
		Project:         "proj-hk",
	})
	stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		// "skipped" is the converged outcome: the retry adopts the landed
		// item by marker, finds every projected field already correct, and
		// patches nothing.
		return len(rows) == 1 && (rows[0].State == "sent" || rows[0].State == "skipped")
	})
	stop()
	if fake.issueCount() != 1 {
		t.Fatalf("dropped create must adopt on retry, not stack: items=%d", fake.issueCount())
	}
	if fake.count("issue_create") != 1 {
		t.Fatalf("create calls=%d want 1 — retry must adopt the dropped item", fake.count("issue_create"))
	}
	link, found, err := st.GetPlaneIssue(t.Context(), task.ID)
	if err != nil || !found || link.ProjectID != "proj-exp" || link.WorkItemID == "wi-old" {
		t.Fatalf("link after dropped-create retry=%+v found=%t err=%v", link, found, err)
	}
}

// TestPlaneMarkerDoesNotAdoptDigitPrefixItem proves the adoption scan cannot
// confuse hk:task/<N> with a remote item carrying hk:task/<N><digit> — the
// substring collision CodeRabbit flagged. The foreign item must be left
// alone and a fresh item must be created for the task.
func TestPlaneMarkerDoesNotAdoptDigitPrefixItem(t *testing.T) {
	st, _ := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	task := createPlaneTask(t, st)
	foreign := fake.seed(Issue{
		ID:              "wi-foreign",
		Name:            "some other task's mirror",
		DescriptionHTML: "<p>Reference: " + store.PlaneExternalID(task.ID) + "9</p>",
		State:           "st-done",
		Project:         "proj-exp",
	})
	stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		return len(rows) == 1 && rows[0].State == "sent"
	})
	stop()
	if fake.issueCount() != 2 {
		t.Fatalf("issue count=%d want 2 (foreign kept + own item)", fake.issueCount())
	}
	if current := fake.get(foreign.ID); current == nil || current.Name != "some other task's mirror" {
		t.Fatalf("foreign item was adopted/rewritten: %+v", current)
	}
	link, found, err := st.GetPlaneIssue(t.Context(), task.ID)
	if err != nil || !found || link.WorkItemID == foreign.ID {
		t.Fatalf("link adopted foreign item: link=%+v found=%t err=%v", link, found, err)
	}
}

func TestPlaneFailureClassification(t *testing.T) {
	t.Run("permanent", func(t *testing.T) {
		st, _ := isolatedPlaneStore(t)
		st.EnablePlaneSync()
		fake := newFakePlane(t)
		fake.always["project_list"] = "401"
		task := createPlaneTask(t, st)
		stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 5*time.Millisecond)
		rows := waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
			return len(rows) == 1 && rows[0].State == "failed"
		})
		stop()
		if rows[0].Attempts != 1 || rows[0].LastError == "" {
			t.Fatalf("permanent row=%+v", rows[0])
		}
	})
	t.Run("transient", func(t *testing.T) {
		st, _ := isolatedPlaneStore(t)
		st.EnablePlaneSync()
		fake := newFakePlane(t)
		fake.always["project_list"] = "503"
		task := createPlaneTask(t, st)
		stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: testMapping()}, 20*time.Millisecond)
		rows := waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
			return len(rows) == 1 && rows[0].State == "pending" && rows[0].Attempts >= 2
		})
		stop()
		if rows[0].LastError == "" {
			t.Fatalf("transient row=%+v", rows[0])
		}
	})
}

// TestPlaneUnmappedProjectFailsClosed proves a task in an hk project with no
// mapping and no default never invents a remote home — it fails the outbox
// row permanently instead.
func TestPlaneUnmappedProjectFailsClosed(t *testing.T) {
	st, _ := isolatedPlaneStore(t)
	st.EnablePlaneSync()
	fake := newFakePlane(t)
	task := createPlaneTask(t, st)
	mapping := ProjectMapping{Map: map[string]string{}, Default: ""}
	stop := startTestDrain(t, &Drain{Store: st, Client: fake.client(t), Projects: mapping}, 5*time.Millisecond)
	rows := waitOutbox(t, st, task.ID, func(rows []store.PlaneOutbox) bool {
		return len(rows) == 1 && rows[0].State == "failed"
	})
	stop()
	if !strings.Contains(rows[0].LastError, "project") {
		t.Fatalf("unmapped error=%q", rows[0].LastError)
	}
	if fake.issueCount() != 0 {
		t.Fatalf("unmapped project created %d items", fake.issueCount())
	}
}

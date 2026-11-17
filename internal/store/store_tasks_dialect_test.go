package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// Dialect-neutral behaviour of the tasks core: task create/read/list, claim and
// rebind, relane, project and area, project vocabulary and metadata, milestones
// and the duplicate check. Each case gets a fresh database, so names need not
// be unique across cases. Only public store methods are used.

func init() {
	registerTaskAreaCase("core/project-vocabulary", caseCoreProjectVocabulary)
	registerTaskAreaCase("core/project-metadata", caseCoreProjectMetadata)
	registerTaskAreaCase("core/create-and-get", caseCoreCreateAndGet)
	registerTaskAreaCase("core/create-refusals", caseCoreCreateRefusals)
	registerTaskAreaCase("core/create-concurrent", caseCoreCreateConcurrent)
	registerTaskAreaCase("core/duplicate-check", caseCoreDuplicateCheck)
	registerTaskAreaCase("core/claim-and-rebind", caseCoreClaimAndRebind)
	registerTaskAreaCase("core/relane", caseCoreRelane)
	registerTaskAreaCase("core/set-project-and-area", caseCoreSetProjectAndArea)
	registerTaskAreaCase("core/list-filters", caseCoreListFilters)
	registerTaskAreaCase("core/glance", caseCoreGlance)
	registerTaskAreaCase("core/milestones", caseCoreMilestones)
}

func coreProject(t *testing.T, s *Store, name string) *string {
	t.Helper()
	if _, err := s.AddTaskProject(t.Context(), name, "tester"); err != nil {
		t.Fatal(err)
	}
	return &name
}

func coreTask(t *testing.T, s *Store, project *string, lane, title string) Task {
	t.Helper()
	x, err := s.CreateTask(t.Context(), Task{Lane: lane, Title: title, Kind: "implement", CreatedBy: "tester", Project: project})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return x
}

func caseCoreProjectVocabulary(t *testing.T, s *Store) {
	ctx := t.Context()
	created, err := s.AddTaskProject(ctx, "core-alpha", "tester")
	if err != nil || !created {
		t.Fatalf("first add = %v, %v; want created", created, err)
	}
	created, err = s.AddTaskProject(ctx, "core-alpha", "tester")
	if err != nil || created {
		t.Fatalf("second add = %v, %v; want not created and no error", created, err)
	}
	if _, err := s.AddTaskProject(ctx, "core-beta", "tester"); err != nil {
		t.Fatal(err)
	}
	names, err := s.ListTaskProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh schema seeds the fleet's projects on both dialects, so the two
	// added names are checked as members, once each.
	count := map[string]int{}
	for _, n := range names {
		count[n]++
	}
	if count["core-alpha"] != 1 || count["core-beta"] != 1 || count["handoffkeep"] != 1 {
		t.Fatalf("projects = %v", names)
	}
	for _, bad := range []string{"", "has space", "bad/slash"} {
		if _, err := s.AddTaskProject(ctx, bad, "tester"); err == nil {
			t.Fatalf("AddTaskProject(%q) succeeded", bad)
		}
	}
	if _, err := s.AddTaskProject(ctx, "core-gamma", ""); err == nil {
		t.Fatal("AddTaskProject without a creator succeeded")
	}

	// Concurrent adds of one name: exactly one creates it.
	var wg sync.WaitGroup
	var mu sync.Mutex
	created8 := 0
	errs := []error{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.AddTaskProject(ctx, "core-race", "tester")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else if ok {
				created8++
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 || created8 != 1 {
		t.Fatalf("concurrent adds created %d, errors %v; want exactly one create", created8, errs)
	}
}

func caseCoreProjectMetadata(t *testing.T, s *Store) {
	ctx := t.Context()
	coreProject(t, s, "core-meta")
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	target := start.AddDate(0, 1, 0)
	if _, err := s.SetProjectMetadata(ctx, ProjectMetadata{Name: "core-meta", Status: "active", StartAt: &start, TargetAt: &target}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProjectMetadata(ctx, "core-meta")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || got.StartAt == nil || !got.StartAt.Equal(start) || got.TargetAt == nil || !got.TargetAt.Equal(target) {
		t.Fatalf("metadata = %+v", got)
	}
	// A patch changes only what it names; SetTarget with nil clears the date.
	status := "paused"
	patched, err := s.PatchProjectMetadata(ctx, "core-meta", ProjectMetadataPatch{Status: &status, SetTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	if patched.Status != "paused" || patched.StartAt == nil || !patched.StartAt.Equal(start) || patched.TargetAt != nil {
		t.Fatalf("patched = %+v", patched)
	}
	got, err = s.GetProjectMetadata(ctx, "core-meta")
	if err != nil || got.TargetAt != nil || got.Status != "paused" {
		t.Fatalf("stored after patch = %+v, %v", got, err)
	}
	// A target before the start is refused and stores nothing.
	early := start.Add(-time.Hour)
	if _, err := s.PatchProjectMetadata(ctx, "core-meta", ProjectMetadataPatch{SetTarget: true, TargetAt: &early}); err == nil {
		t.Fatal("target before start was accepted")
	}
	got, _ = s.GetProjectMetadata(ctx, "core-meta")
	if got.TargetAt != nil {
		t.Fatalf("refused patch stored a target: %+v", got)
	}
	// Unknown projects are not found on every path.
	if _, err := s.GetProjectMetadata(ctx, "core-missing"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("get missing = %v", err)
	}
	if _, err := s.SetProjectMetadata(ctx, ProjectMetadata{Name: "core-missing", Status: "x"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("set missing = %v", err)
	}
	if _, err := s.PatchProjectMetadata(ctx, "core-missing", ProjectMetadataPatch{Status: &status}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("patch missing = %v", err)
	}
}

func caseCoreCreateAndGet(t *testing.T, s *Store) {
	ctx := t.Context()
	project := coreProject(t, s, "core-create")
	expected := "2d"
	area := "docs"
	parent := coreTask(t, s, project, "core-lane", "parent task for the create case")
	x, err := s.CreateTask(ctx, Task{
		Lane: "core-lane", ParentLane: "core-parent", Title: "write the create case", Kind: "fix", CreatedBy: "tester",
		Project: project, Area: &area, Expected: &expected, Priority: 3, ParentID: &parent.ID,
		Refs: TaskRefs{JobID: "should-be-kept"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if x.ID <= parent.ID || x.State != StateBacklog || x.ClaimedBy != "" || x.CreatedBy != "tester" {
		t.Fatalf("created = %+v", x)
	}
	if x.Expected == nil || *x.Expected != "48h0m0s" {
		t.Fatalf("expected = %v, want the normalised duration", x.Expected)
	}
	if x.CreatedAt.IsZero() || !x.CreatedAt.Equal(x.UpdatedAt) || !x.CreatedAt.Equal(x.LastActivityAt) {
		t.Fatalf("timestamps = %v %v %v", x.CreatedAt, x.UpdatedAt, x.LastActivityAt)
	}
	got, found, err := s.GetTask(ctx, x.ID)
	if err != nil || !found {
		t.Fatalf("get = %v, %v", found, err)
	}
	if got.Title != x.Title || got.Lane != "core-lane" || got.ParentLane != "core-parent" || got.Kind != "fix" || got.Priority != 3 {
		t.Fatalf("stored = %+v", got)
	}
	if got.Project == nil || *got.Project != "core-create" || got.Area == nil || *got.Area != "docs" {
		t.Fatalf("project/area = %v / %v", got.Project, got.Area)
	}
	if got.ParentID == nil || *got.ParentID != parent.ID || got.Refs.JobID != "should-be-kept" {
		t.Fatalf("parent/refs = %v / %+v", got.ParentID, got.Refs)
	}
	if !got.CreatedAt.Equal(x.CreatedAt) {
		t.Fatalf("created_at read back %v, returned %v", got.CreatedAt, x.CreatedAt)
	}
	if _, found, err := s.GetTask(ctx, x.ID+1000); err != nil || found {
		t.Fatalf("get missing = %v, %v", found, err)
	}
}

func caseCoreCreateRefusals(t *testing.T, s *Store) {
	ctx := t.Context()
	project := coreProject(t, s, "core-refuse")
	base := Task{Lane: "core-lane", Title: "refused", Kind: "implement", CreatedBy: "tester", Project: project}
	mutate := func(f func(*Task)) error {
		x := base
		f(&x)
		_, err := s.CreateTask(ctx, x)
		return err
	}
	if err := mutate(func(x *Task) { x.Project = nil }); !errors.Is(err, ErrTaskProjectRequired) {
		t.Fatalf("no project = %v", err)
	}
	empty := ""
	if err := mutate(func(x *Task) { x.Project = &empty }); !errors.Is(err, ErrTaskProjectRequired) {
		t.Fatalf("empty project = %v", err)
	}
	unknown := "core-unknown"
	if err := mutate(func(x *Task) { x.Project = &unknown }); !errors.Is(err, ErrTaskProjectUnknown) {
		t.Fatalf("unknown project = %v", err)
	}
	if err := mutate(func(x *Task) { x.Kind = "nonsense" }); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if err := mutate(func(x *Task) { x.Title = "" }); err == nil {
		t.Fatal("empty title accepted")
	}
	badArea := "nonsense"
	if err := mutate(func(x *Task) { x.Area = &badArea }); !errors.Is(err, ErrTaskAreaUnknown) {
		t.Fatalf("unknown area = %v", err)
	}
	badExpected := "10s"
	if err := mutate(func(x *Task) { x.Expected = &badExpected }); err == nil {
		t.Fatal("too short expected accepted")
	}
	missingParent := int64(987654)
	if err := mutate(func(x *Task) { x.ParentID = &missingParent }); !errors.Is(err, ErrTaskParentInvalid) {
		t.Fatalf("missing parent = %v", err)
	}
	if err := mutate(func(x *Task) { x.Source = "intake" }); err == nil {
		t.Fatal("source accepted on a plain create")
	}
	// Refused creates leave nothing behind.
	rows, err := s.ListTasks(ctx, "", "", "", nil, nil, 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("tasks after refusals = %d, %v", len(rows), err)
	}
}

func caseCoreCreateConcurrent(t *testing.T, s *Store) {
	ctx := t.Context()
	project := coreProject(t, s, "core-race")
	const n = 8
	ids := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x, err := s.CreateTask(ctx, Task{Lane: "core-lane", Title: "concurrent create " + string(rune('a'+i)), Kind: "implement", CreatedBy: "tester", Project: project})
			ids[i], errs[i] = x.ID, err
		}()
	}
	wg.Wait()
	seen := map[int64]bool{}
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("create %d: %v", i, errs[i])
		}
		if ids[i] < 1 || seen[ids[i]] {
			t.Fatalf("ids = %v", ids)
		}
		seen[ids[i]] = true
	}
	rows, err := s.ListTasks(ctx, "core-lane", "", "", nil, nil, 100)
	if err != nil || len(rows) != n {
		t.Fatalf("listed %d tasks, %v; want %d", len(rows), err, n)
	}
}

func caseCoreDuplicateCheck(t *testing.T, s *Store) {
	ctx := t.Context()
	project := coreProject(t, s, "core-dup")
	first := coreTask(t, s, project, "core-lane", "Fix the flaky payment webhook retry")
	task := Task{Lane: "core-lane", Title: "Fix the flaky payment webhook retries", Kind: "fix", CreatedBy: "tester", Project: project}

	_, err := s.CreateTaskChecked(ctx, task, DuplicateCheck{})
	var similar *SimilarTasksError
	if !errors.As(err, &similar) || len(similar.Tasks) != 1 || similar.Tasks[0].ID != first.ID {
		t.Fatalf("similar create = %v", err)
	}
	if got := similar.Tasks[0]; got.Title != first.Title || got.State != StateBacklog || got.Lane != "core-lane" || got.Similarity < SimilarTaskThreshold || got.Similarity > 1 {
		t.Fatalf("similar task = %+v", got)
	}

	// An unrelated title passes the check.
	other := task
	other.Title = "Rotate the staging database credentials"
	if _, err := s.CreateTaskChecked(ctx, other, DuplicateCheck{}); err != nil {
		t.Fatalf("unrelated create = %v", err)
	}
	// New files it without looking; Link files it and relates it to the other.
	filed, err := s.CreateTaskChecked(ctx, task, DuplicateCheck{New: true})
	if err != nil {
		t.Fatalf("new create = %v", err)
	}
	linked := task
	linked.Title = "Fix the flaky payment webhook retrying"
	made, err := s.CreateTaskChecked(ctx, linked, DuplicateCheck{Link: first.ID})
	if err != nil {
		t.Fatalf("link create = %v", err)
	}
	got, found, err := s.GetTask(ctx, made.ID)
	if err != nil || !found || len(got.Relations) != 1 {
		t.Fatalf("linked task = %+v, %v, %v", got.Relations, found, err)
	}
	if _, err := s.CreateTaskChecked(ctx, task, DuplicateCheck{Link: first.ID + filed.ID + 1000}); err == nil {
		t.Fatal("link to a missing task accepted")
	}
	if _, err := s.CreateTaskChecked(ctx, task, DuplicateCheck{Link: first.ID, New: true}); err == nil {
		t.Fatal("link and new together accepted")
	}
}

func caseCoreClaimAndRebind(t *testing.T, s *Store) {
	ctx := t.Context()
	project := coreProject(t, s, "core-claim")
	x := coreTask(t, s, project, "core-lane", "claim me")

	if _, err := s.ClaimTask(ctx, x.ID, "worker-1", "", ""); !errors.Is(err, ErrTaskJobRequired) {
		t.Fatalf("bare claim = %v", err)
	}
	claimed, err := s.ClaimTask(ctx, x.ID, "worker-1", "job-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ClaimedBy != "worker-1" || claimed.Refs.JobID != "job-1" || claimed.State == StateBacklog || claimed.StartedAt == nil {
		t.Fatalf("claimed = %+v", claimed)
	}
	// The same claim replays; another claimant conflicts.
	if again, err := s.ClaimTask(ctx, x.ID, "worker-1", "job-1", ""); err != nil || again.ID != x.ID {
		t.Fatalf("replayed claim = %v", err)
	}
	if _, err := s.ClaimTask(ctx, x.ID, "worker-2", "job-2", ""); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("second claimant = %v", err)
	}
	if _, err := s.ClaimTask(ctx, x.ID+1000, "worker-1", "job-9", ""); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("claim of a missing task = %v", err)
	}

	// Rebind moves the job link, is idempotent for the same rebind, and is
	// refused from a job the task no longer carries.
	rebound, err := s.RebindTaskJob(ctx, x.ID, "job-1", "job-2", "worker-1", "worker restarted")
	if err != nil {
		t.Fatal(err)
	}
	if rebound.Refs.JobID != "job-2" || rebound.ClaimedBy != "worker-1" {
		t.Fatalf("rebound = %+v", rebound)
	}
	before, _, _ := s.GetTask(ctx, x.ID)
	if _, err := s.RebindTaskJob(ctx, x.ID, "job-1", "job-2", "worker-1", "worker restarted"); err != nil {
		t.Fatalf("replayed rebind = %v", err)
	}
	after, _, _ := s.GetTask(ctx, x.ID)
	if len(after.Events) != len(before.Events) {
		t.Fatalf("replayed rebind wrote events: %d -> %d", len(before.Events), len(after.Events))
	}
	if _, err := s.RebindTaskJob(ctx, x.ID, "job-1", "job-3", "worker-1", "again"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("rebind from a stale job = %v", err)
	}
	if _, err := s.RebindTaskJob(ctx, x.ID, "job-2", "job-2", "worker-1", "same"); err == nil {
		t.Fatal("rebind to the same job accepted")
	}
	if _, err := s.RebindTaskJob(ctx, x.ID+1000, "job-1", "job-2", "worker-1", "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("rebind of a missing task = %v", err)
	}
	backlog := coreTask(t, s, project, "core-lane", "not started")
	if _, err := s.RebindTaskJob(ctx, backlog.ID, "job-1", "job-2", "worker-1", "not started"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("rebind of a backlog task = %v", err)
	}
}

func caseCoreRelane(t *testing.T, s *Store) {
	ctx := t.Context()
	project := coreProject(t, s, "core-relane")
	a := coreTask(t, s, project, "lane-a", "task in lane a")
	coreTask(t, s, project, "lane-b", "task in lane b")

	moved, changed, err := s.RelaneTask(ctx, a.ID, "lane-b", "tester", "move to b", false)
	if err != nil || !changed || moved.Lane != "lane-b" || !moved.UpdatedAt.After(a.UpdatedAt) {
		t.Fatalf("relane = %+v, %v, %v", moved, changed, err)
	}
	noop, changed, err := s.RelaneTask(ctx, a.ID, "lane-b", "tester", "already there", false)
	if err != nil || changed || noop.Lane != "lane-b" {
		t.Fatalf("no-op relane = %+v, %v, %v", noop, changed, err)
	}
	got, _, _ := s.GetTask(ctx, a.ID)
	relanes := 0
	for _, e := range got.Events {
		if e.Kind == "relane" {
			relanes++
			if e.From != "lane-a" || e.To != "lane-b" {
				t.Fatalf("relane event = %+v", e)
			}
		}
	}
	if relanes != 1 {
		t.Fatalf("relane events = %d, want 1", relanes)
	}
	if _, _, err := s.RelaneTask(ctx, a.ID, "lane-new", "tester", "typo guard", false); !errors.Is(err, ErrTaskLaneUnknown) {
		t.Fatalf("unknown lane = %v", err)
	}
	if x, changed, err := s.RelaneTask(ctx, a.ID, "lane-new", "tester", "a new lane", true); err != nil || !changed || x.Lane != "lane-new" {
		t.Fatalf("allowed new lane = %+v, %v, %v", x, changed, err)
	}
	if _, _, err := s.RelaneTask(ctx, a.ID, "lane-b", "tester", "  ", false); err == nil {
		t.Fatal("blank note accepted")
	}
	if _, _, err := s.RelaneTask(ctx, a.ID+1000, "lane-b", "tester", "missing", false); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task = %v", err)
	}
}

func caseCoreSetProjectAndArea(t *testing.T, s *Store) {
	ctx := t.Context()
	one := coreProject(t, s, "core-one")
	coreProject(t, s, "core-two")
	x := coreTask(t, s, one, "core-lane", "reclassify me")

	moved, changed, err := s.SetTaskProject(ctx, x.ID, "core-two", "tester", "belongs to two")
	if err != nil || !changed || moved.Project == nil || *moved.Project != "core-two" {
		t.Fatalf("set project = %+v, %v, %v", moved.Project, changed, err)
	}
	if _, changed, err := s.SetTaskProject(ctx, x.ID, "core-two", "tester", "same"); err != nil || changed {
		t.Fatalf("same project = %v, %v", changed, err)
	}
	if _, _, err := s.SetTaskProject(ctx, x.ID, "core-nope", "tester", "unknown"); !errors.Is(err, ErrTaskProjectUnknown) {
		t.Fatalf("unknown project = %v", err)
	}
	if _, _, err := s.SetTaskProject(ctx, x.ID+1000, "core-two", "tester", "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task = %v", err)
	}
	// The metadata patch rides in the same transaction, even for a no-op move.
	status := "closing"
	if _, changed, err := s.SetTaskProjectWithMetadata(ctx, x.ID, "core-two", "tester", "patch only", &ProjectMetadataPatch{Status: &status}); err != nil || changed {
		t.Fatalf("patch-only = %v, %v", changed, err)
	}
	if meta, err := s.GetProjectMetadata(ctx, "core-two"); err != nil || meta.Status != "closing" {
		t.Fatalf("metadata = %+v, %v", meta, err)
	}

	area := "go-backend"
	tagged, changed, err := s.SetTaskArea(ctx, x.ID, &area, "tester", "backend work")
	if err != nil || !changed || tagged.Area == nil || *tagged.Area != "go-backend" {
		t.Fatalf("set area = %+v, %v, %v", tagged.Area, changed, err)
	}
	if _, changed, err := s.SetTaskArea(ctx, x.ID, &area, "tester", "same"); err != nil || changed {
		t.Fatalf("same area = %v, %v", changed, err)
	}
	cleared, changed, err := s.SetTaskArea(ctx, x.ID, nil, "tester", "clear")
	if err != nil || !changed || cleared.Area != nil {
		t.Fatalf("cleared area = %+v, %v, %v", cleared.Area, changed, err)
	}
	bad := "nonsense"
	if _, _, err := s.SetTaskArea(ctx, x.ID, &bad, "tester", "bad"); !errors.Is(err, ErrTaskAreaUnknown) {
		t.Fatalf("unknown area = %v", err)
	}
	if _, _, err := s.SetTaskArea(ctx, x.ID+1000, &area, "tester", "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task = %v", err)
	}
	got, _, _ := s.GetTask(ctx, x.ID)
	kinds := map[string]int{}
	for _, e := range got.Events {
		kinds[e.Kind]++
	}
	if kinds["project"] != 1 || kinds["area"] != 2 {
		t.Fatalf("event kinds = %v, want one project and two area events", kinds)
	}
}

func caseCoreListFilters(t *testing.T, s *Store) {
	ctx := t.Context()
	p1 := coreProject(t, s, "core-list-a")
	p2 := coreProject(t, s, "core-list-b")
	area := "docs"
	t1 := coreTask(t, s, p1, "core-lane-1", "first listed task")
	t2 := coreTask(t, s, p1, "core-lane-2", "second listed task")
	t3 := coreTask(t, s, p2, "core-lane-1", "third listed task")
	if _, _, err := s.SetTaskArea(ctx, t2.ID, &area, "tester", "docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, t3.ID, "worker", "job-list", ""); err != nil {
		t.Fatal(err)
	}
	ids := func(xs []Task) []int64 {
		out := make([]int64, 0, len(xs))
		for _, x := range xs {
			out = append(out, x.ID)
		}
		return out
	}
	same := func(label string, got []Task, want ...int64) {
		t.Helper()
		g := ids(got)
		if len(g) != len(want) {
			t.Fatalf("%s = %v, want %v", label, g, want)
		}
		for i := range g {
			if g[i] != want[i] {
				t.Fatalf("%s = %v, want %v", label, g, want)
			}
		}
	}

	all, err := s.ListTasksFiltered(ctx, TaskQuery{})
	if err != nil {
		t.Fatal(err)
	}
	same("default order", all, t1.ID, t2.ID, t3.ID)
	byLane, _ := s.ListTasks(ctx, "core-lane-1", "", "", nil, nil, 10)
	same("lane", byLane, t1.ID, t3.ID)
	byState, _ := s.ListTasks(ctx, "", StateBacklog, "", nil, nil, 10)
	same("state", byState, t1.ID, t2.ID)
	started, _ := s.ListTasks(ctx, "", "developing", "", nil, nil, 10)
	if len(started) > 1 {
		t.Fatalf("developing = %v", ids(started))
	}
	byProject, _ := s.ListTasks(ctx, "", "", "", p2, nil, 10)
	same("project", byProject, t3.ID)
	none := ""
	noProject, _ := s.ListTasks(ctx, "", "", "", &none, nil, 10)
	same("no project", noProject)
	byArea, _ := s.ListTasks(ctx, "", "", "", nil, &area, 10)
	same("area", byArea, t2.ID)
	noArea, _ := s.ListTasks(ctx, "", "", "", nil, &none, 10)
	same("no area", noArea, t1.ID, t3.ID)
	limited, _ := s.ListTasks(ctx, "", "", "", nil, nil, 2)
	same("limit", limited, t1.ID, t2.ID)

	page1, err := s.ListTasksPage(ctx, "", "", "", nil, nil, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	same("page 1", page1, t1.ID, t2.ID)
	page2, _ := s.ListTasksPage(ctx, "", "", "", nil, nil, t2.ID, 2)
	same("page 2", page2, t3.ID)

	// The updated_at keyset orders by (updated_at, id), strictly after a row.
	since := t1.CreatedAt.Add(-time.Minute)
	keyset, err := s.ListTasksFiltered(ctx, TaskQuery{UpdatedSince: &since, Limit: 10})
	if err != nil || len(keyset) != 3 {
		t.Fatalf("keyset = %v, %v", ids(keyset), err)
	}
	for i := 1; i < len(keyset); i++ {
		a, b := keyset[i-1], keyset[i]
		if a.UpdatedAt.After(b.UpdatedAt) || (a.UpdatedAt.Equal(b.UpdatedAt) && a.ID > b.ID) {
			t.Fatalf("keyset order = %v", ids(keyset))
		}
	}
	rest, err := s.ListTasksFiltered(ctx, TaskQuery{AfterUpdatedAt: &keyset[0].UpdatedAt, AfterID: &keyset[0].ID, Limit: 10})
	if err != nil || len(rest) != 2 || rest[0].ID != keyset[1].ID {
		t.Fatalf("after keyset = %v, %v", ids(rest), err)
	}

	// Nothing just touched is stalled; no task is linked to change 1.
	stalled, err := s.ListTasksFiltered(ctx, TaskQuery{Stalled: true})
	if err != nil || len(stalled) != 0 {
		t.Fatalf("stalled = %v, %v", ids(stalled), err)
	}
	changeID := int64(1)
	linked, err := s.ListTasksFiltered(ctx, TaskQuery{ChangeID: &changeID})
	if err != nil || len(linked) != 0 {
		t.Fatalf("linked = %v, %v", ids(linked), err)
	}

	// Malformed filters are refused, not turned into an empty page.
	if _, err := s.ListTasks(ctx, "bad lane", "", "", nil, nil, 10); err == nil {
		t.Fatal("invalid lane accepted")
	}
	if _, err := s.ListTasks(ctx, "", "no-such-state", "", nil, nil, 10); err == nil {
		t.Fatal("invalid state accepted")
	}
	badArea := "nonsense"
	if _, err := s.ListTasks(ctx, "", "", "", nil, &badArea, 10); !errors.Is(err, ErrTaskAreaUnknown) {
		t.Fatalf("invalid area = %v", err)
	}
}

func caseCoreGlance(t *testing.T, s *Store) {
	ctx := t.Context()
	project := coreProject(t, s, "core-glance")
	a := coreTask(t, s, project, "core-lane", "glance one")
	coreTask(t, s, project, "core-lane", "glance two")
	if _, err := s.ClaimTask(ctx, a.ID, "worker", "job-glance", ""); err != nil {
		t.Fatal(err)
	}
	counts, active, err := s.GlanceTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[StateBacklog] != 1 || counts[AwaitingDecisionKey] != 0 {
		t.Fatalf("counts = %v", counts)
	}
	total := 0
	for state, n := range counts {
		if state != AwaitingDecisionKey {
			total += n
		}
	}
	if total != 2 {
		t.Fatalf("counts = %v, want two tasks in all", counts)
	}
	if len(active) != 1 || active[0].ID != a.ID {
		t.Fatalf("active = %+v", active)
	}
}

func caseCoreMilestones(t *testing.T, s *Store) {
	ctx := t.Context()
	project := coreProject(t, s, "core-mile")
	other := coreProject(t, s, "core-mile-other")
	target := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	m, err := s.SaveMilestone(ctx, ProjectMilestone{Project: "core-mile", Name: "beta", Description: "first cut", TargetAt: &target, Status: "open", SortOrder: 2})
	if err != nil || m.ID < 1 {
		t.Fatalf("create milestone = %+v, %v", m, err)
	}
	got, err := s.GetMilestone(ctx, m.ID)
	if err != nil || got.Name != "beta" || got.Project != "core-mile" || got.SortOrder != 2 || got.TargetAt == nil || !got.TargetAt.Equal(target) {
		t.Fatalf("milestone = %+v, %v", got, err)
	}
	m.Name, m.Status = "beta two", "closed"
	if _, err := s.SaveMilestone(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMilestone(ctx, m.ID)
	if got.Name != "beta two" || got.Status != "closed" {
		t.Fatalf("updated milestone = %+v", got)
	}
	if _, err := s.SaveMilestone(ctx, ProjectMilestone{Project: "core-missing", Name: "x"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("milestone of a missing project = %v", err)
	}
	if _, err := s.SaveMilestone(ctx, ProjectMilestone{ID: m.ID + 1000, Project: "core-mile", Name: "ghost"}); !errors.Is(err, ErrMilestoneNotFound) {
		t.Fatalf("update of a missing milestone = %v", err)
	}
	if _, err := s.GetMilestone(ctx, m.ID+1000); !errors.Is(err, ErrMilestoneNotFound) {
		t.Fatalf("get missing = %v", err)
	}

	desc, order := "second cut", 5
	patched, err := s.PatchMilestone(ctx, m.ID, MilestonePatch{Description: &desc, SortOrder: &order, SetTarget: true})
	if err != nil || patched.Description != "second cut" || patched.SortOrder != 5 || patched.TargetAt != nil || patched.Name != "beta two" {
		t.Fatalf("patched = %+v, %v", patched, err)
	}
	if _, err := s.PatchMilestone(ctx, m.ID, MilestonePatch{Project: other}); !errors.Is(err, ErrMilestoneProject) {
		t.Fatalf("project change = %v", err)
	}
	if _, err := s.PatchMilestone(ctx, m.ID+1000, MilestonePatch{Description: &desc}); !errors.Is(err, ErrMilestoneNotFound) {
		t.Fatalf("patch missing = %v", err)
	}

	// A task takes a milestone of its own project only, and clears it again.
	x := coreTask(t, s, project, "core-lane", "milestone task")
	set, changed, err := s.SetTaskMilestone(ctx, x.ID, &m.ID, "tester")
	if err != nil || !changed || set.MilestoneID == nil || *set.MilestoneID != m.ID {
		t.Fatalf("set milestone = %+v, %v, %v", set.MilestoneID, changed, err)
	}
	if _, changed, err := s.SetTaskMilestone(ctx, x.ID, &m.ID, "tester"); err != nil || changed {
		t.Fatalf("same milestone = %v, %v", changed, err)
	}
	if _, _, err := s.SetTaskProject(ctx, x.ID, "core-mile-other", "tester", "move"); !errors.Is(err, ErrMilestoneProject) {
		t.Fatalf("project move with a milestone = %v", err)
	}
	foreign, err := s.SaveMilestone(ctx, ProjectMilestone{Project: "core-mile-other", Name: "foreign"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetTaskMilestone(ctx, x.ID, &foreign.ID, "tester"); !errors.Is(err, ErrMilestoneProject) {
		t.Fatalf("foreign milestone = %v", err)
	}
	missing := m.ID + 1000
	if _, _, err := s.SetTaskMilestone(ctx, x.ID, &missing, "tester"); !errors.Is(err, ErrMilestoneNotFound) {
		t.Fatalf("missing milestone = %v", err)
	}
	if _, _, err := s.SetTaskMilestone(ctx, x.ID+1000, &m.ID, "tester"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task = %v", err)
	}
	cleared, changed, err := s.SetTaskMilestone(ctx, x.ID, nil, "tester")
	if err != nil || !changed || cleared.MilestoneID != nil {
		t.Fatalf("cleared milestone = %+v, %v, %v", cleared.MilestoneID, changed, err)
	}
}

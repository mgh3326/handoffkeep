package store

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestYTLinksWriterEndpoint: each projected edge has exactly one writer task
// (I2). A related edge is written by its lower id whichever way it was given.
func TestYTLinksWriterEndpoint(t *testing.T) {
	for _, c := range []struct {
		kind     string
		src, dst int64
		want     int64
		ok       bool
	}{
		{ytLinkKindParent, 5, 3, 5, true}, // the child
		{RelationDuplicateOf, 7, 2, 7, true},
		{RelationDuplicateOf, 2, 7, 2, true},
		{RelationRelated, 4, 9, 4, true},
		{RelationRelated, 9, 4, 4, true},
		{RelationBlocks, 4, 9, 0, false},
	} {
		if got, ok := ytLinkWriter(c.kind, c.src, c.dst); got != c.want || ok != c.ok {
			t.Errorf("ytLinkWriter(%s, %d, %d) = %d, %t; want %d, %t", c.kind, c.src, c.dst, got, ok, c.want, c.ok)
		}
	}
}

// ytlFeedIDs returns the task ids the change feed recorded after sync id
// `after`, ascending.
func ytlFeedIDs(t *testing.T, s *Store, after int64) []string {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), `SELECT DISTINCT model_id FROM sync_log WHERE model='task' AND sync_id > $1 ORDER BY model_id`, after)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func ytlIDs(ids ...int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	slices.Sort(out)
	return out
}

// TestYTLinksRelationEditReachesFeedForBothEndpoints: a relation-only edit
// writes a feed row for both tasks in its own transaction (I3), with the
// higher id as the subject of the call too.
func TestYTLinksRelationEditReachesFeedForBothEndpoints(t *testing.T) {
	s := schemaStore(t, "yt_links_bump")
	ctx := t.Context()
	lo, hi := ytFeedTask(t, s, "lower"), ytFeedTask(t, s, "higher")
	want := ytlIDs(lo.ID, hi.ID)
	steps := []struct {
		name string
		do   func() (bool, error)
	}{
		{"add related, higher id subject", func() (bool, error) { return s.AddTaskRelation(ctx, hi.ID, RelationRelated, lo.ID, "tester") }},
		{"archive related, higher id subject", func() (bool, error) { return s.RemoveTaskRelation(ctx, hi.ID, RelationRelated, lo.ID, "tester") }},
		{"revive related, lower id subject", func() (bool, error) { return s.AddTaskRelation(ctx, lo.ID, RelationRelated, hi.ID, "tester") }},
		{"archive related, lower id subject", func() (bool, error) { return s.RemoveTaskRelation(ctx, lo.ID, RelationRelated, hi.ID, "tester") }},
		{"add blocks", func() (bool, error) { return s.AddTaskRelation(ctx, hi.ID, RelationBlocks, lo.ID, "tester") }},
		{"archive blocks", func() (bool, error) { return s.RemoveTaskRelation(ctx, hi.ID, RelationBlocks, lo.ID, "tester") }},
	}
	for _, step := range steps {
		before := ytFeedMaxSyncID(t, s)
		if changed, err := step.do(); err != nil || !changed {
			t.Fatalf("%s: changed=%t err=%v", step.name, changed, err)
		}
		if got := ytlFeedIDs(t, s, before); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: feed rows for tasks %v, want %v", step.name, got, want)
		}
	}
	// An edit that changes nothing writes no feed row.
	before := ytFeedMaxSyncID(t, s)
	if changed, err := s.RemoveTaskRelation(ctx, lo.ID, RelationRelated, hi.ID, "tester"); err != nil || changed {
		t.Fatalf("repeated archive: changed=%t err=%v", changed, err)
	}
	if got := ytlFeedIDs(t, s, before); len(got) != 0 {
		t.Fatalf("no-op archive wrote feed rows for %v", got)
	}
}

// TestYTLinksDuplicateOfArchiveReachesFeed: reopening a duplicate archives
// its duplicate_of edge, and RemoveTaskRelation of a live duplicate_of edge on
// a task that is not a duplicate archives it; both reach the feed for the
// duplicate (the writer) and the original.
func TestYTLinksDuplicateOfArchiveReachesFeed(t *testing.T) {
	s := schemaStore(t, "yt_links_dup")
	ctx := t.Context()
	orig, dup := ytFeedTask(t, s, "original"), ytFeedTask(t, s, "duplicate")
	want := ytlIDs(orig.ID, dup.ID)

	before := ytFeedMaxSyncID(t, s)
	if changed, err := s.AddTaskRelation(ctx, dup.ID, RelationDuplicateOf, orig.ID, "tester"); err != nil || !changed {
		t.Fatalf("add duplicate_of: changed=%t err=%v", changed, err)
	}
	if got := ytlFeedIDs(t, s, before); !reflect.DeepEqual(got, want) {
		t.Fatalf("add duplicate_of: feed rows for tasks %v, want %v", got, want)
	}

	before = ytFeedMaxSyncID(t, s)
	if _, err := s.ReopenTask(ctx, dup.ID, "tester", "not a duplicate"); err != nil {
		t.Fatal(err)
	}
	if got := ytlFeedIDs(t, s, before); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopen of a duplicate: feed rows for tasks %v, want %v", got, want)
	}

	// A live duplicate_of edge on a task that is not a duplicate (an edge no
	// current path leaves behind, but RemoveTaskRelation accepts it).
	ytFeedExec(t, s, `UPDATE task_relations SET archived_at=NULL, archived_by='' WHERE kind='duplicate_of' AND src_id=$1 AND dst_id=$2`, dup.ID, orig.ID)
	before = ytFeedMaxSyncID(t, s)
	if changed, err := s.RemoveTaskRelation(ctx, dup.ID, RelationDuplicateOf, orig.ID, "tester"); err != nil || !changed {
		t.Fatalf("remove duplicate_of: changed=%t err=%v", changed, err)
	}
	if got := ytlFeedIDs(t, s, before); !reflect.DeepEqual(got, want) {
		t.Fatalf("remove duplicate_of: feed rows for tasks %v, want %v", got, want)
	}
}

func ytlBind(t *testing.T, s *Store, tenant string, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		ytFeedExec(t, s, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, bound_by, bound_at) VALUES ($1,$2,$3,'test',now())`, id, tenant, ytfIssue(id))
	}
}

func ytlDesired(t *testing.T, s *Store, tenant string, id int64) (ytLinksDesired, bool) {
	t.Helper()
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	got, found, err := ytLinksDesiredTx(t.Context(), tx, tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return got, found
}

func ytlTarget(id int64) ytLinkTarget { return ytLinkTarget{TaskID: id, IssueID: ytfIssue(id)} }

// TestYTLinksDesiredSet: a writer's desired set holds its parent, the related
// edges where it is the lower id and its duplicate_of edges, with bound
// targets as issues and unbound ones held (I2, I14). The higher endpoint of a
// related edge has no relates for it.
func TestYTLinksDesiredSet(t *testing.T) {
	s := schemaStore(t, "yt_links_desired")
	ctx := t.Context()
	const other = "other.youtrack.cloud"
	// Creation order fixes the ids: lower < writer < everything else.
	lower := ytFeedTask(t, s, "lower related")
	writer := ytFeedTask(t, s, "writer")
	parent := ytFeedTask(t, s, "parent")
	peer := ytFeedTask(t, s, "related peer")
	gone := ytFeedTask(t, s, "archived related")
	unbound := ytFeedTask(t, s, "unbound related")
	elsewhere := ytFeedTask(t, s, "bound on another tenant")
	orig := ytFeedTask(t, s, "original")
	orphan := ytFeedTask(t, s, "child of an unbound parent")
	ytlBind(t, s, ytcTenant, lower.ID, writer.ID, parent.ID, peer.ID, gone.ID, orig.ID, orphan.ID)
	ytlBind(t, s, other, elsewhere.ID)

	setParent(t, s, writer, parent)
	setParent(t, s, orphan, unbound)
	for _, e := range []struct {
		src  int64
		kind string
		dst  int64
	}{
		{peer.ID, RelationRelated, writer.ID}, // given from the higher end
		{writer.ID, RelationRelated, gone.ID},
		{writer.ID, RelationRelated, unbound.ID},
		{writer.ID, RelationRelated, elsewhere.ID},
		{lower.ID, RelationRelated, writer.ID},
		{writer.ID, RelationBlocks, peer.ID}, // not projected
		{writer.ID, RelationDuplicateOf, orig.ID},
	} {
		if _, err := s.AddTaskRelation(ctx, e.src, e.kind, e.dst, "tester"); err != nil {
			t.Fatalf("add %d %s %d: %v", e.src, e.kind, e.dst, err)
		}
	}
	if _, err := s.RemoveTaskRelation(ctx, writer.ID, RelationRelated, gone.ID, "tester"); err != nil {
		t.Fatal(err)
	}

	got, found := ytlDesired(t, s, ytcTenant, writer.ID)
	p := ytlTarget(parent.ID)
	want := ytLinksDesired{
		Parent:     &p,
		Related:    []ytLinkTarget{ytlTarget(peer.ID)},
		Duplicates: []ytLinkTarget{ytlTarget(orig.ID)},
		Held:       []ytLinkHeld{{Kind: RelationRelated, TaskID: unbound.ID}, {Kind: RelationRelated, TaskID: elsewhere.ID}},
	}
	if !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("writer desired = %+v (found=%t)\nwant %+v", got, found, want)
	}

	// The lower endpoint of lower–writer writes that link; the higher
	// endpoint of writer–peer has no relates at all.
	if got, _ := ytlDesired(t, s, ytcTenant, lower.ID); !reflect.DeepEqual(got.Related, []ytLinkTarget{ytlTarget(writer.ID)}) || got.Parent != nil {
		t.Fatalf("lower desired = %+v", got)
	}
	if got, _ := ytlDesired(t, s, ytcTenant, peer.ID); len(got.Related) != 0 || len(got.Duplicates) != 0 || len(got.Held) != 0 || got.Parent != nil {
		t.Fatalf("higher endpoint desired = %+v, want empty", got)
	}
	// A parent with no binding on the tenant is held, not written.
	if got, _ := ytlDesired(t, s, ytcTenant, orphan.ID); got.Parent != nil || !reflect.DeepEqual(got.Held, []ytLinkHeld{{Kind: ytLinkKindParent, TaskID: unbound.ID}}) {
		t.Fatalf("orphan desired = %+v", got)
	}
	// Seen from the other tenant, every target but elsewhere is unbound.
	if got, _ := ytlDesired(t, s, other, writer.ID); got.Parent != nil || len(got.Related) != 1 || got.Related[0] != ytlTarget(elsewhere.ID) || len(got.Held) != 4 {
		t.Fatalf("writer desired on the other tenant = %+v", got)
	}
	if _, found := ytlDesired(t, s, ytcTenant, 1<<40); found {
		t.Fatal("a missing task was found")
	}
}

// TestYTLinkWritersOf: the writers that point at a task are its children,
// its duplicates and the lower id of each related edge where it is the higher
// id; not the higher end of its own related edges, and no archived edge.
func TestYTLinkWritersOf(t *testing.T) {
	s := schemaStore(t, "yt_links_writers")
	ctx := t.Context()
	lower := ytFeedTask(t, s, "lower related")
	target := ytFeedTask(t, s, "target")
	child1, child2 := ytFeedTask(t, s, "child 1"), ytFeedTask(t, s, "child 2")
	dup := ytFeedTask(t, s, "duplicate")
	higher := ytFeedTask(t, s, "higher related")
	gone := ytFeedTask(t, s, "archived duplicate")
	blocker := ytFeedTask(t, s, "blocker")
	setParent(t, s, child2, target)
	setParent(t, s, child1, target)
	for _, e := range []struct {
		src  int64
		kind string
		dst  int64
	}{
		{target.ID, RelationRelated, lower.ID},
		{target.ID, RelationRelated, higher.ID},
		{dup.ID, RelationDuplicateOf, target.ID},
		{gone.ID, RelationDuplicateOf, target.ID},
		{blocker.ID, RelationBlocks, target.ID},
	} {
		if _, err := s.AddTaskRelation(ctx, e.src, e.kind, e.dst, "tester"); err != nil {
			t.Fatalf("add %d %s %d: %v", e.src, e.kind, e.dst, err)
		}
	}
	if _, err := s.ReopenTask(ctx, gone.ID, "tester", "not a duplicate"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	got, err := ytLinkWritersOfTx(ctx, tx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{lower.ID, child1.ID, child2.ID, dup.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("writers of %d = %v, want %v", target.ID, got, want)
	}
}

// TestYTIssueLinksMigration: the migration is registered, records its ID, and
// its tables keep one live ownership row per link and an append-only plan log.
func TestYTIssueLinksMigration(t *testing.T) {
	if !slices.ContainsFunc(migrationRegistry, func(m migration) bool { return m.ID == YTIssueLinksMigration }) {
		t.Fatalf("%s is not registered", YTIssueLinksMigration)
	}
	s := schemaStore(t, "yt_links_migration")
	ctx := t.Context()
	if n := ytcCount(t, s, `SELECT count(*) FROM schema_migrations WHERE id=$1`, YTIssueLinksMigration); n != 1 {
		t.Fatalf("schema_migrations rows for %s = %d", YTIssueLinksMigration, n)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_links_sweep`); n != 0 {
		t.Fatalf("yt_links_sweep rows = %d, want 0", n)
	}
	own := `INSERT INTO yt_link_owned(tenant, link_type, from_issue, to_issue, writer_task_id, target_task_id, kind, intended_op_id, intended_at)
		VALUES ('t', 'relates', '2-1', '2-2', 1, 2, 'related', $1, now()) RETURNING id`
	var first int64
	if err := s.pool.QueryRow(ctx, own, 10).Scan(&first); err != nil {
		t.Fatal(err)
	}
	wantViolation := func(what string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != "23505" && pgErr.Code != "23514" && pgErr.Code != "P0001") {
			t.Fatalf("%s: err=%v, want a constraint or guard error", what, err)
		}
	}
	var id int64
	wantViolation("second live owner of one link", s.pool.QueryRow(ctx, own, 11).Scan(&id))
	ytFeedExec(t, s, `UPDATE yt_link_owned SET released_op_id=11 WHERE id=$1`, first)
	if err := s.pool.QueryRow(ctx, own, 12).Scan(&id); err != nil {
		t.Fatalf("owning a released link again: %v", err)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO yt_link_owned(tenant, link_type, from_issue, to_issue, writer_task_id, target_task_id, kind, intended_op_id, intended_at)
		VALUES ('t', 'subtask', '2-1', '2-2', 1, 2, 'related', 1, now())`)
	wantViolation("kind and link type disagree", err)

	ytFeedExec(t, s, `INSERT INTO yt_link_plans(op_id, attempt, plan, created_at) VALUES (10, 1, '{}', now())`)
	_, err = s.pool.Exec(ctx, `INSERT INTO yt_link_plans(op_id, attempt, plan, created_at) VALUES (10, 1, '{}', now())`)
	wantViolation("a second plan for one attempt", err)
	_, err = s.pool.Exec(ctx, `UPDATE yt_link_plans SET plan='{"x":1}'`)
	wantViolation("update of a plan", err)
}

// TestYTLinksRelationBumpNeverMovesUpdatedAtBack: the endpoint bump keeps a
// newer revision of the linked task (PR #420 review: a create that links an
// existing task must not regress that task's updated_at).
func TestYTLinksRelationBumpNeverMovesUpdatedAtBack(t *testing.T) {
	s := schemaStore(t, "yt_links_monotonic")
	ctx := t.Context()
	linked := ytFeedTask(t, s, "linked")
	ahead := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	ytFeedExec(t, s, `UPDATE tasks SET updated_at=$2 WHERE id=$1`, linked.ID, ahead)
	before := ytFeedMaxSyncID(t, s)
	created, err := s.CreateTaskChecked(ctx, Task{Lane: "yt-feed-test", Title: "links to linked", Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName)}, DuplicateCheck{Link: linked.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, s, linked.ID).UpdatedAt; !got.Equal(ahead) {
		t.Fatalf("linked updated_at = %v, want the newer %v kept", got, ahead)
	}
	if got, want := ytlFeedIDs(t, s, before), ytlIDs(linked.ID, created.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("feed rows for tasks %v, want %v", got, want)
	}
}

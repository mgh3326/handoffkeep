//go:build mysql

package store

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// seedTasksAreaPostgres fills every table of the tasks area through raw SQL:
// the copy must carry rows the store's own methods would never write in this
// shape (a child with a lower id than its parent, an empty text array, a JSON
// column with unordered keys).
func seedTasksAreaPostgres(t *testing.T, pool *pgxpool.Pool, when time.Time) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO task_projects(name,created_by,created_at,status,start_at,target_at,design_lane) VALUES('copy-proj','test',$1,'active',$1,$1,'design-lane')`, when)
	exec(`INSERT INTO project_milestones(id,project,name,description,target_at,status,sort_order) VALUES(93001,'copy-proj','m1','d',$1,'open',1)`, when)
	exec(`INSERT INTO project_owner_events(id,project,previous_lane,design_lane,actor,note,created_at) VALUES(93001,'copy-proj','','design-lane','test','n',$1)`, when)
	// The parent has the larger id: a load ordered by id alone would insert the
	// child first and trip the enforced foreign key.
	exec(`INSERT INTO tasks(id,lane,title,kind,state,refs,created_by,created_at,updated_at,last_activity_at) VALUES(93002,'lane-a','parent','implement','developing','{"b":2,"decision_request":{"id":"copy/request-1"},"a":1}','test',$1,$1,$1)`, when)
	exec(`INSERT INTO tasks(id,lane,title,kind,state,created_by,created_at,updated_at,last_activity_at,project,parent_id,milestone_id,source,source_ref,awaiting_decision) VALUES(93001,'lane-a','child','implement','backlog','test',$1,$1,$1,'copy-proj',93002,93001,'copy','ref-1',TRUE)`, when)
	exec(`INSERT INTO tasks(id,lane,title,kind,state,created_by,created_at,updated_at,last_activity_at,area,expected,due_at) VALUES(93003,'lane-b','research','research','triage','test',$1,$1,$1,'docs','done when done',$1)`, when)
	exec(`INSERT INTO documents(id,key,kind,session,job,body,sha256,created_by,created_at,updated_at,task_id,origin_class) VALUES(93001,'copy/doc','brief','','','long body','hash','test',$1,$1,93001,'authored')`, when)
	exec(`INSERT INTO task_comments(id,task_id,body,author,created_at,source_ref,system_event_key) VALUES(93001,93001,'comment','test',$1,'ref',NULL)`, when)
	exec(`INSERT INTO mcp_write_audit(id,tool,actor,channel,author,task_id,idempotency_key,result,at) VALUES(93001,'comment','actor','chatgpt','test',93001,'key','{"id":93001}',$1)`, when)
	exec(`INSERT INTO translations(entity,entity_id,field,lang,source_sha256,text,translator,updated_at) VALUES('task','93001','title','ko',repeat('a',64),'translated','test',$1)`, when)
	exec(`INSERT INTO task_events(id,task_id,"from","to","by",note,at,refs,kind,no_job,source,source_ref,field,from_value,to_value,auto,applied) VALUES(93001,93001,'backlog','developing','test','n',$1,'{"x":1,"a":[1,2]}','transition','','copy','ref-1','state','backlog','developing',TRUE,TRUE)`, when)
	exec(`INSERT INTO task_events(id,task_id,"from","to","by",note,at,kind) VALUES(93002,93001,'','','test','plain',$1,'metadata')`, when)
	exec(`INSERT INTO task_relations(id,kind,src_id,dst_id,created_by,created_at,archived_at,archived_by) VALUES(93001,'blocks',93001,93002,'test',$1,NULL,'')`, when)
	exec(`INSERT INTO task_origins(id,task_id,provider,namespace,external_id,display_ref,url,title_snapshot,snapshot_at,created_by,created_at,tenant,role,synced_at) VALUES(93001,93003,'youtrack','MGH','MGH-1','MGH-1','https://example.test/issue/MGH-1','t',$1,'test',$1,'tenant','origin',$1)`, when)
	exec(`INSERT INTO task_origins(id,task_id,provider,namespace,external_id,snapshot_at,created_by,created_at) VALUES(93002,93001,'github','org/repo','12',$1,'test',$1)`, when)
	exec(`INSERT INTO ticket_sets(id,source,source_ref,payload_sha256,parent_task_id,created_at) VALUES(93001,'copy','set-1',repeat('a',64),93002,$1)`, when)
	exec(`INSERT INTO task_spawn_records(task_id,request_id,job,state,attempt,updated_at) VALUES(93001,'r','j','waiting',1,$1)`, when)
	exec(`INSERT INTO task_spawn_notices(notice_key,task_id,created_at) VALUES('93001|r|1|waiting',93001,$1)`, when)
	exec(`INSERT INTO task_thread_deliveries(task_id,destination,thread_id,thread_name,start_event_id,since,last_event_id,posted,state,next_attempt_at,created_at,updated_at) VALUES(93001,'discord','12345','t',1,$1,2,ARRAY['a','b,c','d"e'],'active',$1,$1,$1)`, when)
	exec(`INSERT INTO task_thread_deliveries(task_id,destination,thread_id,thread_name,start_event_id,since,last_event_id,posted,state,next_attempt_at,created_at,updated_at) VALUES(93002,'discord',NULL,'t2',1,$1,2,ARRAY[]::TEXT[],'pending',$1,$1,$1)`, when)
	exec(`INSERT INTO task_thread_cursors(destination,epoch_event_id,scan_event_id,updated_at) VALUES('discord',1,2,$1)`, when)
	exec(`INSERT INTO checkpoints(id,session,kind,title,body,refs,created_by,created_at) VALUES(93001,'sess','checkpoint','t','body','{"k":"v"}','test',$1)`, when)
	exec(`INSERT INTO memory(agent,name,description,memory_type,content,updated_by,updated_at) VALUES('agent','name','desc','user','content','test',$1)`, when)
}

// The tasks area copies in both directions with parity, and neither target
// journals the load: the PostgreSQL emit triggers skip on the bulk-load GUC and
// the MySQL ones on @hk_bulk_load, so sync_log stays empty while sync_decisions,
// which is copied itself, arrives intact.
func TestCopyTasksAreaRoundTripWithParity(t *testing.T) {
	src, srcURL := schemaStoreAt(t, "copy_tasks_src")
	when := time.Date(2026, 11, 18, 3, 4, 5, 123456000, time.UTC)
	seedTasksAreaPostgres(t, src.pool, when)
	var sourceDecisions, sourceFeed int
	if err := src.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sync_decisions), (SELECT count(*) FROM sync_log)`).Scan(&sourceDecisions, &sourceFeed); err != nil {
		t.Fatal(err)
	}
	if sourceDecisions != 1 || sourceFeed == 0 {
		t.Fatalf("seed through triggers: sync_decisions=%d sync_log=%d, want 1 and some", sourceDecisions, sourceFeed)
	}
	myURL := testdb.MySQLDatabase(t)
	my, err := Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	my.Close()

	tables := tasksAreaCopyTables()
	forward, err := CopyStore(t.Context(), srcURL, myURL, tables, false)
	if err != nil {
		t.Fatal(err)
	}
	if !forward.Match || len(forward.Tables) != len(tables) {
		t.Fatalf("PostgreSQL to MySQL parity mismatch: %+v", forward)
	}
	my, err = Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	var feed, decisions int
	var nextTask int64
	if err := my.mysqlPool.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM sync_log), (SELECT COUNT(*) FROM sync_decisions), (SELECT AUTO_INCREMENT FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='tasks')`).Scan(&feed, &decisions, &nextTask); err != nil {
		t.Fatal(err)
	}
	my.Close()
	if feed != 0 || decisions != 1 || nextTask <= 93003 {
		t.Fatalf("MySQL target: sync_log=%d sync_decisions=%d next task id=%d, want 0, 1, >93003", feed, decisions, nextTask)
	}

	back, backURL := schemaStoreAt(t, "copy_tasks_back")
	reverse, err := CopyStore(t.Context(), myURL, backURL, tables, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reverse.Match || len(reverse.Tables) != len(tables) {
		t.Fatalf("MySQL to PostgreSQL parity mismatch: %+v", reverse)
	}
	var backFeed, backDecisions int
	var nextID int64
	if err := back.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sync_log), (SELECT count(*) FROM sync_decisions), nextval(pg_get_serial_sequence('tasks','id'))`).Scan(&backFeed, &backDecisions, &nextID); err != nil {
		t.Fatal(err)
	}
	if backFeed != 0 || backDecisions != 1 || nextID <= 93003 {
		t.Fatalf("PostgreSQL target: sync_log=%d sync_decisions=%d next task id=%d, want 0, 1, >93003", backFeed, backDecisions, nextID)
	}
	// The copied tables stay writable and journaled once the load is over.
	if _, err := back.pool.Exec(t.Context(), `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at) VALUES('lane-a','after','implement','backlog','test',now(),now())`); err != nil {
		t.Fatal(err)
	}
	if err := back.pool.QueryRow(t.Context(), `SELECT count(*) FROM sync_log`).Scan(&backFeed); err != nil || backFeed != 1 {
		t.Fatalf("sync_log after a normal insert = %d (%v), want 1", backFeed, err)
	}
}

func tasksAreaCopyTables() []string {
	var out []string
	for _, name := range StoreCopyTables() {
		if !strings.HasPrefix(name, "bench_") && !strings.HasPrefix(name, "quota_") {
			out = append(out, name)
		}
	}
	return out
}

// A column value larger than the MySQL limit is refused, never truncated: the
// tasks.source_ref bound is 512 characters.
func TestCopyRefusesOverlongTaskSourceRef(t *testing.T) {
	src, srcURL := schemaStoreAt(t, "copy_tasks_overlong")
	when := time.Date(2026, 11, 18, 3, 4, 5, 0, time.UTC)
	if _, err := src.pool.Exec(t.Context(), `INSERT INTO tasks(id,lane,title,kind,state,created_by,created_at,updated_at,source,source_ref) VALUES(94001,'lane-a','long','implement','backlog','test',$1,$1,'copy',$2)`, when, strings.Repeat("r", 513)); err != nil {
		t.Fatal(err)
	}
	myURL := testdb.MySQLDatabase(t)
	my, err := Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	my.Close()
	_, err = CopyStore(t.Context(), srcURL, myURL, []string{"tasks"}, false)
	if err == nil || !strings.Contains(err.Error(), "refusing tasks.source_ref") {
		t.Fatalf("expected overlong-value refusal, got %v", err)
	}
}

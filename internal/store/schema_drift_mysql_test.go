//go:build mysql

package store

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// These are deliberately named, not a wildcard: an unexpected PostgreSQL
// table must fail this check until its owner ports it or records an exception.
var mysqlNotYetPorted = wordSet(`attachment_refs attachment_uploads attachment_usage attachments chat_message_questions chat_messages chat_questions decision_answer_attempts decision_deliveries decision_notify_digest decision_requests decision_resolutions documents dots_results dots_run_tokens jobs ledger_cursors locks mcp_write_audit notification_outbox operator_inbox_events operator_inbox_state placement_decisions relay_events takes translations youtrack_projects yt_article_bindings yt_authority yt_binding_cursor yt_binding_history yt_binding_intents yt_comment_canary yt_comment_write_links yt_decision_bindings yt_entity_map yt_family_roots yt_feed_cursors yt_issue_comment_bounds yt_issue_intents yt_link_owned yt_link_plans yt_link_steps yt_link_unowned yt_links_sweep yt_snapshot_state yt_task_bindings yt_write_op_alerts yt_write_ops`)
var mysqlLeftOut = wordSet(`decision_notify deploy_changes deploys labels linear_board_owners linear_board_ownership_conflicts linear_board_tasks linear_capacity_state linear_create_admissions linear_decision_issues linear_decision_ops linear_issues linear_outbox schema_migrations schema_version services standing_orders status_updates task_labels task_state_migration task_subscribers verifications youtrack_attempts youtrack_comment_links youtrack_cursor youtrack_decision_mirrors youtrack_doc_articles youtrack_field_cache youtrack_outbox youtrack_processed_activities yt_write_op_adoptions`)

// Tables of other areas that exist on MySQL only as the stubs of
// migration_mysql_tasks.go; their owners extend them when they port.
var mysqlStubs = wordSet(`jobs relay_events chat_questions`)
var mysqlPorted = wordSet(`bench_area_exceptions bench_catalog bench_grades bench_reps bench_scores change_checks change_findings change_gate_events change_tasks change_threads changes checkpoints deploy_record_projection deploy_services memory project_milestones project_owner_events quota_latest quota_leases quota_observations quota_presence reviews sync_decisions sync_log sync_state task_comments task_events task_origins task_projects task_relations task_spawn_notices task_spawn_records task_thread_cursors task_thread_deliveries tasks ticket_sets`)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

type driftSchema struct {
	columns map[string]map[string]string
	unique  map[string][]string
	// named is each unique key's column list by table and index name.
	named map[string]map[string][]string
}

// PostgreSQL partial unique indexes whose MySQL counterpart is a functional
// unique key of the same name (see migration_mysql_tasks.go). Their column
// lists differ by construction (a WHERE clause against CASE expressions), so
// the check pairs them by table and name instead.
var mysqlFunctionalPartialKeys = map[string][]string{
	"tasks":        {"tasks_source_ref"},
	"task_events":  {"task_events_source_ref"},
	"task_origins": {"task_origins_youtrack_task", "task_origins_youtrack_external"},
}

func inspectDriftSchema(t *testing.T, s *Store) driftSchema {
	t.Helper()
	d := driftSchema{columns: map[string]map[string]string{}, unique: map[string][]string{}}
	schema := s.q(`current_schema()`, `DATABASE()`)
	rows, err := s.db().Query(t.Context(), `SELECT table_name,column_name,is_nullable FROM information_schema.columns WHERE table_schema=`+schema+` ORDER BY table_name,column_name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var table, column, nullable string
		if err := rows.Scan(&table, &column, &nullable); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if d.columns[table] == nil {
			d.columns[table] = map[string]string{}
		}
		d.columns[table][column] = nullable
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	rows, err = s.db().Query(t.Context(), s.q(
		`SELECT tbl.relname,idx.relname,pg_get_indexdef(i.indexrelid,k.n,true) || CASE WHEN i.indpred IS NULL THEN '' ELSE ' WHERE ' || pg_get_expr(i.indpred,i.indrelid) END FROM pg_index i JOIN pg_class tbl ON tbl.oid=i.indrelid JOIN pg_class idx ON idx.oid=i.indexrelid JOIN pg_namespace ns ON ns.oid=tbl.relnamespace CROSS JOIN LATERAL generate_series(1,i.indnkeyatts) AS k(n) WHERE ns.nspname=current_schema() AND i.indisunique ORDER BY tbl.relname,idx.relname,k.n`,
		`SELECT table_name,index_name,COALESCE(column_name,CONCAT('EXPRESSION:',expression)) FROM information_schema.statistics WHERE table_schema=DATABASE() AND non_unique=0 ORDER BY table_name,index_name,seq_in_index`))
	if err != nil {
		t.Fatal(err)
	}
	constraints := map[string]map[string][]string{}
	for rows.Next() {
		var table, name, column string
		if err := rows.Scan(&table, &name, &column); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if constraints[table] == nil {
			constraints[table] = map[string][]string{}
		}
		constraints[table][name] = append(constraints[table][name], column)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	d.named = constraints
	for table, keys := range constraints {
		for name, cols := range keys {
			if isFunctionalPartial(table, name) {
				d.unique[table] = append(d.unique[table], "partial("+name+")")
				continue
			}
			_ = name
			d.unique[table] = append(d.unique[table], strings.Join(cols, ","))
		}
		sort.Strings(d.unique[table])
	}
	return d
}

func isFunctionalPartial(table, name string) bool {
	for _, n := range mysqlFunctionalPartialKeys[table] {
		if n == name {
			return true
		}
	}
	return false
}

func TestMySQLSchemaDrift(t *testing.T) {
	pg := schemaStore(t, "mysql_drift")
	mysqlURL := testdb.MySQLDatabase(t)
	my, err := Open(t.Context(), mysqlURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(my.Close)
	p, m := inspectDriftSchema(t, pg), inspectDriftSchema(t, my)
	// MySQL's unique key admits multiple NULL system_event_key values,
	// equivalent to PostgreSQL's partial index.
	for i, key := range p.unique["task_comments"] {
		parts := strings.Split(key, ",")
		for j, part := range parts {
			parts[j], _, _ = strings.Cut(part, " WHERE ")
		}
		if strings.Join(parts, ",") == "task_id,system_event_key" {
			p.unique["task_comments"][i] = "task_id,system_event_key"
		}
	}
	sort.Strings(p.unique["task_comments"])
	// Transaction identity has dialect-specific types and names.
	delete(p.columns["sync_log"], "txid")
	delete(m.columns["sync_log"], "txn")
	for table := range p.columns {
		if !mysqlPorted[table] && !mysqlNotYetPorted[table] && !mysqlLeftOut[table] {
			t.Errorf("PostgreSQL table %s is not classified", table)
		}
	}
	for table := range m.columns {
		if !mysqlPorted[table] && table != "schema_migrations" && table != "hk_locks" && !mysqlStubs[table] {
			t.Errorf("unexpected MySQL table %s", table)
		}
	}
	for table := range mysqlPorted {
		pc, pok := p.columns[table]
		mc, mok := m.columns[table]
		if !pok || !mok {
			t.Errorf("%s: table missing (PostgreSQL=%t MySQL=%t)", table, pok, mok)
			continue
		}
		if !reflect.DeepEqual(pc, mc) {
			t.Errorf("%s: columns/nullability differ: PostgreSQL=%s MySQL=%s", table, formatColumns(pc), formatColumns(mc))
		}
		if !reflect.DeepEqual(p.unique[table], m.unique[table]) {
			t.Errorf("%s: unique keys differ: PostgreSQL=%v MySQL=%v", table, p.unique[table], m.unique[table])
		}
	}
}

func formatColumns(cols map[string]string) string {
	var entries []string
	for name, nullable := range cols {
		entries = append(entries, fmt.Sprintf("%s=%s", name, nullable))
	}
	sort.Strings(entries)
	return strings.Join(entries, ",")
}

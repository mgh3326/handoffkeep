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
var mysqlNotYetPorted = wordSet(`attachment_refs attachment_uploads attachment_usage attachments change_checks change_findings change_gate_events change_tasks change_threads changes chat_message_questions chat_messages chat_questions checkpoints decision_answer_attempts decision_deliveries decision_notify_digest decision_requests decision_resolutions deploy_record_projection deploy_services documents dots_results dots_run_tokens jobs ledger_cursors locks mcp_write_audit memory notification_outbox operator_inbox_events operator_inbox_state placement_decisions project_milestones project_owner_events relay_events reviews sync_decisions sync_log sync_state takes task_comments task_events task_origins task_projects task_relations task_spawn_notices task_spawn_records task_thread_cursors task_thread_deliveries tasks ticket_sets translations youtrack_projects yt_article_bindings yt_authority yt_binding_cursor yt_binding_history yt_binding_intents yt_comment_canary yt_comment_write_links yt_decision_bindings yt_entity_map yt_family_roots yt_feed_cursors yt_issue_comment_bounds yt_issue_intents yt_link_owned yt_link_plans yt_link_steps yt_link_unowned yt_links_sweep yt_snapshot_state yt_task_bindings yt_write_op_alerts yt_write_ops`)
var mysqlLeftOut = wordSet(`decision_notify deploy_changes deploys labels linear_board_owners linear_board_ownership_conflicts linear_board_tasks linear_capacity_state linear_create_admissions linear_decision_issues linear_decision_ops linear_issues linear_outbox schema_migrations schema_version services standing_orders status_updates task_labels task_state_migration task_subscribers verifications youtrack_attempts youtrack_comment_links youtrack_cursor youtrack_decision_mirrors youtrack_doc_articles youtrack_field_cache youtrack_outbox youtrack_processed_activities yt_write_op_adoptions`)
var mysqlPorted = wordSet(`bench_area_exceptions bench_catalog bench_grades bench_reps bench_scores quota_latest quota_leases quota_observations quota_presence`)

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
	rows, err = s.db().Query(t.Context(), `SELECT tc.table_name,tc.constraint_name,kcu.column_name FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu ON kcu.constraint_schema=tc.constraint_schema AND kcu.table_name=tc.table_name AND kcu.constraint_name=tc.constraint_name WHERE tc.table_schema=`+schema+` AND tc.constraint_type IN ('PRIMARY KEY','UNIQUE') ORDER BY tc.table_name,tc.constraint_name,kcu.ordinal_position`)
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
	for table, keys := range constraints {
		for _, cols := range keys {
			d.unique[table] = append(d.unique[table], strings.Join(cols, ","))
		}
		sort.Strings(d.unique[table])
	}
	return d
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
	for table := range p.columns {
		if !mysqlPorted[table] && !mysqlNotYetPorted[table] && !mysqlLeftOut[table] {
			t.Errorf("PostgreSQL table %s is not classified", table)
		}
	}
	for table := range m.columns {
		if !mysqlPorted[table] && table != "schema_migrations" {
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

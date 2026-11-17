package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/mgh3326/handoffkeep/internal/mysqlurl"
)

// CopyTable is the copy contract for one table: column order is read from the
// schema's ordinal positions and then fixed for both hashing and insertion.
type CopyTable struct {
	Name       string `json:"name"`
	SourceRows int64  `json:"source_rows"`
	TargetRows int64  `json:"target_rows"`
	SourceHash string `json:"source_sha256"`
	TargetHash string `json:"target_sha256"`
	Match      bool   `json:"match"`
}

// CopyReport reports copied and intentionally skipped schema tables.
type CopyReport struct {
	From    string      `json:"from"`
	To      string      `json:"to"`
	Tables  []CopyTable `json:"tables"`
	Skipped []string    `json:"skipped"`
	Match   bool        `json:"match"`
}

type copyTableSpec struct {
	name    string
	columns []string
	order   []string
	bounds  map[string]int
}

// Ported copy registry. Keep quota_observations before quota_latest because
// quota_latest references it; areas add their tables here when they port.
var copyTableRegistry = []copyTableSpec{
	{name: "bench_area_exceptions", columns: strings.Fields("profile effort area action evidence_refs decision_ref decided_by decided_at expires_with_model_id retired_at"), order: []string{"profile", "effort", "area"}, bounds: map[string]int{"profile": 200, "effort": 200, "area": 200, "action": 16}},
	{name: "bench_catalog", columns: strings.Fields("profile effort model_id pool grade score gate gate_reason benchmark_source benchmark_annotation boundary_version deviation_ref decided_at decided_by retired_at"), order: []string{"profile", "effort"}, bounds: map[string]int{"profile": 200, "effort": 200, "grade": 16, "gate": 32}},
	{name: "bench_grades", columns: strings.Fields("profile grade boundary_version deviation_ref decided_at decided_by"), order: []string{"profile"}, bounds: map[string]int{"profile": 200, "grade": 16}},
	{name: "bench_reps", columns: strings.Fields("id origin_id profile model_id task_ref tier role rounds blockers_found completed input_tokens output_tokens notes recorded_at effort grade table_grade created_by created_at experiment_id arm first_ci_green critical_findings high_findings merged reverted_within_7d reviewer_profile reviewer_effort reasoning_tokens cache_read_tokens cache_write_tokens spawn_to_ready_s spawn_to_merge_s"), order: []string{"id"}, bounds: map[string]int{"created_by": 128}},
	{name: "bench_scores", columns: strings.Fields("model_id effort harness source metric score rank captured_at time_per_task_min cost_per_task_usd provenance updated_by updated_at"), order: []string{"model_id", "effort", "harness", "source", "metric"}, bounds: map[string]int{"model_id": 128, "effort": 128, "harness": 128, "source": 128, "metric": 128}},
	{name: "quota_observations", columns: strings.Fields("id account_key kind window_key measured_at value received_at"), order: []string{"id"}, bounds: map[string]int{"account_key": 64, "kind": 16, "window_key": 16}},
	{name: "quota_latest", columns: strings.Fields("account_key kind window_key observation_id measured_at confirmed_at"), order: []string{"account_key", "kind", "window_key"}, bounds: map[string]int{"account_key": 64, "kind": 16, "window_key": 16}},
	{name: "quota_leases", columns: strings.Fields("account_key owner holder expires_at"), order: []string{"account_key"}, bounds: map[string]int{"account_key": 64}},
	{name: "quota_presence", columns: strings.Fields("host_id account_key last_seen"), order: []string{"host_id", "account_key"}, bounds: map[string]int{"host_id": 64, "account_key": 64}},
}

// StoreCopyTables returns the currently ported tables in dependency order.
func StoreCopyTables() []string {
	out := make([]string, len(copyTableRegistry))
	for i, t := range copyTableRegistry {
		out[i] = t.name
	}
	return out
}

var copyLeftOutTables = strings.Fields(`attachment_refs attachment_uploads attachment_usage attachments change_checks change_findings change_gate_events change_tasks change_threads changes chat_message_questions chat_messages chat_questions checkpoints decision_answer_attempts decision_deliveries decision_notify decision_notify_digest decision_requests decision_resolutions deploy_changes deploy_record_projection deploy_services deploys documents dots_results dots_run_tokens jobs labels ledger_cursors linear_board_owners linear_board_ownership_conflicts linear_board_tasks linear_capacity_state linear_create_admissions linear_decision_issues linear_decision_ops linear_issues linear_outbox locks mcp_write_audit memory notification_outbox operator_inbox_events operator_inbox_state placement_decisions project_milestones project_owner_events relay_events relay_events_retired_20260918 reviews schema_migrations schema_version services standing_orders status_updates sync_decisions sync_log sync_state takes task_comments task_events task_labels task_origins task_projects task_relations task_spawn_notices task_spawn_records task_state_migration task_subscribers task_thread_cursors task_thread_deliveries tasks ticket_sets translations verifications youtrack_attempts youtrack_comment_links youtrack_cursor youtrack_decision_mirrors youtrack_doc_articles youtrack_field_cache youtrack_outbox youtrack_processed_activities yt_article_bindings yt_authority yt_binding_cursor yt_binding_history yt_binding_intents yt_comment_canary yt_comment_write_links yt_decision_bindings yt_entity_map yt_family_roots yt_feed_cursors yt_issue_comment_bounds yt_issue_intents yt_link_owned yt_link_plans yt_link_steps yt_link_unowned yt_links_sweep yt_snapshot_state yt_task_bindings yt_write_op_adoptions yt_write_op_alerts yt_write_ops`)

type copyDB struct {
	db    *sql.DB
	conn  *sql.Conn
	mySQL bool
}

func openCopyDB(ctx context.Context, raw string) (*copyDB, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	var driver, dsn string
	my := u.Scheme == "mysql"
	switch u.Scheme {
	case "mysql":
		cfg, e := mysqlurl.Parse(raw)
		if e != nil {
			return nil, e
		}
		driver, dsn = "mysql", cfg.FormatDSN()
	case "postgres", "postgresql":
		driver, dsn = "pgx", raw
	default:
		return nil, errors.New("database URL must use postgres:// or mysql://")
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	c := &copyDB{db: db, mySQL: my}
	return c, nil
}

func (d *copyDB) close() {
	if d.conn != nil {
		_ = d.conn.Close()
	}
	_ = d.db.Close()
}
func (d *copyDB) session(ctx context.Context) (*sql.Conn, error) {
	if d.conn != nil {
		return d.conn, nil
	}
	c, err := d.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	d.conn = c
	return c, nil
}

// CopyStore copies the currently ported tables in either direction. It does
// not migrate schemas or truncate destination tables. Table writes are atomic.
func CopyStore(ctx context.Context, fromURL, toURL string, selected []string, verifyOnly bool) (CopyReport, error) {
	report := CopyReport{From: schemeOf(fromURL), To: schemeOf(toURL), Match: true, Skipped: append([]string(nil), copyLeftOutTables...)}
	sort.Strings(report.Skipped)
	from, err := openCopyDB(ctx, fromURL)
	if err != nil {
		return report, err
	}
	defer from.close()
	to, err := openCopyDB(ctx, toURL)
	if err != nil {
		return report, err
	}
	defer to.close()
	if from.mySQL == to.mySQL {
		return report, errors.New("store-copy requires one PostgreSQL and one MySQL URL")
	}
	allowed := map[string]bool{}
	for _, name := range StoreCopyTables() {
		allowed[name] = true
	}
	if len(selected) == 0 {
		selected = StoreCopyTables()
	}
	wanted := map[string]bool{}
	for _, name := range selected {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !allowed[name] {
			return report, fmt.Errorf("table %q is not in the ported copy registry", name)
		}
		wanted[name] = true
	}
	for _, spec := range copyTableRegistry {
		if !wanted[spec.name] {
			continue
		}
		cols, err := copyColumns(ctx, from.db, from.mySQL, spec.name)
		if err != nil {
			return report, fmt.Errorf("%s source schema: %w", spec.name, err)
		}
		targetCols, err := copyColumns(ctx, to.db, to.mySQL, spec.name)
		if err != nil {
			return report, fmt.Errorf("%s target schema: %w", spec.name, err)
		}
		if !sameCopyColumns(cols, spec.columns) || !sameCopyColumns(targetCols, spec.columns) {
			return report, fmt.Errorf("%s schema columns differ: source=%v target=%v", spec.name, cols, targetCols)
		}
		cols = spec.columns
		var srcCount int64
		srcHash, err := checksum(ctx, from.db, from.mySQL, spec, cols, func(row []any) error {
			srcCount++
			for i, col := range cols {
				if max := spec.bounds[col]; max > 0 {
					if s, ok := row[i].(string); ok && utf8.RuneCountInString(s) > max {
						return fmt.Errorf("refusing %s.%s value of %d characters; MySQL limit is %d", spec.name, col, utf8.RuneCountInString(s), max)
					}
				}
			}
			return nil
		})
		if err != nil {
			return report, fmt.Errorf("%s source read: %w", spec.name, err)
		}
		if !verifyOnly {
			if err := copyRows(ctx, from.db, to, from.mySQL, spec, cols); err != nil {
				return report, fmt.Errorf("%s copy: %w", spec.name, err)
			}
			if err := resetIdentity(ctx, to, spec.name); err != nil {
				return report, fmt.Errorf("%s identity: %w", spec.name, err)
			}
		}
		tgtCount := int64(0)
		tgtHash, err := checksum(ctx, to.db, to.mySQL, spec, cols, func([]any) error { tgtCount++; return nil })
		if err != nil {
			return report, fmt.Errorf("%s target read: %w", spec.name, err)
		}
		row := CopyTable{Name: spec.name, SourceRows: srcCount, TargetRows: tgtCount, SourceHash: srcHash, TargetHash: tgtHash, Match: srcCount == tgtCount && srcHash == tgtHash}
		report.Tables = append(report.Tables, row)
		report.Match = report.Match && row.Match
	}
	return report, nil
}

func sameCopyColumns(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	a, e := append([]string(nil), actual...), append([]string(nil), expected...)
	sort.Strings(a)
	sort.Strings(e)
	return strings.Join(a, "\x00") == strings.Join(e, "\x00")
}

func schemeOf(raw string) string {
	u, _ := url.Parse(raw)
	if u == nil {
		return ""
	}
	return u.Scheme
}

func copyColumns(ctx context.Context, db *sql.DB, mysqlDB bool, table string) ([]string, error) {
	q := `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 ORDER BY ordinal_position`
	if mysqlDB {
		q = `SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? ORDER BY ordinal_position`
	}
	rows, err := db.QueryContext(ctx, q, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table does not exist")
	}
	return cols, nil
}

func quoteCopyIdent(s string, mysqlDB bool) string {
	if mysqlDB {
		return "`" + strings.ReplaceAll(s, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func checksum(ctx context.Context, db *sql.DB, mysqlDB bool, spec copyTableSpec, cols []string, each func([]any) error) (string, error) {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteCopyIdent(c, mysqlDB)
	}
	order := make([]string, len(spec.order))
	for i, c := range spec.order {
		order[i] = quoteCopyIdent(c, mysqlDB)
	}
	q := `SELECT ` + strings.Join(quoted, ",") + ` FROM ` + quoteCopyIdent(spec.name, mysqlDB) + ` ORDER BY ` + strings.Join(order, ",")
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		vals := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return "", err
		}
		for i := range vals {
			vals[i] = canonicalCopyValue(vals[i], cols[i] == "value" || cols[i] == "evidence_refs")
		}
		b, err := json.Marshal(vals)
		if err != nil {
			return "", err
		}
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{'\n'})
		if err := each(vals); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func canonicalCopyValue(v any, isJSON bool) any {
	switch x := v.(type) {
	case []byte:
		if !isJSON {
			return string(x)
		}
		var value any
		dec := json.NewDecoder(strings.NewReader(string(x)))
		dec.UseNumber()
		if dec.Decode(&value) == nil {
			if b, err := json.Marshal(value); err == nil {
				return json.RawMessage(b)
			}
		}
		return string(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}

func copyRows(ctx context.Context, src *sql.DB, dst *copyDB, sourceMySQL bool, spec copyTableSpec, cols []string) error {
	conn, err := dst.session(ctx)
	if err != nil {
		return err
	}
	if dst.mySQL {
		if _, err := conn.ExecContext(ctx, `SET @hk_bulk_load = 1`); err != nil {
			return fmt.Errorf("set MySQL bulk-load session flag: %w", err)
		}
	} else if _, err := conn.ExecContext(ctx, `SET session_replication_role = replica`); err != nil {
		return fmt.Errorf("set PostgreSQL replication role: %w", err)
	}
	defer func() {
		if dst.mySQL {
			_, _ = conn.ExecContext(context.Background(), `SET @hk_bulk_load = NULL`)
		} else {
			_, _ = conn.ExecContext(context.Background(), `SET session_replication_role = DEFAULT`)
		}
	}()
	qt := quoteCopyIdent(spec.name, sourceMySQL)
	qcols := make([]string, len(cols))
	for i, c := range cols {
		qcols[i] = quoteCopyIdent(c, sourceMySQL)
	}
	order := make([]string, len(spec.order))
	for i, c := range spec.order {
		order[i] = quoteCopyIdent(c, sourceMySQL)
	}
	rows, err := src.QueryContext(ctx, `SELECT `+strings.Join(qcols, ",")+` FROM `+qt+` ORDER BY `+strings.Join(order, ","))
	if err != nil {
		return err
	}
	defer rows.Close()
	placeholders := make([]string, len(cols))
	for i := range placeholders {
		placeholders[i] = "?"
		if !dst.mySQL {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
	}
	dqcols := make([]string, len(cols))
	for i, c := range cols {
		dqcols[i] = quoteCopyIdent(c, dst.mySQL)
	}
	insert := `INSERT INTO ` + quoteCopyIdent(spec.name, dst.mySQL) + ` (` + strings.Join(dqcols, ",") + `) VALUES (` + strings.Join(placeholders, ",") + `)`
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for rows.Next() {
		values := make([]any, len(cols))
		dests := make([]any, len(cols))
		for i := range values {
			dests[i] = &values[i]
		}
		if err := rows.Scan(dests...); err != nil {
			return err
		}
		for i, value := range values {
			values[i] = copyInsertValue(value, cols[i] == "value" || cols[i] == "evidence_refs")
		}
		if _, err := tx.ExecContext(ctx, insert, values...); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

func copyInsertValue(v any, isJSON bool) any {
	if b, ok := v.([]byte); ok {
		if !isJSON {
			return string(b)
		}
		var value any
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		if dec.Decode(&value) == nil {
			if encoded, err := json.Marshal(value); err == nil {
				return string(encoded)
			}
		}
		return string(b)
	}
	if t, ok := v.(time.Time); ok {
		return t.UTC()
	}
	return v
}

func resetIdentity(ctx context.Context, dst *copyDB, table string) error {
	conn, err := dst.session(ctx)
	if err != nil {
		return err
	}
	if table != "bench_reps" && table != "quota_observations" {
		return nil
	}
	if dst.mySQL {
		var max sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT MAX(id) FROM `+quoteCopyIdent(table, true)).Scan(&max); err != nil {
			return err
		}
		if max.Valid {
			_, err = conn.ExecContext(ctx, `ALTER TABLE `+quoteCopyIdent(table, true)+` AUTO_INCREMENT = `+strconv.FormatInt(max.Int64+1, 10))
		}
		return err
	}
	var seq sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1,'id')`, table).Scan(&seq); err != nil {
		return err
	}
	if !seq.Valid || seq.String == "" {
		return nil
	}
	_, err = conn.ExecContext(ctx, `SELECT setval($1, COALESCE((SELECT MAX(id) FROM `+quoteCopyIdent(table, false)+`), 1), EXISTS(SELECT 1 FROM `+quoteCopyIdent(table, false)+`))`, seq.String)
	return err
}

var _ hash.Hash = sha256.New()

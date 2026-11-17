package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgtype"
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

type copyValueKind uint8

const (
	copyText copyValueKind = iota
	copyInteger
	copyFloat
	copyBoolean
	copyTime
	copyJSON
	copyTextArray
)

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

func prepareCopySession(ctx context.Context, dst *copyDB) (func(), error) {
	conn, err := dst.session(ctx)
	if err != nil {
		return nil, err
	}
	if dst.mySQL {
		if _, err := conn.ExecContext(ctx, `SET @hk_bulk_load = 1`); err != nil {
			return nil, fmt.Errorf("MySQL copy destination must use the copy admin account: %w", err)
		}
		return func() { _, _ = conn.ExecContext(context.Background(), `SET @hk_bulk_load = NULL`) }, nil
	}
	if _, err := conn.ExecContext(ctx, `SET session_replication_role = replica`); err != nil {
		return nil, fmt.Errorf("PostgreSQL copy destination must allow SET session_replication_role = replica (use a superuser such as postgres): %w", err)
	}
	return func() { _, _ = conn.ExecContext(context.Background(), `SET session_replication_role = DEFAULT`) }, nil
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
	if !verifyOnly && len(wanted) > 0 {
		cleanup, err := prepareCopySession(ctx, to)
		if err != nil {
			return report, err
		}
		defer cleanup()
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
		sourceKinds, err := copyColumnKinds(ctx, from.db, from.mySQL, spec.name)
		if err != nil {
			return report, fmt.Errorf("%s source column types: %w", spec.name, err)
		}
		targetKinds, err := copyColumnKinds(ctx, to.db, to.mySQL, spec.name)
		if err != nil {
			return report, fmt.Errorf("%s target column types: %w", spec.name, err)
		}
		for _, col := range spec.columns {
			if !compatibleCopyKind(spec.name, col, sourceKinds[col], targetKinds[col]) {
				return report, fmt.Errorf("%s.%s incompatible source and target column types", spec.name, col)
			}
		}
		cols = spec.columns
		var srcCount int64
		srcHash, err := checksum(ctx, from.db, from.mySQL, spec, cols, sourceKinds, func(row []any) error {
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
			if err := copyRows(ctx, from.db, to, from.mySQL, spec, cols, sourceKinds); err != nil {
				return report, fmt.Errorf("%s copy: %w", spec.name, err)
			}
			if err := resetIdentity(ctx, to, spec.name); err != nil {
				return report, fmt.Errorf("%s identity: %w", spec.name, err)
			}
		}
		tgtCount := int64(0)
		tgtKinds, err := copyColumnKinds(ctx, to.db, to.mySQL, spec.name)
		if err != nil {
			return report, fmt.Errorf("%s target column types: %w", spec.name, err)
		}
		tgtHash, err := checksum(ctx, to.db, to.mySQL, spec, cols, tgtKinds, func([]any) error { tgtCount++; return nil })
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

func copyColumnKinds(ctx context.Context, db *sql.DB, mysqlDB bool, table string) (map[string]copyValueKind, error) {
	q := `SELECT column_name,data_type,udt_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1`
	if mysqlDB {
		q = `SELECT column_name,data_type,column_type FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=?`
	}
	rows, err := db.QueryContext(ctx, q, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kinds := map[string]copyValueKind{}
	for rows.Next() {
		var name, dataType, detail string
		if err := rows.Scan(&name, &dataType, &detail); err != nil {
			return nil, err
		}
		kind, err := classifyCopyColumn(mysqlDB, dataType, detail)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", name, err)
		}
		kinds[name] = kind
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return kinds, nil
}

func classifyCopyColumn(mysqlDB bool, dataType, detail string) (copyValueKind, error) {
	if mysqlDB {
		switch strings.ToLower(dataType) {
		case "json":
			return copyJSON, nil
		case "datetime", "timestamp", "date":
			return copyTime, nil
		case "tinyint":
			if strings.EqualFold(detail, "tinyint(1)") {
				return copyBoolean, nil
			}
			return copyInteger, nil
		case "smallint", "mediumint", "int", "integer", "bigint":
			return copyInteger, nil
		case "float", "double", "real", "decimal":
			return copyFloat, nil
		case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
			return copyText, nil
		default:
			return 0, fmt.Errorf("unsupported MySQL type %q", dataType)
		}
	}
	switch strings.ToLower(dataType) {
	case "boolean":
		return copyBoolean, nil
	case "smallint", "integer", "bigint":
		return copyInteger, nil
	case "real", "double precision", "numeric":
		return copyFloat, nil
	case "timestamp with time zone", "timestamp without time zone", "date", "time without time zone", "time with time zone":
		return copyTime, nil
	case "json", "jsonb":
		return copyJSON, nil
	case "array":
		if detail == "_text" {
			return copyTextArray, nil
		}
		return 0, fmt.Errorf("unsupported PostgreSQL array type %q", detail)
	case "character", "character varying", "text", "USER-DEFINED":
		return copyText, nil
	default:
		return 0, fmt.Errorf("unsupported PostgreSQL type %q", dataType)
	}
}

func compatibleCopyKind(table, column string, source, target copyValueKind) bool {
	if source == target {
		return true
	}
	return table == "bench_area_exceptions" && column == "evidence_refs" &&
		(source == copyTextArray && target == copyJSON || source == copyJSON && target == copyTextArray)
}

func quoteCopyIdent(s string, mysqlDB bool) string {
	if mysqlDB {
		return "`" + strings.ReplaceAll(s, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func checksum(ctx context.Context, db *sql.DB, mysqlDB bool, spec copyTableSpec, cols []string, kinds map[string]copyValueKind, each func([]any) error) (string, error) {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteCopyIdent(c, mysqlDB)
	}
	q := `SELECT ` + strings.Join(quoted, ",") + ` FROM ` + quoteCopyIdent(spec.name, mysqlDB)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	rowHashes := make([][sha256.Size]byte, 0)
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
			value, err := canonicalCopyValue(vals[i], spec.name, cols[i], kinds[cols[i]], mysqlDB)
			if err != nil {
				return "", fmt.Errorf("normalize %s.%s: %w", spec.name, cols[i], err)
			}
			vals[i] = value
		}
		b, err := json.Marshal(vals)
		if err != nil {
			return "", err
		}
		rowHashes = append(rowHashes, sha256.Sum256(b))
		if err := each(vals); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return checksumRowHashes(rowHashes), nil
}

func checksumRowHashes(rowHashes [][sha256.Size]byte) string {
	sort.Slice(rowHashes, func(i, j int) bool {
		return bytes.Compare(rowHashes[i][:], rowHashes[j][:]) < 0
	})
	h := sha256.New()
	for _, rowHash := range rowHashes {
		_, _ = h.Write(rowHash[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalCopyValue(v any, table, column string, kind copyValueKind, mysqlSource bool) (any, error) {
	if table == "bench_area_exceptions" && column == "evidence_refs" {
		var refs []string
		var err error
		if mysqlSource {
			refs, err = parseJSONTextArray(v)
		} else {
			refs, err = parsePostgresTextArray(v)
		}
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(refs)
		return json.RawMessage(b), err
	}
	value, err := normalizeCopyValue(v, kind)
	if err != nil {
		return nil, err
	}
	if kind == copyTime {
		if t, ok := value.(time.Time); ok {
			return t.UTC().Format(time.RFC3339Nano), nil
		}
	}
	return value, nil
}

func copyRows(ctx context.Context, src *sql.DB, dst *copyDB, sourceMySQL bool, spec copyTableSpec, cols []string, kinds map[string]copyValueKind) error {
	conn, err := dst.session(ctx)
	if err != nil {
		return err
	}
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
			values[i], err = copyInsertValue(value, spec.name, cols[i], kinds[cols[i]], sourceMySQL, dst.mySQL)
			if err != nil {
				return fmt.Errorf("convert %s.%s: %w", spec.name, cols[i], err)
			}
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

func copyInsertValue(v any, table, column string, kind copyValueKind, sourceMySQL, targetMySQL bool) (any, error) {
	if table == "bench_area_exceptions" && column == "evidence_refs" {
		var refs []string
		var err error
		if sourceMySQL {
			refs, err = parseJSONTextArray(v)
		} else {
			refs, err = parsePostgresTextArray(v)
		}
		if err != nil {
			return nil, err
		}
		if targetMySQL {
			b, err := json.Marshal(refs)
			return string(b), err
		}
		return pgtype.FlatArray[string](refs), nil
	}
	value, err := normalizeCopyValue(v, kind)
	if err != nil {
		return nil, err
	}
	if kind == copyJSON {
		return string(value.(json.RawMessage)), nil
	}
	return value, nil
}

func normalizeCopyValue(v any, kind copyValueKind) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch kind {
	case copyText:
		if b, ok := v.([]byte); ok {
			return string(b), nil
		}
		return v, nil
	case copyInteger:
		switch x := v.(type) {
		case int:
			return int64(x), nil
		case int16:
			return int64(x), nil
		case int32:
			return int64(x), nil
		case int64:
			return x, nil
		case uint64:
			return int64(x), nil
		case []byte:
			return parseCopyInt(string(x))
		case string:
			return parseCopyInt(x)
		default:
			return nil, fmt.Errorf("cannot normalize %T as integer", v)
		}
	case copyFloat:
		switch x := v.(type) {
		case float32:
			return float64(x), nil
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		case []byte:
			return parseCopyFloat(string(x))
		case string:
			return parseCopyFloat(x)
		default:
			return nil, fmt.Errorf("cannot normalize %T as float", v)
		}
	case copyBoolean:
		switch x := v.(type) {
		case bool:
			return x, nil
		case int64:
			if x == 0 {
				return false, nil
			}
			if x == 1 {
				return true, nil
			}
		case []byte:
			return parseCopyBool(string(x))
		case string:
			return parseCopyBool(x)
		}
		return nil, fmt.Errorf("cannot normalize %T as boolean", v)
	case copyTime:
		if t, ok := v.(time.Time); ok {
			return t.UTC(), nil
		}
		return nil, fmt.Errorf("cannot normalize %T as time", v)
	case copyJSON:
		var data []byte
		switch x := v.(type) {
		case []byte:
			data = x
		case string:
			data = []byte(x)
		case json.RawMessage:
			data = x
		default:
			return nil, fmt.Errorf("cannot normalize %T as JSON", v)
		}
		var value any
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.UseNumber()
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		b, err := json.Marshal(value)
		return json.RawMessage(b), err
	case copyTextArray:
		return parsePostgresTextArray(v)
	default:
		return nil, fmt.Errorf("unknown copy value kind %d", kind)
	}
}

func parseCopyInt(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func parseCopyFloat(s string) (float64, error) {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func parseCopyBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value %q", s)
	}
}

func parsePostgresTextArray(v any) ([]string, error) {
	switch x := v.(type) {
	case []string:
		return x, nil
	case pgtype.FlatArray[string]:
		return []string(x), nil
	case string:
		return decodePostgresTextArray([]byte(x))
	case []byte:
		return decodePostgresTextArray(x)
	default:
		return nil, fmt.Errorf("cannot decode PostgreSQL text array from %T", v)
	}
}

func decodePostgresTextArray(data []byte) ([]string, error) {
	var values pgtype.FlatArray[string]
	plan := pgtype.NewMap().PlanScan(pgtype.TextArrayOID, pgtype.TextFormatCode, &values)
	if plan == nil {
		return nil, errors.New("PostgreSQL text array decoder unavailable")
	}
	if err := plan.Scan(data, &values); err != nil {
		return nil, err
	}
	return []string(values), nil
}

func parseJSONTextArray(v any) ([]string, error) {
	var data []byte
	switch x := v.(type) {
	case []byte:
		data = x
	case string:
		data = []byte(x)
	default:
		return nil, fmt.Errorf("cannot decode MySQL JSON array from %T", v)
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return values, nil
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

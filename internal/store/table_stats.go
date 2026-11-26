package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// MySQL table statistics (hk 6231, operator decision 489). Once a day the
// server reads, for every base table of the current MySQL schema, an exact
// COUNT(*) and the engine's TABLE_ROWS, DATA_LENGTH and INDEX_LENGTH. Only
// numbers and table names are read; no row content leaves the database.
//
// The snapshots live in one report document, TableStatsDocKey. Its body has a
// Markdown summary for people on top and the whole history below it, in one
// JSON line inside a fenced block, so growth survives a restart and the
// document is the only store (no table, no migration). PostgreSQL has no such
// snapshot: RunTableStats returns without reading anything there.

// TableStatsDocKey is the report document the snapshots are written to.
const TableStatsDocKey = "tbls/oci/mysql/handoffkeep/stats"

const (
	// TableStatsHour is the UTC hour of the daily snapshot.
	TableStatsHour = 3
	// TableStatsMaxAge is how old the newest snapshot may be before one is
	// taken whatever the hour, so a redeploy does not skip a day.
	TableStatsMaxAge = 24 * time.Hour
	// TableStatsGrowthRef is the look-back of the long growth column.
	TableStatsGrowthRef = 7 * 24 * time.Hour
	// TableStatsHistoryMax caps the snapshots kept (about two months daily).
	TableStatsHistoryMax = 60
	// TableStatsBodyBudget keeps the rendered body well under
	// DocumentMaxBytes; the oldest snapshots are dropped past it.
	TableStatsBodyBudget = 768 << 10
	// TableStatsActor is the created_by of the snapshot document.
	TableStatsActor = "table-stats-worker"

	tableStatsVersion = 1
	tableStatsFence   = "```json table-stats-history"
	tableStatsDayFmt  = "2006-01-02"
)

// TableStatsRun reports one RunTableStats call. Ran is false when no snapshot
// was due or the store is not MySQL; Tables and Elapsed describe the read when
// it ran.
type TableStatsRun struct {
	Ran     bool
	Tables  int
	Elapsed time.Duration
}

// tableStatRecord is one snapshot. Tables maps a base table name to
// [rows_exact, rows_estimate, data_bytes, index_bytes].
type tableStatRecord struct {
	At     time.Time           `json:"at"`
	ReadMS int64               `json:"read_ms"`
	Fresh  bool                `json:"fresh"`
	Tables map[string][4]int64 `json:"tables"`
}

// tableStatsHistory is the machine-readable body block (format v1).
type tableStatsHistory struct {
	V         int               `json:"v"`
	Snapshots []tableStatRecord `json:"snapshots"`
}

// TableStatsDue reports whether a snapshot should be taken at now: when the
// document has no snapshot yet, when the newest is at least TableStatsMaxAge
// old, or when the UTC hour has reached TableStatsHour on a later UTC day
// than the newest snapshot.
func TableStatsDue(now time.Time, newest time.Time, found bool) bool {
	now = now.UTC()
	if !found {
		return true
	}
	if now.Sub(newest.UTC()) >= TableStatsMaxAge {
		return true
	}
	return now.Hour() >= TableStatsHour && newest.UTC().Format(tableStatsDayFmt) != now.Format(tableStatsDayFmt)
}

// RunTableStats takes the snapshot if one is due at now and writes the
// document. It reads the history from the document first, so a restart never
// repeats or skips a day. On PostgreSQL it returns at once with Ran false.
func (s *Store) RunTableStats(ctx context.Context, now time.Time) (TableStatsRun, error) {
	if s.dialect != mysqlDialect {
		return TableStatsRun{}, nil
	}
	now = now.UTC().Truncate(time.Second)
	var result TableStatsRun
	err := s.retryTx(ctx, func() error {
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if err = s.lockWorkerOperationTx(ctx, tx, "table_stats"); err != nil {
			return err
		}
		// Read after acquiring the lock: another worker generation may have
		// completed the daily snapshot while this call waited.
		var docBody string
		err = tx.QueryRow(ctx, s.q(`SELECT body FROM documents WHERE key=$1`, "SELECT body FROM documents WHERE `key`=?"), TableStatsDocKey).Scan(&docBody)
		found := err == nil
		if err != nil && !errors.Is(err, ErrNoRows) {
			return fmt.Errorf("table stats document: %w", err)
		}
		hist, err := parseTableStatsDoc(docBody, found)
		if err != nil {
			return err
		}
		var newest time.Time
		if n := len(hist.Snapshots); n > 0 {
			newest = hist.Snapshots[n-1].At
		}
		if !TableStatsDue(now, newest, len(hist.Snapshots) > 0) {
			return tx.Commit(ctx)
		}
		read, err := s.readTableStatsMySQLOnce(ctx)
		if err != nil {
			return fmt.Errorf("table stats read: %w", err)
		}
		hist.Snapshots = append(hist.Snapshots, tableStatRecord{
			At: now, ReadMS: read.elapsed.Milliseconds(), Fresh: read.fresh, Tables: read.tables,
		})
		body, err := fitTableStatsDoc(hist)
		if err != nil {
			return err
		}
		if err = putTableStatsDocumentTx(ctx, s, tx, body); err != nil {
			return fmt.Errorf("table stats write: %w", err)
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		result = TableStatsRun{Ran: true, Tables: len(read.tables), Elapsed: read.elapsed}
		return nil
	})
	return result, err
}

func putTableStatsDocumentTx(ctx context.Context, s *Store, tx transaction, body string) error {
	if !ValidDocKey(TableStatsDocKey) || !documentKinds["report"] || !ValidActor(TableStatsActor) || !validDocumentText(body) {
		return errors.New("invalid table stats document")
	}
	if len(body) > DocumentMaxBytes {
		return fmt.Errorf("%w: body is %d bytes; the limit is %d", ErrDocumentTooLarge, len(body), DocumentMaxBytes)
	}
	if err := guard.Reject(TableStatsDocKey + "\n" + body); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(body))
	now := time.Now().UTC()
	_, err := tx.Exec(ctx, s.q(
		`INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at) VALUES($1,'report','','',$2,$3,$4,$5,$6) ON CONFLICT(key) DO UPDATE SET kind=EXCLUDED.kind,session=EXCLUDED.session,job=EXCLUDED.job,body=EXCLUDED.body,sha256=EXCLUDED.sha256,created_by=EXCLUDED.created_by,updated_at=EXCLUDED.updated_at`,
		"INSERT INTO documents(`key`,kind,session,job,body,sha256,created_by,created_at,updated_at) VALUES(?,'report','','',?,?,?,?,?) ON DUPLICATE KEY UPDATE kind=VALUES(kind),session=VALUES(session),job=VALUES(job),body=VALUES(body),sha256=VALUES(sha256),created_by=VALUES(created_by),updated_at=VALUES(updated_at)"),
		TableStatsDocKey, body, hex.EncodeToString(sum[:]), TableStatsActor, now, now)
	return documentBodyWriteError(err)
}

// parseTableStatsDoc reads the history block of the document body. A missing
// document is an empty history. A body without a well-formed block is an
// error, never an empty history: rewriting it would discard the snapshots.
func parseTableStatsDoc(body string, found bool) (tableStatsHistory, error) {
	h := tableStatsHistory{V: tableStatsVersion}
	if !found {
		return h, nil
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if line != tableStatsFence {
			continue
		}
		if i+2 >= len(lines) || lines[i+2] != "```" {
			return h, errors.New("table stats history block is not closed")
		}
		if err := json.Unmarshal([]byte(lines[i+1]), &h); err != nil {
			return h, fmt.Errorf("table stats history: %w", err)
		}
		if h.V != tableStatsVersion {
			return h, fmt.Errorf("table stats history version %d is not supported", h.V)
		}
		sort.SliceStable(h.Snapshots, func(a, b int) bool { return h.Snapshots[a].At.Before(h.Snapshots[b].At) })
		return h, nil
	}
	return h, errors.New("table stats document has no history block")
}

// tableStatsReference is the newest snapshot at or before cur minus
// TableStatsGrowthRef, the comparison for the long growth column.
func tableStatsReference(snaps []tableStatRecord, cur time.Time) (tableStatRecord, bool) {
	cutoff := cur.Add(-TableStatsGrowthRef)
	for i := len(snaps) - 1; i >= 0; i-- {
		if !snaps[i].At.After(cutoff) {
			return snaps[i], true
		}
	}
	return tableStatRecord{}, false
}

// fitTableStatsDoc keeps the newest TableStatsHistoryMax snapshots and drops
// the oldest while the body would exceed TableStatsBodyBudget, then renders
// it. The newest snapshot is never dropped. h is changed in place.
func fitTableStatsDoc(h tableStatsHistory) (string, error) {
	h.V = tableStatsVersion
	if len(h.Snapshots) > TableStatsHistoryMax {
		h.Snapshots = h.Snapshots[len(h.Snapshots)-TableStatsHistoryMax:]
	}
	body, err := renderTableStatsDoc(h)
	for err == nil && len(body) > TableStatsBodyBudget && len(h.Snapshots) > 1 {
		h.Snapshots = h.Snapshots[1:]
		body, err = renderTableStatsDoc(h)
	}
	return body, err
}

// renderTableStatsDoc writes the document body: a Markdown summary of the
// newest snapshot with its growth against the previous one and against the
// reference about seven days earlier, then the history block. Growth is a pure
// function of the history, so it is the same after any restart.
func renderTableStatsDoc(h tableStatsHistory) (string, error) {
	n := len(h.Snapshots)
	if n == 0 {
		return "", errors.New("table stats: no snapshot to render")
	}
	cur := h.Snapshots[n-1]
	var prev, ref *tableStatRecord
	if n >= 2 {
		prev = &h.Snapshots[n-2]
	}
	if r, ok := tableStatsReference(h.Snapshots, cur.At); ok {
		ref = &r
	}

	var b strings.Builder
	b.WriteString("# handoffkeep MySQL table stats\n\n")
	fmt.Fprintf(&b, "Snapshot read at %s (UTC) in %d ms for %d tables. Rows are an exact `COUNT(*)`. \"Rows (estimate)\" is `TABLE_ROWS`. Data and index bytes are the engine's `DATA_LENGTH` and `INDEX_LENGTH`: estimates, not exact sizes. Statistics read fresh: %s.\n\n",
		cur.At.Format(time.RFC3339), cur.ReadMS, len(cur.Tables), yesNo(cur.Fresh))
	fmt.Fprintf(&b, "Previous snapshot: %s. About seven days earlier: %s.\n\n", recordLabel(prev), recordLabel(ref))

	rows, bytes := tableStatsTotals(cur)
	fmt.Fprintf(&b, "Totals: %d rows (%s vs previous, %s vs about seven days earlier); %d data and index bytes (%s, %s).\n\n",
		rows, deltaOf(rows, prev, tableStatsRowsTotal), deltaOf(rows, ref, tableStatsRowsTotal),
		bytes, deltaOf(bytes, prev, tableStatsBytesTotal), deltaOf(bytes, ref, tableStatsBytesTotal))
	if prev != nil {
		added, removed := tableStatsChurn(*prev, cur)
		fmt.Fprintf(&b, "Tables added since the previous snapshot: %d. Removed: %d.\n\n", added, removed)
	}

	b.WriteString("| Table | Rows (exact) | Rows (estimate) | Data bytes | Index bytes | Δ rows vs previous | Δ bytes vs previous | Δ rows vs ~7 days | Δ bytes vs ~7 days |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, name := range tableStatsByBytes(cur) {
		v := cur.Tables[name]
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %s | %s | %s | %s |\n",
			tableCell(name), v[0], v[1], v[2], v[3],
			tableDelta(name, v, prev, false), tableDelta(name, v, prev, true),
			tableDelta(name, v, ref, false), tableDelta(name, v, ref, true))
	}

	js, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\n## History\n\nFormat v1: one JSON line, snapshots oldest first. A table maps to `[rows_exact, rows_estimate, data_bytes, index_bytes]`; `fresh` says the statistics were read with `information_schema_stats_expiry = 0`. Kept: %d snapshots, at most %d.\n\n", n, TableStatsHistoryMax)
	b.WriteString(tableStatsFence + "\n")
	b.Write(js)
	b.WriteString("\n```\n")
	return b.String(), nil
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no (the server rejected the setting; estimates may be stale)"
}

func recordLabel(r *tableStatRecord) string {
	if r == nil {
		return "none yet"
	}
	return r.At.Format(time.RFC3339)
}

func tableStatsTotals(r tableStatRecord) (rows, bytes int64) {
	for _, v := range r.Tables {
		rows += v[0]
		bytes += v[2] + v[3]
	}
	return rows, bytes
}

func tableStatsRowsTotal(r tableStatRecord) int64 {
	rows, _ := tableStatsTotals(r)
	return rows
}

func tableStatsBytesTotal(r tableStatRecord) int64 {
	_, bytes := tableStatsTotals(r)
	return bytes
}

// deltaOf formats cur minus the reference's total, or n/a without a reference.
func deltaOf(cur int64, ref *tableStatRecord, total func(tableStatRecord) int64) string {
	if ref == nil {
		return "n/a"
	}
	return signed(cur - total(*ref))
}

// tableDelta formats one table's change against ref: rows (or data+index bytes
// when bytes is true). A table absent from ref is "new"; no ref is "n/a".
func tableDelta(name string, v [4]int64, ref *tableStatRecord, bytes bool) string {
	if ref == nil {
		return "n/a"
	}
	old, ok := ref.Tables[name]
	if !ok {
		return "new"
	}
	if bytes {
		return signed((v[2] + v[3]) - (old[2] + old[3]))
	}
	return signed(v[0] - old[0])
}

func tableStatsChurn(prev, cur tableStatRecord) (added, removed int) {
	for name := range cur.Tables {
		if _, ok := prev.Tables[name]; !ok {
			added++
		}
	}
	for name := range prev.Tables {
		if _, ok := cur.Tables[name]; !ok {
			removed++
		}
	}
	return added, removed
}

// tableStatsByBytes orders the tables by data plus index bytes, largest first,
// then by name, so the summary reads top-down.
func tableStatsByBytes(r tableStatRecord) []string {
	names := make([]string, 0, len(r.Tables))
	for name := range r.Tables {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := r.Tables[names[i]], r.Tables[names[j]]
		if sa, sb := a[2]+a[3], b[2]+b[3]; sa != sb {
			return sa > sb
		}
		return names[i] < names[j]
	})
	return names
}

// tableCell makes a table name safe inside a Markdown table cell.
func tableCell(name string) string {
	safe := strings.NewReplacer("|", `\|`, "`", "'", "\n", " ").Replace(name)
	return "`" + safe + "`"
}

func signed(v int64) string {
	if v > 0 {
		return fmt.Sprintf("+%d", v)
	}
	return fmt.Sprintf("%d", v)
}

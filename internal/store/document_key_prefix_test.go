package store

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestKeyPrefixBounds pins the half-open range that stands in for a prefix
// match: the prefix itself, and the first string that sorts after every key
// beginning with it under byte order.
func TestKeyPrefixBounds(t *testing.T) {
	cases := []struct{ prefix, hi string }{
		{"doc/", "doc0"}, // '/'(0x2f) → '0'(0x30)
		{"a", "b"},
		{"reports/mwr-", "reports/mwr."}, // '-'(0x2d) → '.'(0x2e)
		{`back\`, `back]`},               // '\'(0x5c) → ']'(0x5d)
		{"é", "ê"},                       // U+00e9 → U+00ea
		{"\uD7FF", "\uE000"},             // the surrogate block holds no code point
		// A last code point with no successor is closed by the key length.
		{"x" + keyPrefixSentinel, "x" + keyPrefixSentinel + strings.Repeat(keyPrefixSentinel, maxDocKeyBytes-len("x"+keyPrefixSentinel)+1)},
		{"", strings.Repeat(keyPrefixSentinel, maxDocKeyBytes+1)},
		{"\xff", "\xff" + strings.Repeat(keyPrefixSentinel, maxDocKeyBytes)}, // not UTF-8: matches no key
	}
	for _, tc := range cases {
		lo, hi := keyPrefixBounds(tc.prefix)
		if lo != tc.prefix || hi != tc.hi {
			t.Errorf("keyPrefixBounds(%q) = (%q, %q), want (%q, %q)", tc.prefix, lo, hi, tc.prefix, tc.hi)
		}
	}
}

// TestListDocumentsPrefixReachesUnboundableKeys pins the case a bound cannot
// express: a prefix whose last code point is U+10FFFF still reads the keys
// that extend it, which the previous release can write during a rollout.
func TestListDocumentsPrefixReachesUnboundableKeys(t *testing.T) {
	s := schemaStore(t, "documents_prefix_noncharacter")
	keys := []string{"policy/" + keyPrefixSentinel, "policy/" + keyPrefixSentinel + keyPrefixSentinel, "policy/other"}
	for _, key := range keys {
		if _, _, err := s.PutDocument(t.Context(), Document{Key: key, Kind: "note", Session: "s", Body: "b", CreatedBy: "tester"}); err != nil {
			t.Fatalf("put %q: %v", key, err)
		}
	}
	rows, err := s.ListDocuments(t.Context(), "policy/"+keyPrefixSentinel, "", "", 0, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, doc := range rows {
		got = append(got, doc.Key)
	}
	if want := []string{keys[0], keys[1]}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("prefix read = %q, want %q", got, want)
	}
}

// TestListDocumentsPrefixMatchesLike is the equivalence the rewrite owes: a
// prefix read must return the rows, in the order, that the escaped LIKE filter
// it replaced returned — including keys holding LIKE metacharacters.
func TestListDocumentsPrefixMatchesLike(t *testing.T) {
	s := schemaStore(t, "documents_prefix_like")
	ctx := t.Context()
	keys := []string{
		"doc/", "doc/a", "doc/a/b", "doc/a/b/", "doc/a%", "doc/a_b",
		`doc/back\slash`, "doc/aé", "doc/zzz", "doc0", "doc-", "doc.",
		"do", "docs", "doc_", "deploy/doc/x", "DOC/x",
	}
	for _, key := range keys {
		if _, _, err := s.PutDocument(ctx, Document{Key: key, Kind: "note", Session: "prefix-test", Body: "body", CreatedBy: "tester"}); err != nil {
			t.Fatalf("put %q: %v", key, err)
		}
	}
	prefixes := []string{"", "doc/", "doc", "doc/a", "doc/a_", "doc/a%", `doc/back\`, "doc0", "docs", "DOC/", "deploy/doc/"}
	for _, prefix := range prefixes {
		got, err := s.ListDocuments(ctx, prefix, "", "", 0, nil, 1000)
		if err != nil {
			t.Fatalf("prefix %q: %v", prefix, err)
		}
		var gotKeys []string
		for _, doc := range got {
			gotKeys = append(gotKeys, doc.Key)
		}
		if want := likePrefixKeys(t, s, prefix); strings.Join(gotKeys, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("prefix %q = %v, want the LIKE read %v", prefix, gotKeys, want)
		}
	}

	// Paging by id keeps the same rows under the same prefix.
	zero := int64(0)
	paged, err := s.ListDocuments(ctx, "doc/", "", "", 0, &zero, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := likePrefixKeys(t, s, "doc/"); len(paged) != len(want) {
		t.Fatalf("after_id read under doc/ returned %d rows, want %d", len(paged), len(want))
	}
}

// likePrefixKeys is the read the prefix filter replaced: the escaped prefix
// followed by '%', which is the literal-prefix match ListDocuments made.
func likePrefixKeys(t *testing.T, s *Store, prefix string) []string {
	t.Helper()
	pattern := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix) + "%"
	rows, err := s.pool.Query(t.Context(), `SELECT key FROM documents WHERE key LIKE $1 ESCAPE '\' ORDER BY key`, pattern)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// documentsSeedCount mirrors the production row count of documents on
// 2026-11-16 (33,482, hk 4899), so the planner is asked at the size the bug
// appeared at rather than at a fixture's.
const documentsSeedCount = 33482

// seedProductionDocuments fills documents with documentsSeedCount keys spread
// over prefixes production holds, so a prefix read is as selective as it is
// there.
func seedProductionDocuments(t *testing.T, s *Store) {
	t.Helper()
	mwr := documentsSeedCount / 100
	if _, err := s.pool.Exec(t.Context(), `
INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at)
SELECT 'reports/mwr-'||lpad(g::text,6,'0'),'report','seed','job',repeat('r',64),'sha-r-'||g,'tester',now(),now()
FROM generate_series(1,$1::int) g
UNION ALL
SELECT 'brief/'||lpad(g::text,6,'0'),'brief','seed','job',repeat('b',64),'sha-b-'||g,'tester',now(),now()
FROM generate_series(1,$2::int) g`, mwr, documentsSeedCount-mwr); err != nil {
		t.Fatalf("seed documents: %v", err)
	}
	if _, err := s.pool.Exec(t.Context(), `ANALYZE documents`); err != nil {
		t.Fatalf("analyze documents: %v", err)
	}
}

// TestConsolePrefixReadsUseIndex is the plan evidence for the fix: at
// production row counts every statement one console-shaped read issues against
// documents is an index range on documents_key_c, never a sequential scan of
// the whole table.
func TestConsolePrefixReadsUseIndex(t *testing.T) {
	s, counter := countingStore(t, "documents_prefix_plan")
	ctx := t.Context()
	seedProductionDocuments(t, s)
	svc := DeployService{Name: "hk", Repo: "mgh3326/handoffkeep", RecordPrefixes: []string{"deploy/hk/"}, Active: true}
	if _, err := s.PutDeployService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		seedDeployRecord(t, s, "hk", "success", time.Date(2026, 11, 1+i, 0, 0, 0, 0, time.UTC))
	}

	counter.Reset()
	if _, err := s.ListDocuments(ctx, "brief/00001", "", "", 0, nil, 100); err != nil {
		t.Fatal(err)
	}
	pages, err := s.ListDeployRecordsByPrefix(ctx, []string{"deploy/hk/"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(pages["deploy/hk/"].Records); n != 5 {
		t.Fatalf("deploy/hk/ page holds %d records, want 5", n)
	}
	if _, err := s.DeployLagStatuses(ctx, []DeployService{svc}); err != nil {
		t.Fatal(err)
	}

	// The listing, the record page and the last-success scan.
	statements := counter.StatementsMatching("FROM documents")
	if len(statements) != 3 {
		t.Fatalf("captured %d statements reading documents, want 3", len(statements))
	}
	for _, st := range statements {
		plan := explainPlan(t, s, st.SQL, st.Args)
		if strings.Contains(plan, "Seq Scan on documents") {
			t.Fatalf("statement scans the whole table:\n%s\nplan: %s", st.SQL, plan)
		}
		if !strings.Contains(plan, "documents_key_c") {
			t.Fatalf("statement does not use documents_key_c:\n%s\nplan: %s", st.SQL, plan)
		}
	}
}

// explainPlan runs EXPLAIN over the statement with its own arguments and
// returns the plan text, where a node names its relation ("Seq Scan on
// documents") as FORMAT JSON would keep the two apart.
func explainPlan(t *testing.T, s *Store, query string, args []any) string {
	t.Helper()
	return explainWith(t, s, "EXPLAIN ", query, args)
}

// explainAnalyzedPlan is explainPlan with the row counts the plan actually
// touched, which is how a test tells an ordered index scan of one page from a
// sort of the whole prefix range.
func explainAnalyzedPlan(t *testing.T, s *Store, query string, args []any) string {
	t.Helper()
	return explainWith(t, s, "EXPLAIN (ANALYZE, BUFFERS) ", query, args)
}

func explainWith(t *testing.T, s *Store, prefix, query string, args []any) string {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), prefix+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain: %v", err)
	}
	return strings.Join(lines, "\n")
}

// maxScanRows is the largest actual row count among the plan's nodes over the
// named scan ("on documents d2"); the greedy match takes the actual count that
// follows the estimate on the same line.
func maxScanRows(t *testing.T, plan, scan string) int {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(scan) + ` .*rows=(\d+)`)
	max := -1
	for _, line := range strings.Split(plan, "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("plan row count %q: %v", m[1], err)
		}
		if n > max {
			max = n
		}
	}
	if max < 0 {
		t.Fatalf("no node over %q in the plan:\n%s", scan, plan)
	}
	return max
}

// TestLastSuccessPageReadsOnePage pins the plan of the paged deploy-record
// read: the bounds, the cursor and the ORDER BY share the index's collation,
// so a page is an ordered index scan of one page instead of a sort of the whole
// prefix range. Six hundred records under one prefix make the two plans differ
// by the rows they touch, and one prefix per statement keeps EXPLAIN's per-loop
// counts unambiguous.
func TestLastSuccessPageReadsOnePage(t *testing.T) {
	s, counter := countingStore(t, "last_success_page_order")
	ctx := t.Context()
	for i := range 600 {
		seedDeployRecord(t, s, "hk", "failed", time.Date(2026, 9, 1, 0, 0, i, 0, time.UTC))
	}
	// Statistics for the planner: without them it estimates a handful of rows
	// and a bitmap path looks as good as the ordered index scan.
	if _, err := s.pool.Exec(ctx, `ANALYZE documents`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	counter.Reset()
	if _, err := scanPrefixesLastSuccessDeployTx(ctx, tx, []lastSuccessPrefix{{prefix: "deploy/hk/", services: []string{"hk"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	statements := counter.StatementsMatching("FROM documents")
	if len(statements) == 0 {
		t.Fatal("no documents statement captured")
	}
	plan := explainAnalyzedPlan(t, s, statements[0].SQL, statements[0].Args)
	t.Logf("last-success page plan:\n%s", plan)
	if rows := maxScanRows(t, plan, "on documents d2"); rows > 2*lastSuccessPageSize {
		t.Fatalf("a page read %d rows of documents, want about %d:\n%s", rows, lastSuccessPageSize, plan)
	}
}

// TestDeployLagStatusesReadsAreBounded pins the batching: four services cost
// the same three statements a single service does (the service list, one page
// per prefix, one count for every boundary).
func TestDeployLagStatusesReadsAreBounded(t *testing.T) {
	s, counter := countingStore(t, "deploy_lag_statements")
	ctx := t.Context()
	var svcs []DeployService
	for i, name := range []string{"alpha", "beta", "gamma", "delta"} {
		svc := DeployService{Name: name, Repo: "org/" + name, RecordPrefixes: []string{"deploy/" + name + "/"}, Active: true}
		if _, err := s.PutDeployService(ctx, svc); err != nil {
			t.Fatal(err)
		}
		seedDeployRecord(t, s, name, "success", time.Date(2026, 11, 1+i, 0, 0, 0, 0, time.UTC))
		svcs = append(svcs, svc)
	}
	counter.Reset()
	statuses, err := s.DeployLagStatuses(ctx, svcs)
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range svcs {
		if st, ok := statuses[svc.Name]; !ok || !st.HasDeployed || st.DeployedAt == nil {
			t.Fatalf("statuses[%s] = %+v, want a deployed boundary", svc.Name, st)
		}
	}
	if n := counter.QueryCount(); n != 3 {
		t.Fatalf("DeployLagStatuses issued %d queries for %d services, want 3", n, len(svcs))
	}
}

// TestListDeployRecordsByPrefixReadsOnce pins the one-statement read and the
// page it reports: newest key first, and More when the prefix holds more than
// were asked for.
func TestListDeployRecordsByPrefixReadsOnce(t *testing.T) {
	s, counter := countingStore(t, "deploy_records_statements")
	ctx := t.Context()
	for _, doc := range []struct{ key, service string }{
		{"deploy/a/20261101T000000Z", "a"},
		{"deploy/a/20261102T000000Z", "a"},
		{"deploy/a/20261103T000000Z", "a"},
		{"deploy/b/20261101T000000Z", "b"},
		{"deploy/c/20261101T000000Z", "c"},
	} {
		putDeploy(t, s, doc.key, doc.service, "success", nil)
	}
	counter.Reset()
	pages, err := s.ListDeployRecordsByPrefix(ctx, []string{"deploy/a/", "deploy/b/", "deploy/c/"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if n := counter.QueryCount(); n != 1 {
		t.Fatalf("ListDeployRecordsByPrefix issued %d queries for 3 prefixes, want 1", n)
	}
	page := pages["deploy/a/"]
	if !page.More || len(page.Records) != 2 || page.Records[0].Key != "deploy/a/20261103T000000Z" {
		t.Fatalf("deploy/a/ page = %+v (more=%v), want the two newest keys and more", page.Records, page.More)
	}
	if other := pages["deploy/b/"]; other.More || len(other.Records) != 1 {
		t.Fatalf("deploy/b/ page = %+v (more=%v), want one record and no more", other.Records, other.More)
	}
}

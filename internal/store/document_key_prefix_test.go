package store

import (
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
		{"x\U0010FFFF", "x\U0010FFFF" + keyPrefixSentinel},
		{"", keyPrefixSentinel},
		{"\xff", "\xff" + keyPrefixSentinel}, // not UTF-8: no narrower bound is provable
	}
	for _, tc := range cases {
		lo, hi := keyPrefixBounds(tc.prefix)
		if lo != tc.prefix || hi != tc.hi {
			t.Errorf("keyPrefixBounds(%q) = (%q, %q), want (%q, %q)", tc.prefix, lo, hi, tc.prefix, tc.hi)
		}
	}
}

// TestValidDocKeyRefusesUnboundableCodePoint pins the invariant every prefix
// range depends on: U+10FFFF has no successor, so a key holding it could not be
// reached by the upper bound of a prefix that ends before it.
func TestValidDocKeyRefusesUnboundableCodePoint(t *testing.T) {
	s := schemaStore(t, "documents_prefix_noncharacter")
	ctx := t.Context()
	if _, _, err := s.PutDocument(ctx, Document{Key: "policy/x\U0010FFFF", Kind: "note", Session: "s", Body: "b", CreatedBy: "tester"}); err == nil {
		t.Fatal("a key holding U+10FFFF was accepted")
	}
	// A prefix ending in U+10FFFF therefore matches nothing, rather than
	// matching a key its bound admits but the table cannot hold.
	rows, err := s.ListDocuments(ctx, "policy/\U0010FFFF", "", "", 0, nil, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows=%v err=%v, want none", rows, err)
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
// documents is an index range on documents_key_pattern, never a sequential
// scan of the whole table.
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
		if !strings.Contains(plan, "documents_key_pattern") {
			t.Fatalf("statement does not use documents_key_pattern:\n%s\nplan: %s", st.SQL, plan)
		}
	}
}

// explainPlan runs EXPLAIN over the statement with its own arguments and
// returns the plan text, where a node names its relation ("Seq Scan on
// documents") as FORMAT JSON would keep the two apart.
func explainPlan(t *testing.T, s *Store, query string, args []any) string {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), "EXPLAIN "+query, args...)
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
	if n := counter.Count(); n != 3 {
		t.Fatalf("DeployLagStatuses issued %d statements for %d services, want 3", n, len(svcs))
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
	if n := counter.Count(); n != 1 {
		t.Fatalf("ListDeployRecordsByPrefix issued %d statements for 3 prefixes, want 1", n)
	}
	page := pages["deploy/a/"]
	if !page.More || len(page.Records) != 2 || page.Records[0].Key != "deploy/a/20261103T000000Z" {
		t.Fatalf("deploy/a/ page = %+v (more=%v), want the two newest keys and more", page.Records, page.More)
	}
	if other := pages["deploy/b/"]; other.More || len(other.Records) != 1 {
		t.Fatalf("deploy/b/ page = %+v (more=%v), want one record and no more", other.Records, other.More)
	}
}

#!/usr/bin/env python3
import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location("sqlport", Path(__file__).with_name("sqlport.py"))
sqlport = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = sqlport
spec.loader.exec_module(sqlport)


class RewriteTests(unittest.TestCase):
    def test_placeholders_expand_reused_arguments_in_occurrence_order(self):
        self.assertEqual(sqlport.rewrite("SELECT $2,$1,$2"), ("SELECT ?,?,?", [2, 1, 2], ["$n → ?"], False))

    def test_cast_map_is_strict_and_handles_function_operands(self):
        self.assertEqual(sqlport.rewrite("SELECT $1::jsonb")[:2], ("SELECT CAST(? AS JSON)", [1]))
        self.assertEqual(sqlport.rewrite("SELECT count(*)::int FROM t")[0], "SELECT CAST(count(*) AS SIGNED) FROM t")
        self.assertEqual(sqlport.rewrite("SELECT lower(name)::text FROM t")[0], "SELECT CAST(lower(name) AS CHAR) FROM t")
        self.assertEqual(sqlport.rewrite("SELECT lower(name)::text || '%' FROM t")[0], "SELECT CONCAT(CAST(lower(name) AS CHAR), '%') FROM t")
        self.assertEqual(sqlport.rewrite("SELECT x::double precision")[0], "SELECT CAST(x AS DOUBLE)")
        self.assertEqual(sqlport.rewrite("SELECT x::timestamp with time zone")[0], "SELECT CAST(x AS DATETIME(6))")
        self.assertTrue(any("unsupported ::regclass cast" in item for item in sqlport.rewrite("SELECT 't'::regclass")[2]))
        self.assertTrue(any("unsupported ::numeric cast modifier" in item for item in sqlport.rewrite("SELECT x::numeric(8,2)")[2]))
        self.assertTrue(any("unsupported ::numeric cast" in item for item in sqlport.rewrite("SELECT x::numeric")[2]))
        self.assertTrue(any("unsupported ::decimal cast" in item for item in sqlport.rewrite("SELECT x::decimal")[2]))
        self.assertTrue(any("unsupported ::text cast modifier" in item for item in sqlport.rewrite("SELECT x::text[]")[2]))

    def test_text_concat_requires_text_evidence_and_preserves_calls(self):
        self.assertEqual(sqlport.rewrite("SELECT host_id || ':' || account_key")[0], "SELECT CONCAT(host_id, ':', account_key)")
        self.assertEqual(sqlport.rewrite("SELECT name LIKE $1 || '%' ")[:2], ("SELECT name LIKE CONCAT(?, '%') ", [1]))
        self.assertIsNone(sqlport.rewrite("SELECT x || lower(y) || '%' ")[0])
        self.assertEqual(sqlport.rewrite("SELECT CAST(x AS CHAR) || lower(y) || '%' ")[0], "SELECT CONCAT(CAST(x AS CHAR), lower(y), '%') ")
        self.assertTrue(any("non-text" in item for item in sqlport.rewrite("SELECT x || lower(y)")[2]))
        self.assertIsNone(sqlport.rewrite("SELECT lower (name) || '%'")[0])
        self.assertIsNone(sqlport.rewrite("SELECT 'x' || lower (y) FROM t")[0])

    def test_concat_boundaries_are_allow_listed(self):
        for sql in ("SELECT a || b + c", "SELECT a + b || c", "SELECT (host_id || ':') || account_key"):
            with self.subTest(sql=sql):
                self.assertIsNone(sqlport.rewrite(sql)[0])
        self.assertIsNone(sqlport.rewrite("SELECT 1 FROM t WHERE x = $1 AND (a) || b = 'c'")[0])
        self.assertEqual(sqlport.rewrite("SELECT 1 FROM t WHERE x = $1 AND (a) IS DISTINCT FROM b")[0], "SELECT 1 FROM t WHERE x = ? AND NOT ((a) <=> b)")
        self.assertEqual(sqlport.rewrite("SELECT doc->>'x' || 'y'")[0], "SELECT CONCAT(JSON_UNQUOTE(JSON_EXTRACT(doc, '$.x')), 'y')")
        self.assertIsNone(sqlport.rewrite("SELECT doc->'x' || 'y'")[0])

    def test_distinct_predicate_and_function_operands(self):
        self.assertEqual(sqlport.rewrite("a IS DISTINCT FROM b")[0], "NOT (a <=> b)")
        self.assertEqual(sqlport.rewrite("a IS NOT DISTINCT FROM b")[0], "(a <=> b)")
        self.assertEqual(sqlport.rewrite("lower(a) IS DISTINCT FROM b")[0], "NOT (lower(a) <=> b)")
        self.assertIsNone(sqlport.rewrite("lower (a) IS DISTINCT FROM b")[0])
        self.assertIsNone(sqlport.rewrite("a IS DISTINCT FROM lower (b)")[0])
        for sql in ("a = b IS DISTINCT FROM c", "x + 1 IS DISTINCT FROM y", "a IS DISTINCT FROM b + 1", "x LIKE a IS DISTINCT FROM b", "a IS DISTINCT FROM b LIKE c"):
            with self.subTest(sql=sql):
                self.assertIsNone(sqlport.rewrite(sql)[0])

    def test_count_filter_preserves_zero_for_empty_input(self):
        self.assertEqual(sqlport.rewrite("COUNT(*) FILTER (WHERE ok)")[0], "COUNT(CASE WHEN ok THEN 1 END)")
        self.assertEqual(sqlport.rewrite("COUNT(value) FILTER (WHERE ok)")[0], "COUNT(CASE WHEN ok THEN value END)")
        self.assertIsNone(sqlport.rewrite("COUNT(DISTINCT value) FILTER (WHERE ok)")[0])

    def test_json_arrows_map_to_mysql_and_chains_refuse(self):
        self.assertEqual(sqlport.rewrite("doc -> 'x', doc ->> 'x'")[0], "JSON_EXTRACT(doc, '$.x'), JSON_UNQUOTE(JSON_EXTRACT(doc, '$.x'))")
        self.assertEqual(sqlport.rewrite("doc -> 0")[0], "JSON_EXTRACT(doc, '$[0]')")
        for sql in ("refs->'disposition'->'answer'->>'event_id'", "CAST(? AS JSON) -> 'x'", "doc -> 'has.dot'"):
            with self.subTest(sql=sql):
                self.assertIsNone(sqlport.rewrite(sql)[0])

    def test_key_exists_after_single_arrow_is_rewritten(self):
        self.assertEqual(sqlport.rewrite("doc ? 'x'")[0], "JSON_CONTAINS_PATH(doc, 'one', '$.x')")
        self.assertEqual(sqlport.rewrite("doc->'a' ? 'b'")[0], "JSON_CONTAINS_PATH(JSON_EXTRACT(doc, '$.a'), 'one', '$.b')")
        self.assertIsNone(sqlport.rewrite("doc ? 'has.dot'")[0])
        self.assertIsNone(sqlport.rewrite("doc->'a'->'b' ? 'c'")[0])

    def test_now_is_flagged_even_when_other_construct_is_todo(self):
        self.assertTrue(sqlport.rewrite("SELECT now()")[3])
        self.assertTrue(sqlport.rewrite("SELECT now() - interval '1 day'")[3])
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / "now.go"
            source.write_text("var q = `SELECT now() - interval '1 day'`\n")
            rows, counts = sqlport.analyze(source)
        self.assertTrue(rows[0]["now"])
        self.assertIn("now() [manual time audit]", rows[0]["constructs"])
        self.assertEqual(counts[("TODO", "now() [manual time audit]")], 1)

    def test_reviewer_regressions_fail_closed(self):
        samples = (
            "SELECT substring(t.refs->>'pr' FROM '^https://example.invalid')",
            "SELECT lower (name) || '%'",
            "SELECT lower (a) IS DISTINCT FROM b",
            "SELECT refs->'disposition'->'answer'->>'event_id'",
            "SELECT CAST(? AS JSON) -> 'x'",
        )
        for sql in samples:
            with self.subTest(sql=sql):
                self.assertIsNone(sqlport.rewrite(sql)[0])
        self.assertEqual(sqlport.rewrite("SELECT t.refs->'disposition' ? 'answer'")[0], "SELECT JSON_CONTAINS_PATH(JSON_EXTRACT(t.refs, '$.disposition'), 'one', '$.answer')")

    def test_construct_table_refusals(self):
        samples = {
            "RETURNING": "INSERT INTO t VALUES(1) RETURNING id",
            "ON CONFLICT": "INSERT INTO t VALUES(1) ON CONFLICT DO NOTHING",
            "= ANY / arrays / unnest": "SELECT 1 WHERE id = ANY($1)",
            "DISTINCT ON": "SELECT DISTINCT ON (id) id FROM t",
            "LATERAL": "SELECT * FROM t, LATERAL (SELECT 1) x",
            "data-modifying CTE": "WITH changed AS (DELETE FROM t RETURNING id) SELECT id FROM changed",
            "xmax": "SELECT xmax FROM t",
            "advisory locks": "SELECT pg_advisory_xact_lock(1)",
            "plpgsql": "CREATE FUNCTION f() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN END $$",
            "ILIKE": "SELECT x ILIKE '%x%'",
            "regex match operators": "SELECT x ~ '^x'",
            "NULLS FIRST/LAST": "SELECT x ORDER BY x NULLS LAST",
            "interval literal": "SELECT now() - interval '1 day'",
            "clock_timestamp()": "SELECT clock_timestamp()",
            "generate_series/jsonb_* and other PostgreSQL functions": "SELECT jsonb_set(x, '{}', '1')",
            "EXTRACT(EPOCH)": "SELECT EXTRACT(EPOCH FROM now())",
            "IS TRUE/IS FALSE": "SELECT x IS TRUE",
            "SIMILAR TO": "SELECT x SIMILAR TO '%x%'",
            "substring FROM regex": "SELECT substring(x FROM '^x')",
            "DO block": "DO $$ BEGIN END $$",
            "PostgreSQL-only type spelling": "CREATE TABLE x (data JSONB)",
            "CREATE INDEX IF NOT EXISTS": "CREATE INDEX IF NOT EXISTS idx_x ON x(id)",
        }
        for expected, sql in samples.items():
            with self.subTest(expected=expected):
                result = sqlport.rewrite(sql)
                self.assertIsNone(result[0])
                self.assertIn(expected, result[2])

    def test_postgres_only_constructs_are_discovered_without_other_hints(self):
        source = 'var q = "SELECT name ILIKE \'%x%\'"'
        literals = sqlport.go_literals(source)
        self.assertEqual(len(literals), 1)
        self.assertIsNone(sqlport.rewrite(literals[0].text)[0])

    def test_hk5059_quota_comparisons(self):
        quota = "SELECT o.id,o.value,l.confirmed_at FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=$1 ORDER BY l.kind,l.window_key"
        self.assertEqual(sqlport.rewrite(quota)[0], "SELECT o.id,o.value,l.confirmed_at FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=? ORDER BY l.kind,l.window_key")
        presence = "SELECT host_id,account_key,last_seen FROM quota_presence WHERE ($1='' OR host_id=$1) AND ($2='' OR account_key=$2) AND last_seen >= clock_timestamp()-$4*interval '1 second' ORDER BY last_seen DESC,host_id,account_key LIMIT $3"
        self.assertIsNone(sqlport.rewrite(presence)[0])
        self.assertEqual(sqlport.rewrite(" FROM bench_reps WHERE id=$1 FOR UPDATE")[0], " FROM bench_reps WHERE id=? FOR UPDATE")
        self.assertIsNone(sqlport.rewrite("INSERT INTO quota_observations(account_key) VALUES($1) RETURNING id")[0])

    def test_corpus_invariant_for_internal_store(self):
        violations = sqlport._corpus_auto_violations(Path(__file__).resolve().parents[1])
        self.assertEqual(violations, [])


if __name__ == "__main__":
    unittest.main()

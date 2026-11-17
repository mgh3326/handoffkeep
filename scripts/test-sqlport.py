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

    def test_all_constructs_outside_placeholders_are_todo(self):
        samples = (
            "SELECT $1::text",
            "SELECT t.done::text FROM tasks t WHERE t.id = $1",
            "SELECT $1 / 2",
            "SELECT count(*) / 2 FROM t WHERE id = $1",
            "SELECT id FROM t WHERE a = $1 ORDER BY last_seen DESC",
            "SELECT host_id || ':' || account_key",
            "SELECT a IS DISTINCT FROM b",
            "SELECT a IS NOT DISTINCT FROM b",
            "SELECT COUNT(*) FILTER (WHERE ok)",
            "SELECT COUNT(DISTINCT id) FILTER (WHERE ok)",
            "SELECT doc ? 'x'",
            "SELECT doc->'x'",
            "SELECT doc->>'x'",
            "SELECT doc || '{}'",
            "SELECT (doc->'a') || '{}'",
            "SELECT a || b->>'k'",
            "SELECT doc->'$.x'",
            "SELECT doc->>'$'",
            "SELECT doc ? $1",
            "SELECT doc @> '{}'",
            "SELECT a && ARRAY[1]",
            'SELECT id FROM t WHERE "to" = $1',
            "SELECT id FROM t WHERE dir = 'C:\\temp' AND id = $1",
            "SELECT name COLLATE C = $1",
            'SELECT d.key COLLATE "C" >= $1',
        )
        for sql in samples:
            with self.subTest(sql=sql):
                self.assertIsNone(sqlport.rewrite(sql)[0])

    def test_double_quoted_identifiers_and_collations_are_todo(self):
        self.assertIsNone(sqlport.rewrite('SELECT id FROM t WHERE "to" = $1')[0])
        self.assertIsNone(sqlport.rewrite('SELECT d.key COLLATE "C" >= $1')[0])

    def test_backslash_in_string_literal_is_todo(self):
        self.assertIsNone(sqlport.rewrite("SELECT id FROM t WHERE dir = 'C:\\temp' AND id = $1")[0])

    def test_now_is_flagged_even_when_other_construct_is_todo(self):
        self.assertIsNone(sqlport.rewrite("SELECT now()")[0])
        self.assertTrue(sqlport.rewrite("SELECT now()")[3])
        self.assertTrue(sqlport.rewrite("SELECT now() - interval '1 day'")[3])
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / "now.go"
            source.write_text("var q = `SELECT now() - interval '1 day'`\n")
            rows, counts = sqlport.analyze(source)
        self.assertTrue(rows[0]["now"])
        self.assertIn("now() [manual time audit]", rows[0]["constructs"])
        self.assertEqual(counts[("TODO", "now() [manual time audit]")], 1)

    def test_construct_table_refusals(self):
        samples = {
            "::type cast": "SELECT $1::text",
            "ORDER BY": "SELECT id FROM t ORDER BY last_seen DESC",
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
            "nonportable PostgreSQL operator": "SELECT a @> b",
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

    def test_go_literal_discovery_skips_comments_and_rune_literals(self):
        source = '// old query: "SELECT $1"\n/* old query: `SELECT $2` */\nvar rune = \'x\'\nvar q = `SELECT $3`\n'
        literals = sqlport.go_literals(source)
        self.assertEqual([literal.text for literal in literals], ["SELECT $3"])

    def test_unlexable_sql_literal_is_counted_as_todo(self):
        source = "var q = `SELECT 'unterminated`\n"
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "broken.go"
            path.write_text(source)
            rows, counts = sqlport.analyze(path)
        self.assertEqual(len(rows), 1)
        self.assertIsNone(rows[0]["mysql"])
        self.assertIn("TODO unlexable SQL: unterminated string literal", rows[0]["constructs"])
        self.assertEqual(counts[("TODO", "TODO unlexable SQL: unterminated string literal")], 1)

    def test_postgres_escape_string_literal_is_discovered_and_refused(self):
        source = r"var q = `SELECT id FROM t WHERE name ILIKE $1 ESCAPE '\'`"
        literals = sqlport.go_literals(source)
        self.assertEqual(len(literals), 1)
        result = sqlport.rewrite(literals[0].text)
        self.assertIsNone(result[0])
        self.assertIn("backslash in string literal", result[2])

    def test_hk5059_quota_and_bench_comparisons(self):
        quota = "SELECT o.id,o.value,l.confirmed_at FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=$1 ORDER BY l.kind,l.window_key"
        self.assertIsNone(sqlport.rewrite(quota)[0])
        self.assertIn("ORDER BY", sqlport.rewrite(quota)[2])
        presence = "SELECT host_id,account_key,last_seen FROM quota_presence WHERE ($1='' OR host_id=$1) AND ($2='' OR account_key=$2) AND last_seen >= clock_timestamp()-$4*interval '1 second' ORDER BY last_seen DESC,host_id,account_key LIMIT $3"
        self.assertIsNone(sqlport.rewrite(presence)[0])
        self.assertEqual(sqlport.rewrite(" FROM bench_reps WHERE id=$1 FOR UPDATE")[0], " FROM bench_reps WHERE id=? FOR UPDATE")
        self.assertIsNone(sqlport.rewrite("INSERT INTO quota_observations(account_key) VALUES($1) RETURNING id")[0])

    def test_corpus_invariant_for_internal_store(self):
        violations = sqlport._corpus_auto_violations(Path(__file__).resolve().parents[1])
        self.assertEqual(violations, [])

    def test_only_placeholders_are_auto_rewrites(self):
        self.assertEqual(
            {c.name for c in sqlport.CONSTRUCTS if c.disposition == "rewrite"},
            {"$n → ?"},
        )


if __name__ == "__main__":
    unittest.main()

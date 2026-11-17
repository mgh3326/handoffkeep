#!/usr/bin/env python3
import importlib.util
import sys
import unittest
from pathlib import Path


spec = importlib.util.spec_from_file_location("sqlport", Path(__file__).with_name("sqlport.py"))
sqlport = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = sqlport
spec.loader.exec_module(sqlport)


class RewriteTests(unittest.TestCase):
    def test_placeholders_expand_reused_arguments_in_occurrence_order(self):
        self.assertEqual(sqlport.rewrite("SELECT $2,$1,$2"), ("SELECT ?,?,?", [2, 1, 2], ["$n → ?"], False))

    def test_concat(self):
        self.assertEqual(sqlport.rewrite("SELECT host_id || ':' || account_key")[0], "SELECT CONCAT(host_id, ':', account_key)")

    def test_concat_with_placeholder_operand(self):
        self.assertEqual(sqlport.rewrite("SELECT name LIKE $1 || '%'")[:2], ("SELECT name LIKE CONCAT(?, '%')", [1]))

    def test_nested_concat_is_refused_when_inner_operator_remains(self):
        mysql, _, constructs, _ = sqlport.rewrite("SELECT (host_id || ':') || account_key")
        self.assertIsNone(mysql)
        self.assertIn("unsupported || expression", constructs)

    def test_cast(self):
        self.assertEqual(sqlport.rewrite("SELECT $1::jsonb"), ("SELECT CAST(? AS JSON)", [1], ["$n → ?", "::type → CAST"], False))
        self.assertEqual(sqlport.rewrite("SELECT value::text")[0], "SELECT CAST(value AS CHAR)")
        self.assertIn("unsupported ::regclass cast", sqlport.rewrite("SELECT 't'::regclass")[2])

    def test_distinct_predicate(self):
        self.assertEqual(sqlport.rewrite("a IS DISTINCT FROM b")[0], "NOT (a <=> b)")
        self.assertEqual(sqlport.rewrite("a IS NOT DISTINCT FROM b")[0], "(a <=> b)")
    def test_distinct_predicate_with_placeholder_operand(self):
        self.assertEqual(sqlport.rewrite("$1 IS DISTINCT FROM col")[:2], ("NOT (? <=> col)", [1]))

    def test_filter(self):
        self.assertEqual(sqlport.rewrite("COUNT(*) FILTER (WHERE ok)")[0], "SUM(CASE WHEN ok THEN 1 ELSE 0 END)")
        self.assertEqual(sqlport.rewrite("COUNT(value) FILTER (WHERE ok)")[0], "COUNT(CASE WHEN ok THEN value ELSE NULL END)")

    def test_json_ops_are_preserved_and_key_exists_is_rewritten(self):
        self.assertEqual(sqlport.rewrite("doc -> 'x', doc ->> 'x', doc ? 'x'")[0], "doc -> 'x', doc ->> 'x', JSON_CONTAINS_PATH(doc, 'one', '$.x')")
        self.assertIn("unsupported ? key-exists key", sqlport.rewrite("doc ? 'has.dot'")[2])

    def test_now_is_flagged(self):
        self.assertTrue(sqlport.rewrite("SELECT now()")[3])

    def test_refusals(self):
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
        }
        for expected, sql in samples.items():
            with self.subTest(expected=expected):
                self.assertIn(expected, sqlport.rewrite(sql)[2])
                self.assertIsNone(sqlport.rewrite(sql)[0])

    def test_hk5059_quota_literal_matches_the_hand_port(self):
        # From PR #470's quota.go:187. This is a direct mechanical port.
        pg = "SELECT o.id,o.value,l.confirmed_at FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=$1 ORDER BY l.kind,l.window_key"
        mysql_hand = "SELECT o.id,o.value,l.confirmed_at FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=? ORDER BY l.kind,l.window_key"
        self.assertEqual(sqlport.rewrite(pg)[0], mysql_hand)

    def test_hk5059_reused_quota_placeholders_match_the_hand_port(self):
        # From PR #470's quota.go:523. The caller expands arguments to match.
        pg = "SELECT host_id,account_key,last_seen FROM quota_presence WHERE ($1='' OR host_id=$1) AND ($2='' OR account_key=$2) AND last_seen >= clock_timestamp()-$4*interval '1 second' ORDER BY last_seen DESC,host_id,account_key LIMIT $3"
        mysql_hand = "SELECT host_id,account_key,last_seen FROM quota_presence WHERE (?='' OR host_id=?) AND (?='' OR account_key=?) AND last_seen >= DATE_SUB(UTC_TIMESTAMP(6), INTERVAL ? SECOND) ORDER BY last_seen DESC,host_id,account_key LIMIT ?"
        mysql, order, _, _ = sqlport.rewrite(pg)
        self.assertEqual(order, [1, 1, 2, 2, 4, 3])
        self.assertEqual(mysql.replace("clock_timestamp()-?*interval '1 second'", "DATE_SUB(UTC_TIMESTAMP(6), INTERVAL ? SECOND)"), mysql_hand)

    def test_hk5059_quota_semantic_port_is_refused_and_needs_hand_work(self):
        # PR #470's RETURNING/upsert rewrites are deliberately outside this tool.
        pg = "INSERT INTO quota_observations(account_key) VALUES($1) RETURNING id"
        self.assertIsNone(sqlport.rewrite(pg)[0])
        self.assertIn("RETURNING", sqlport.rewrite(pg)[2])

    def test_hk5059_bench_literal_matches_the_hand_port(self):
        # From PR #470's bench_outcomes.go:148; the SQL prefix is shared.
        pg = " FROM bench_reps WHERE id=$1 FOR UPDATE"
        mysql_hand = " FROM bench_reps WHERE id=? FOR UPDATE"
        self.assertEqual(sqlport.rewrite(pg)[0], mysql_hand)


if __name__ == "__main__":
    unittest.main()

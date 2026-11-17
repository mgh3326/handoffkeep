#!/usr/bin/env python3
"""Report mechanical MySQL rewrites for PostgreSQL SQL literals in Go files.

This deliberately does not attempt to port statement semantics. Refused SQL is
shown as TODO and must be rewritten by hand.
"""

from __future__ import annotations

import argparse
import ast
import json
import re
from collections import Counter
from dataclasses import dataclass
from pathlib import Path


REFUSALS = {
    "RETURNING": re.compile(r"\bRETURNING\b", re.I),
    "ON CONFLICT": re.compile(r"\bON\s+CONFLICT\b", re.I),
    "= ANY / arrays / unnest": re.compile(r"=\s*ANY\b|\bARRAY\s*\[|\bUNNEST\s*\(", re.I),
    "DISTINCT ON": re.compile(r"\bDISTINCT\s+ON\s*\(", re.I),
    "LATERAL": re.compile(r"\bLATERAL\b", re.I),
    "data-modifying CTE": re.compile(r"\bWITH\b(?:(?!\bWITH\b).){0,1000}?\bAS\s*\(\s*(?:INSERT|UPDATE|DELETE)\b", re.I | re.S),
    "xmax": re.compile(r"\bxmax\b", re.I),
    "advisory locks": re.compile(r"\bpg_(?:try_)?advisory_\w+\s*\(", re.I),
    "plpgsql": re.compile(r"\bLANGUAGE\s+plpgsql\b", re.I),
}

GO_STRING = re.compile(r"`[^`]*`|\"(?:\\[^\n]|[^\"\\\n])*\"", re.S)
SQL_HINT = re.compile(r"\b(?:SELECT|INSERT|UPDATE|DELETE|CREATE|ALTER|WITH|FROM|WHERE)\b", re.I)
PG_HINT = re.compile(r"\$\d+|::|\|\||\bIS\s+(?:NOT\s+)?DISTINCT\s+FROM\b|\bFILTER\s*\(|\bRETURNING\b|\bON\s+CONFLICT\b|=\s*ANY\b|\bARRAY\s*\[|\bUNNEST\s*\(|\bDISTINCT\s+ON\s*\(|\bLATERAL\b|\bxmax\b|\bpg_(?:try_)?advisory_\w+|\bLANGUAGE\s+plpgsql\b|\bCREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\b|\bnow\s*\(\s*\)|\b[\w.]+\s+\?\s*'|->>?", re.I)


@dataclass
class Literal:
    line: int
    text: str


def go_literals(source: str) -> list[Literal]:
    """Extract Go string literals that look like SQL, retaining source lines."""
    out = []
    for match in GO_STRING.finditer(source):
        raw = match.group()
        value = raw[1:-1] if raw.startswith("`") else ast.literal_eval(raw)
        if SQL_HINT.search(value) and PG_HINT.search(value):
            out.append(Literal(source.count("\n", 0, match.start()) + 1, value))
    return out


def _replace_concat(sql: str) -> tuple[str, int, bool]:
    # Operands are intentionally limited to identifiers, parameters, SQL
    # literals, and parenthesized expressions. Anything more complex remains
    # visible for hand review instead of being parsed with a risky guess.
    function_name = r"(?!(?i:AND|OR|NOT|WHERE|ON|WHEN|THEN|ELSE|IN|LIKE|SELECT|FROM|JOIN)\b)[A-Za-z_][\w.]*"
    function = rf"{function_name}\([^()]*\)"
    atom = rf"(?:\?|\$\d+|'(?:''|[^'])*'|{function}|[A-Za-z_][\w.]*|\((?:[^()]|\([^()]*\))*\))"
    pat = re.compile(rf"({atom})(?:\s*\|\|\s*({atom}))+", re.I)
    unsafe = False
    def sub(match: re.Match[str]) -> str:
        nonlocal unsafe
        before = sql[:match.start()].rstrip()
        after = sql[match.end():].lstrip()
        if re.search(r"(?:->>|->|[+*/%\-])$", before) or re.match(r"(?:->>|->|[+*/%\-])", after):
            unsafe = True
        # Capture chain operands without interpreting SQL nested in parentheses.
        chain = re.compile(rf"({atom})").findall(match.group())
        return "CONCAT(" + ", ".join(chain) + ")"
    rewritten, count = pat.subn(sub, sql)
    return rewritten, count, unsafe


def _rewrite_filter(sql: str) -> tuple[str, int]:
    # Supports the aggregate forms used by the store; nested SQL expressions
    # are left untouched and surfaced as TODO by the caller.
    pat = re.compile(r"\b(COUNT|SUM|AVG|MIN|MAX)\s*\(\s*([^()]*?)\s*\)\s*FILTER\s*\(\s*WHERE\s+([^()]*)\)", re.I)
    def sub(m: re.Match[str]) -> str:
        fn, value, cond = m.group(1).upper(), m.group(2).strip(), m.group(3).strip()
        if fn == "COUNT":
            if value == "*":
                return f"COUNT(CASE WHEN {cond} THEN 1 END)"
            return f"COUNT(CASE WHEN {cond} THEN {value} ELSE NULL END)"
        return f"{fn}(CASE WHEN {cond} THEN {value} ELSE NULL END)"
    return pat.subn(sub, sql)[0], len(pat.findall(sql))


def rewrite(sql: str) -> tuple[str | None, list[int], list[str], bool]:
    """Return mysql SQL, positional argument order, constructs, and now flag."""
    refused = [name for name, pattern in REFUSALS.items() if pattern.search(sql)]
    if refused:
        return None, [], refused, bool(re.search(r"\bnow\s*\(\s*\)", sql, re.I))

    constructs: list[str] = []
    order: list[int] = []
    def param(match: re.Match[str]) -> str:
        n = int(match.group(1))
        order.append(n)
        return "?"
    sql, n = re.subn(r"\$(\d+)", param, sql)
    if n:
        constructs.append("$n → ?")

    cast_type_map = {"jsonb": "JSON", "json": "JSON", "text": "CHAR", "int": "SIGNED", "int8": "SIGNED", "int4": "SIGNED", "bigint": "SIGNED", "integer": "SIGNED", "boolean": "SIGNED", "numeric": "DECIMAL", "timestamptz": "DATETIME(6)", "timestamp": "DATETIME(6)", "date": "DATE", "uuid": "CHAR(36)"}
    function_name = r"(?!(?i:AND|OR|NOT|WHERE|ON|WHEN|THEN|ELSE|IN|LIKE|SELECT|FROM|JOIN)\b)[A-Za-z_][\w.]*"
    cast_function = rf"{function_name}\([^()]*\)"
    cast_group = r"(?<![\w.)])\([^()]*\)"
    cast_atom = rf"(?:\?|\$\d+|'(?:''|[^'])*'|{cast_function}|[A-Za-z_][\w.]*|{cast_group})"
    cast = re.compile(rf"({cast_atom})\s*::\s*([A-Za-z_]\w*)", re.I)
    unsupported_cast = re.compile(r"::\s*(regclass|regtype|regproc|oid|interval)\b", re.I)
    if unsupported_cast.search(sql):
        return None, order, constructs + [f"unsupported ::{unsupported_cast.search(sql).group(1)} cast"], bool(re.search(r"\bnow\s*\(\s*\)", sql, re.I))
    for match in cast.finditer(sql):
        if match.group(1).startswith("(") and re.search(r"[A-Za-z_][\w.]*\s+$", sql[:match.start(1)]):
            return None, order, constructs + ["unsupported ::type function operand"], bool(re.search(r"\bnow\s*\(\s*\)", sql, re.I))
    sql, n = cast.subn(lambda m: f"CAST({m.group(1)} AS {cast_type_map.get(m.group(2).lower(), m.group(2))})", sql)
    if n:
        constructs.append("::type → CAST")

    sql, n, unsafe_concat_precedence = _replace_concat(sql)
    if n:
        constructs.append("|| → CONCAT")
    if "||" in sql or unsafe_concat_precedence:
        return None, order, constructs + ["unsupported || expression"], bool(re.search(r"\bnow\s*\(\s*\)", sql, re.I))

    distinct_function = rf"{function_name}\([^()]*\)"
    distinct_group = r"(?<![\w.)])\([^()]*\)"
    distinct_operand = rf"(?:\?|\$\d+|{distinct_function}|[\w.]+|{distinct_group})"
    distinct = re.compile(rf"({distinct_operand})\s+IS\s+(NOT\s+)?DISTINCT\s+FROM\s+({distinct_operand})", re.I)
    for match in distinct.finditer(sql):
        before = sql[:match.start(1)].rstrip()
        after = sql[match.end(3):].lstrip()
        if before and re.search(r"[=+*/%<>!|&-]$", before) or after and re.match(r"[=+*/%<>!|&-]", after):
            return None, order, constructs + ["unsupported IS [NOT] DISTINCT FROM expression"], bool(re.search(r"\bnow\s*\(\s*\)", sql, re.I))
    sql, n = distinct.subn(lambda m: f"{' ' if m.group(2) else 'NOT '}({m.group(1)} <=> {m.group(3)})".strip(), sql)
    if n:
        constructs.append("IS [NOT] DISTINCT FROM → <=>")
    if re.search(r"\bIS\s+(?:NOT\s+)?DISTINCT\s+FROM\b", sql, re.I):
        return None, order, constructs + ["unsupported IS [NOT] DISTINCT FROM expression"], bool(re.search(r"\bnow\s*\(\s*\)", sql, re.I))

    sql, n = _rewrite_filter(sql)
    if n:
        constructs.append("FILTER (WHERE) → SUM(CASE)")
    if re.search(r"\bFILTER\s*\(", sql, re.I):
        constructs.append("unsupported FILTER expression")
        return None, order, constructs, bool(re.search(r"\bnow\s*\(\s*\)", sql, re.I))

    # PostgreSQL uses object keys and array indexes; MySQL requires JSON paths.
    # Convert only simple keys and numeric indexes, refusing ambiguous paths.
    key_exists = re.compile(r"(\b[\w.]+)\s*\?\s*'([A-Za-z_][A-Za-z0-9_]*)'")
    sql, n = key_exists.subn(lambda m: f"JSON_CONTAINS_PATH({m.group(1)}, 'one', '$.{m.group(2)}')", sql)
    if n:
        constructs.append("? key-exists → JSON_CONTAINS_PATH")
    if re.search(r"\b[\w.]+\s*\?\s*'", sql):
        return None, order, constructs + ["unsupported ? key-exists key"], bool(re.search(r"\bnow\s*\(\s*\)", sql, re.I))
    arrow = re.compile(r"(\b[\w.]+)\s*(->>|->)\s*(?:'([A-Za-z_][A-Za-z0-9_]*)'|(\d+))")
    sql, n = arrow.subn(lambda m: f"{m.group(1)} {m.group(2)} '{'$.%s' % m.group(3) if m.group(3) else '$[%s]' % m.group(4)}'", sql)
    if n:
        constructs.append("jsonb ->/->> → JSON path")
    if re.search(r"\b[\w.]+\s*(?:->>|->(?!>))(?!\s*'\$(?:\.|\[))", sql):
        return None, order, constructs + ["unsupported JSON arrow path"], bool(re.search(r"\bnow\s*\(\s*\)", sql, re.I))
    flagged_now = bool(re.search(r"\bnow\s*\(\s*\)", sql, re.I))
    if flagged_now:
        constructs.append("now() [manual time audit]")
    return sql, order, constructs, flagged_now


def analyze(path: Path) -> tuple[list[dict], Counter]:
    source = path.read_text()
    results = []
    counts: Counter = Counter()
    for literal in go_literals(source):
        mysql, order, constructs, flagged_now = rewrite(literal.text)
        if not constructs and mysql is not None:
            continue
        results.append({"line": literal.line, "pg": literal.text, "mysql": mysql, "order": order, "constructs": constructs, "now": flagged_now})
        for construct in constructs:
            counts[("TODO" if mysql is None else "auto", construct)] += 1
    return results, counts


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("paths", nargs="+", type=Path, help="Go store file(s), or directories")
    parser.add_argument("--summary", action="store_true", help="print counts only")
    args = parser.parse_args()
    files = sorted({p for item in args.paths for p in ([item] if item.is_file() else item.rglob("*.go"))})
    total: Counter = Counter()
    for path in files:
        rows, counts = analyze(path)
        total.update(counts)
        if not args.summary:
            for row in rows:
                print(f"{path}:{row['line']} [{'; '.join(row['constructs']) or 'TODO unsupported'}]")
                if row["mysql"] is None:
                    print(f"TODO: {', '.join(row['constructs'])}")
                    print(f"PG: {row['pg']}")
                else:
                    print(f"s.q({json.dumps(row['pg'])}, {json.dumps(row['mysql'])})")
                    if row["order"]:
                        print(f"args: {row['order']}")
    print("Counts:")
    for (kind, construct), count in sorted(total.items()):
        print(f"{kind}\t{construct}\t{count}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

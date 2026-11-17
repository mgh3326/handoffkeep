#!/usr/bin/env python3
"""Report verified mechanical MySQL rewrites for PostgreSQL SQL in Go files."""

from __future__ import annotations

import argparse
import ast
import json
import re
import sys
from collections import Counter
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Iterable


@dataclass(frozen=True)
class Token:
    kind: str
    value: str
    start: int
    end: int


@dataclass(frozen=True)
class Literal:
    line: int
    text: str


@dataclass(frozen=True)
class Operand:
    first: int
    after: int
    start: int
    end: int
    kind: str
    name: str = ""


@dataclass(frozen=True)
class Construct:
    name: str
    detect: Callable[[list[Token]], bool]
    disposition: str
    residual: Callable[[list[Token]], bool] | None = None


class SQLTokenError(ValueError):
    pass


WORD_RE = re.compile(r"[A-Za-z_][A-Za-z0-9_$]*")
NUMBER_RE = re.compile(r"(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?")
OPERATORS = set("~!@#%^&|*+=<>/-:")
SQL_WORDS = {
    "AND", "OR", "NOT", "WHERE", "ON", "WHEN", "THEN", "ELSE", "END",
    "IN", "LIKE", "ILIKE", "IS", "BETWEEN", "SELECT", "FROM", "AS",
    "CASE", "DISTINCT", "JOIN", "LEFT", "RIGHT", "INNER", "OUTER",
    "CROSS", "SET", "VALUES", "RETURNING", "GROUP", "ORDER", "BY",
    "HAVING", "LIMIT", "OFFSET", "UNION", "ALL", "EXISTS", "NULL",
    "TRUE", "FALSE", "ESCAPE", "FILTER", "OVER", "PARTITION", "WINDOW",
}
CAST_TYPE_MAP = {
    "text": "CHAR",
    "int": "SIGNED",
    "integer": "SIGNED",
    "bigint": "SIGNED",
}


def lex(sql: str) -> list[Token]:
    """Tokenize SQL while preserving spans; strings/comments are opaque tokens."""
    out: list[Token] = []
    i = 0
    while i < len(sql):
        start = i
        ch = sql[i]
        if ch.isspace():
            i += 1
            while i < len(sql) and sql[i].isspace():
                i += 1
            out.append(Token("space", sql[start:i], start, i))
            continue
        if sql.startswith("--", i):
            i = sql.find("\n", i)
            if i < 0:
                i = len(sql)
            out.append(Token("comment", sql[start:i], start, i))
            continue
        if sql.startswith("/*", i):
            close = sql.find("*/", i + 2)
            if close < 0:
                raise SQLTokenError("unterminated SQL comment")
            i = close + 2
            out.append(Token("comment", sql[start:i], start, i))
            continue
        if ch == "'":
            i += 1
            while i < len(sql):
                if sql[i] == "'":
                    if i + 1 < len(sql) and sql[i + 1] == "'":
                        i += 2
                        continue
                    i += 1
                    break
                if sql[i] == "\\" and i + 1 < len(sql):
                    i += 2
                else:
                    i += 1
            else:
                raise SQLTokenError("unterminated string literal")
            out.append(Token("string", sql[start:i], start, i))
            continue
        if ch == '"':
            i += 1
            while i < len(sql):
                if sql[i] == '"':
                    if i + 1 < len(sql) and sql[i + 1] == '"':
                        i += 2
                        continue
                    i += 1
                    break
                i += 1
            else:
                raise SQLTokenError("unterminated quoted identifier")
            out.append(Token("quoted_identifier", sql[start:i], start, i))
            continue
        if ch == "$":
            delimiter = re.match(r"\$[A-Za-z_][A-Za-z0-9_]*\$|\$\$", sql[i:])
            if delimiter:
                marker = delimiter.group()
                close = sql.find(marker, i + len(marker))
                if close < 0:
                    raise SQLTokenError("unterminated dollar-quoted string")
                i = close + len(marker)
                out.append(Token("string", sql[start:i], start, i))
                continue
        if ch == "$" and i + 1 < len(sql) and sql[i + 1].isdigit():
            i += 2
            while i < len(sql) and sql[i].isdigit():
                i += 1
            out.append(Token("parameter", sql[start:i], start, i))
            continue
        match = WORD_RE.match(sql, i)
        if match:
            i = match.end()
            out.append(Token("word", sql[start:i], start, i))
            continue
        match = NUMBER_RE.match(sql, i)
        if match:
            i = match.end()
            out.append(Token("number", sql[start:i], start, i))
            continue
        if ch in "(),.":
            i += 1
            out.append(Token("punct", ch, start, i))
            continue
        if ch in OPERATORS or ch == "?":
            i += 1
            while i < len(sql) and sql[i] in OPERATORS:
                # A second dash starts a comment, not an operator run.
                if sql.startswith("--", i):
                    break
                i += 1
            out.append(Token("operator", sql[start:i], start, i))
            continue
        i += 1
        out.append(Token("other", sql[start:i], start, i))
    return out


def significant(tokens: Iterable[Token]) -> list[Token]:
    return [t for t in tokens if t.kind not in {"space", "comment"}]


def word(token: Token, value: str) -> bool:
    return token.kind == "word" and token.value.upper() == value


def has_word(tokens: list[Token], value: str) -> bool:
    return any(word(t, value) for t in tokens)


def has_sequence(tokens: list[Token], values: tuple[str, ...]) -> bool:
    for i in range(0, len(tokens) - len(values) + 1):
        if all(word(tokens[i + n], value) for n, value in enumerate(values)):
            return True
    return False


def matching_parens(tokens: list[Token]) -> tuple[dict[int, int], dict[int, int]]:
    opens: dict[int, int] = {}
    closes: dict[int, int] = {}
    stack: list[int] = []
    for i, token in enumerate(tokens):
        if token.kind == "punct" and token.value == "(":
            stack.append(i)
        elif token.kind == "punct" and token.value == ")":
            if not stack:
                raise SQLTokenError("unmatched closing parenthesis")
            start = stack.pop()
            opens[start] = i
            closes[i] = start
    if stack:
        raise SQLTokenError("unmatched opening parenthesis")
    return opens, closes


def _bare_identifier(tokens: list[Token], i: int) -> tuple[int, str] | None:
    if i >= len(tokens) or tokens[i].kind not in {"word", "quoted_identifier"}:
        return None
    start = i
    if tokens[i].kind == "word" and tokens[i].value.upper() in SQL_WORDS:
        return None
    parts = [tokens[i].value]
    i += 1
    while i + 1 < len(tokens) and tokens[i].kind == "punct" and tokens[i].value == "." and tokens[i + 1].kind in {"word", "quoted_identifier"}:
        parts.extend((".", tokens[i + 1].value))
        i += 2
    return i, "".join(parts)


def primary_at(tokens: list[Token], i: int, opens: dict[int, int], *, allow_group: bool = True) -> Operand | None:
    if i >= len(tokens):
        return None
    t = tokens[i]
    if t.kind in {"parameter", "string", "number", "quoted_identifier"}:
        return Operand(i, i + 1, t.start, t.end, t.kind, t.value)
    if t.kind == "operator" and t.value == "?":
        return Operand(i, i + 1, t.start, t.end, "placeholder", "?")
    ident = _bare_identifier(tokens, i)
    if ident:
        after_ident, name = ident
        if after_ident < len(tokens) and tokens[after_ident].kind == "punct" and tokens[after_ident].value == "(":
            opening = tokens[after_ident]
            if tokens[after_ident - 1].end == opening.start:
                close = opens.get(after_ident)
                if close is not None:
                    return Operand(i, close + 1, t.start, tokens[close].end, "function", name)
            else:
                return None
        last = tokens[after_ident - 1]
        return Operand(i, after_ident, t.start, last.end, "identifier", name)
    if allow_group and t.kind == "punct" and t.value == "(":
        close = opens.get(i)
        if close is None:
            return None
        if i > 0 and tokens[i - 1].kind == "punct" and tokens[i - 1].value == ")":
            return None
        if i > 0 and tokens[i - 1].kind in {"word", "quoted_identifier"} and tokens[i - 1].value.upper() not in SQL_WORDS:
            return None
        return Operand(i, close + 1, t.start, tokens[close].end, "group")
    return None


def primary_before(tokens: list[Token], end: int, opens: dict[int, int], closes: dict[int, int]) -> Operand | None:
    if end <= 0:
        return None
    last = tokens[end - 1]
    if last.kind == "punct" and last.value == ")":
        opening = closes.get(end - 1)
        if opening is None:
            return None
        # A call is one operand only when the opening parenthesis is adjacent to its name.
        ident_end = opening
        if ident_end > 0:
            j = ident_end - 1
            if tokens[j].kind in {"word", "quoted_identifier"}:
                start = j
                while start >= 2 and tokens[start - 1].kind == "punct" and tokens[start - 1].value == "." and tokens[start - 2].kind in {"word", "quoted_identifier"}:
                    start -= 2
                p = primary_at(tokens, start, opens)
                if p and p.after == end and tokens[ident_end - 1].end == tokens[opening].start:
                    return p
        return primary_at(tokens, opening, opens)
    i = end - 1
    if tokens[i].kind in {"word", "quoted_identifier"}:
        while i >= 2 and tokens[i - 1].kind == "punct" and tokens[i - 1].value == "." and tokens[i - 2].kind in {"word", "quoted_identifier"}:
            i -= 2
        return primary_at(tokens, i, opens)
    return primary_at(tokens, i, opens)


def apply_edits(sql: str, edits: list[tuple[int, int, str]]) -> str:
    for start, end, replacement in sorted(edits, reverse=True):
        sql = sql[:start] + replacement + sql[end:]
    return sql


def _has_words(tokens: list[Token], words: tuple[str, ...]) -> bool:
    return has_sequence(tokens, words)


def _has_function(tokens: list[Token], name: str) -> bool:
    for i in range(len(tokens) - 1):
        if tokens[i].kind == "word" and tokens[i].value.lower() == name and tokens[i + 1].kind == "punct" and tokens[i + 1].value == "(":
            return True
    return False


def _has_any_function(tokens: list[Token], prefixes: tuple[str, ...]) -> bool:
    return any(tokens[i].kind == "word" and tokens[i].value.lower().startswith(prefix) and i + 1 < len(tokens) and tokens[i + 1].kind == "punct" and tokens[i + 1].value == "(" for i in range(len(tokens) - 1) for prefix in prefixes)


def _has_array_construct(tokens: list[Token]) -> bool:
    if has_word(tokens, "ARRAY") or has_word(tokens, "UNNEST"):
        return True
    return any(word(tokens[i], "ANY") and i > 0 and tokens[i - 1].kind == "operator" and tokens[i - 1].value == "=" for i in range(len(tokens)))


def _has_data_modifying_cte(tokens: list[Token]) -> bool:
    for i, token in enumerate(tokens[:-3]):
        if not word(token, "AS") or tokens[i + 1].kind != "punct" or tokens[i + 1].value != "(":
            continue
        if i + 2 < len(tokens) and tokens[i + 2].kind == "word" and tokens[i + 2].value.upper() in {"INSERT", "UPDATE", "DELETE"}:
            return True
    return False


def _has_arrow(tokens: list[Token]) -> bool:
    return any(t.kind == "operator" and t.value in {"->", "->>"} for t in tokens)


def _has_invalid_arrow(tokens: list[Token]) -> bool:
    opens, closes = matching_parens(tokens)
    i = 0
    while i < len(tokens):
        if tokens[i].kind != "operator" or tokens[i].value not in {"->", "->>"}:
            i += 1
            continue
        left = primary_before(tokens, i, opens, closes)
        right = tokens[i + 1] if i + 1 < len(tokens) else None
        if left is None or left.kind != "identifier" or right is None:
            return True
        if right.kind == "string":
            key = right.value[1:-1].replace("''", "'")
            if key.startswith("$") or re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
                i += 2
                continue
            return True
        if right.kind == "number" and re.fullmatch(r"[0-9]+", right.value):
            i += 2
            continue
        return True
    return False


def _has_question_key(tokens: list[Token]) -> bool:
    if not any(t.kind == "operator" and t.value in {"?", "?|", "?&"} for t in tokens):
        return False
    try:
        opens, closes = matching_parens(tokens)
    except SQLTokenError:
        return any(
            t.kind == "operator" and t.value == "?" and i > 0 and i + 1 < len(tokens)
            and tokens[i - 1].kind in {"word", "quoted_identifier"}
            and tokens[i + 1].kind in {"string", "parameter", "word", "quoted_identifier"}
            for i, t in enumerate(tokens)
        )
    for i, token in enumerate(tokens[:-1]):
        if token.kind != "operator" or token.value != "?":
            continue
        left = primary_before(tokens, i, opens, closes)
        right = tokens[i + 1]
        if left is not None and left.kind in {"identifier", "quoted_identifier", "group", "function"} and right.kind in {"string", "parameter", "word", "quoted_identifier"}:
            return True
    return False


def _has_nonportable_operator(tokens: list[Token]) -> bool:
    safe = {"=", "<>", "!=", "<", ">", "<=", ">=", "+", "-", "*", "/", "%", "::", "||", "->", "->>", "?"}
    return any(t.kind == "operator" and t.value not in safe for t in tokens)


def _has_distinct(tokens: list[Token]) -> bool:
    for i, token in enumerate(tokens):
        if not word(token, "IS"):
            continue
        j = i + 1
        if j < len(tokens) and word(tokens[j], "NOT"):
            j += 1
        if j + 1 < len(tokens) and word(tokens[j], "DISTINCT") and word(tokens[j + 1], "FROM"):
            return True
    return False


def _has_filter(tokens: list[Token]) -> bool:
    return any(word(t, "FILTER") for t in tokens)


def _has_now(tokens: list[Token]) -> bool:
    return _has_function(tokens, "now")


def _has_plpgsql(tokens: list[Token]) -> bool:
    if _has_words(tokens, ("LANGUAGE", "PLPGSQL")):
        return True
    return any(word(t, "FUNCTION") for t in tokens) and any(word(t, "CREATE") for t in tokens)


def _has_advisory(tokens: list[Token]) -> bool:
    return any(t.kind == "word" and t.value.lower().startswith("pg_") and "advisory" in t.value.lower() for t in tokens)


def _has_interval_literal(tokens: list[Token]) -> bool:
    return any(word(tokens[i], "INTERVAL") and i + 1 < len(tokens) and tokens[i + 1].kind == "string" for i in range(len(tokens)))


def _has_regex_operator(tokens: list[Token]) -> bool:
    return any(t.kind == "operator" and t.value in {"~", "~*", "!~", "!~*"} for t in tokens)


def _has_nulls_order(tokens: list[Token]) -> bool:
    return has_sequence(tokens, ("NULLS", "FIRST")) or has_sequence(tokens, ("NULLS", "LAST"))


def _has_extract_epoch(tokens: list[Token]) -> bool:
    for i in range(len(tokens) - 2):
        if word(tokens[i], "EXTRACT") and tokens[i + 1].kind == "punct" and tokens[i + 1].value == "(" and word(tokens[i + 2], "EPOCH"):
            return True
    return False


def _has_is_bool(tokens: list[Token]) -> bool:
    return _has_words(tokens, ("IS", "TRUE")) or _has_words(tokens, ("IS", "FALSE"))


def _has_operator_sequence(tokens: list[Token], op: str) -> bool:
    return any(t.kind == "operator" and t.value == op for t in tokens)


def _has_type_cast(tokens: list[Token]) -> bool:
    return _has_operator_sequence(tokens, "::")


def _has_distinct_on(tokens: list[Token]) -> bool:
    return _has_words(tokens, ("DISTINCT", "ON"))


def _has_on_conflict(tokens: list[Token]) -> bool:
    return _has_words(tokens, ("ON", "CONFLICT"))


def _has_lateral(tokens: list[Token]) -> bool:
    return has_word(tokens, "LATERAL")


def _has_returning(tokens: list[Token]) -> bool:
    return has_word(tokens, "RETURNING")


def _has_xmax(tokens: list[Token]) -> bool:
    return has_word(tokens, "XMAX")


def _has_ilike(tokens: list[Token]) -> bool:
    return has_word(tokens, "ILIKE")


def _has_clock_timestamp(tokens: list[Token]) -> bool:
    return _has_function(tokens, "clock_timestamp")


def _has_mig_function(tokens: list[Token]) -> bool:
    return _has_any_function(tokens, ("generate_series", "to_jsonb", "array_agg", "string_agg", "date_trunc")) or _has_any_function(tokens, ("jsonb_",))


def _has_substring_from_regex(tokens: list[Token]) -> bool:
    for i in range(len(tokens) - 1):
        if word(tokens[i], "SUBSTRING") and i + 1 < len(tokens) and tokens[i + 1].kind == "punct" and tokens[i + 1].value == "(":
            depth = 0
            for j in range(i + 1, len(tokens)):
                if tokens[j].kind == "punct" and tokens[j].value == "(":
                    depth += 1
                elif tokens[j].kind == "punct" and tokens[j].value == ")":
                    depth -= 1
                    if depth == 0:
                        break
                elif depth == 1 and word(tokens[j], "FROM") and j + 1 < len(tokens) and tokens[j + 1].kind == "string":
                    return True
    return False


def _has_do_block(tokens: list[Token]) -> bool:
    if not has_word(tokens, "DO"):
        return False
    return any(t.kind == "string" and re.match(r"\$[A-Za-z_0-9]*\$", t.value) for t in tokens)


def _has_pg_type(tokens: list[Token]) -> bool:
    pg_types = {"SERIAL", "BIGSERIAL", "JSONB", "TIMESTAMPTZ", "BYTEA"}
    return any(
        t.kind == "word" and t.value.upper() in pg_types
        and not (i > 0 and tokens[i - 1].kind == "operator" and tokens[i - 1].value == "::")
        for i, t in enumerate(tokens)
    )


def _has_create_index_if_not_exists(tokens: list[Token]) -> bool:
    return _has_words(tokens, ("CREATE", "INDEX", "IF", "NOT", "EXISTS"))


def _has_quoted_identifier(tokens: list[Token]) -> bool:
    return any(token.kind == "quoted_identifier" for token in tokens)


def _has_backslash_string(tokens: list[Token]) -> bool:
    return any(token.kind == "string" and "\\" in token.value for token in tokens)


def _has_collate(tokens: list[Token]) -> bool:
    return has_word(tokens, "COLLATE")


def _has_unverified_cast_type(tokens: list[Token]) -> bool:
    for i, token in enumerate(tokens):
        if token.kind != "operator" or token.value != "::":
            continue
        if i + 1 >= len(tokens) or tokens[i + 1].kind != "word":
            return True
        if tokens[i + 1].value.lower() not in CAST_TYPE_MAP:
            return True
        after_type = i + 2
        if after_type < len(tokens) and (
            (tokens[after_type].kind == "punct" and tokens[after_type].value == "(")
            or (tokens[after_type].kind == "other" and tokens[after_type].value == "[")
            or (tokens[after_type].kind == "operator" and tokens[after_type].value == "::")
        ):
            return True
    return False


CONSTRUCTS = [
    Construct("$n → ?", lambda ts: any(t.kind == "parameter" for t in ts), "rewrite"),
    Construct("::type → CAST", _has_type_cast, "rewrite"),
    Construct("unverified ::type or cast modifier", _has_unverified_cast_type, "refuse"),
    Construct("|| concat or JSON merge", lambda ts: _has_operator_sequence(ts, "||"), "refuse"),
    Construct("IS [NOT] DISTINCT FROM", _has_distinct, "refuse"),
    Construct("FILTER", _has_filter, "refuse"),
    Construct("? JSON key/array operator", _has_question_key, "refuse"),
    Construct("nonportable PostgreSQL operator", _has_nonportable_operator, "refuse"),
    Construct("JSON ->/->> arrows", _has_arrow, "refuse", _has_invalid_arrow),
    Construct("now() [manual time audit]", _has_now, "refuse", lambda ts: False),
    Construct("RETURNING", _has_returning, "refuse"),
    Construct("ON CONFLICT", _has_on_conflict, "refuse"),
    Construct("= ANY / arrays / unnest", _has_array_construct, "refuse"),
    Construct("DISTINCT ON", _has_distinct_on, "refuse"),
    Construct("LATERAL", _has_lateral, "refuse"),
    Construct("data-modifying CTE", _has_data_modifying_cte, "refuse"),
    Construct("xmax", _has_xmax, "refuse"),
    Construct("advisory locks", _has_advisory, "refuse"),
    Construct("plpgsql", _has_plpgsql, "refuse"),
    Construct("ILIKE", _has_ilike, "refuse"),
    Construct("regex match operators", _has_regex_operator, "refuse"),
    Construct("NULLS FIRST/LAST", _has_nulls_order, "refuse"),
    Construct("interval literal", _has_interval_literal, "refuse"),
    Construct("clock_timestamp()", _has_clock_timestamp, "refuse"),
    Construct("generate_series/jsonb_* and other PostgreSQL functions", _has_mig_function, "refuse"),
    Construct("EXTRACT(EPOCH)", _has_extract_epoch, "refuse"),
    Construct("IS TRUE/IS FALSE", _has_is_bool, "refuse"),
    Construct("SIMILAR TO", lambda ts: _has_words(ts, ("SIMILAR", "TO")), "refuse"),
    Construct("substring FROM regex", _has_substring_from_regex, "refuse"),
    Construct("DO block", _has_do_block, "refuse"),
    Construct("PostgreSQL-only type spelling", _has_pg_type, "refuse"),
    Construct("CREATE INDEX IF NOT EXISTS", _has_create_index_if_not_exists, "refuse"),
    Construct("double-quoted identifier", _has_quoted_identifier, "refuse"),
    Construct("backslash in string literal", _has_backslash_string, "refuse"),
    Construct("COLLATE", _has_collate, "refuse"),
]


def present(tokens: list[Token]) -> list[Construct]:
    return [construct for construct in CONSTRUCTS if construct.detect(tokens)]


def _rewrite_casts(sql: str) -> tuple[str, str | None, int]:
    count = 0
    for _ in range(100):
        tokens = significant(lex(sql))
        opens, closes = matching_parens(tokens)
        for i, token in enumerate(tokens):
            if token.kind != "operator" or token.value != "::":
                continue
            operand = primary_before(tokens, i, opens, closes)
            if operand is None:
                return sql, "unsupported ::type operand", count
            if operand.kind not in {"identifier", "parameter", "string", "number"}:
                return sql, "unsupported ::type primary", count
            cursor = i
            expr = sql[operand.start:operand.end]
            while cursor < len(tokens) and tokens[cursor].kind == "operator" and tokens[cursor].value == "::":
                if cursor + 1 >= len(tokens) or tokens[cursor + 1].kind != "word":
                    return sql, "unsupported ::type cast", count
                type_name = tokens[cursor + 1].value.lower()
                after_type = cursor + 2
                target = CAST_TYPE_MAP.get(type_name)
                if after_type < len(tokens) and (
                    (tokens[after_type].kind == "punct" and tokens[after_type].value == "(")
                    or (tokens[after_type].kind == "other" and tokens[after_type].value == "[")
                ):
                    return sql, f"unsupported ::{type_name} cast modifier", count
                if target is None:
                    return sql, f"unsupported ::{type_name} cast", count
                if after_type < len(tokens) and tokens[after_type].kind == "operator" and tokens[after_type].value == "::":
                    return sql, "unsupported multiple ::type casts", count
                expr = f"CAST({expr} AS {target})"
                cursor = after_type
            sql = sql[:operand.start] + expr + sql[tokens[cursor - 1].end:]
            count += 1
            break
        else:
            return sql, None, count
    return sql, "unsupported nested ::type casts", count


def _replace_parameters(sql: str) -> tuple[str, list[int]]:
    tokens = significant(lex(sql))
    edits: list[tuple[int, int, str]] = []
    order: list[int] = []
    for token in tokens:
        if token.kind == "parameter":
            order.append(int(token.value[1:]))
            edits.append((token.start, token.end, "?"))
    return apply_edits(sql, edits), order


def _rewrite(sql: str) -> tuple[str | None, list[int], list[str], bool]:
    try:
        original_tokens = significant(lex(sql))
    except SQLTokenError as exc:
        return None, [], [f"TODO unlexable SQL: {exc}"], False
    detected = present(original_tokens)
    if not detected:
        return sql, [], [], False
    now_flag = any(c.name == "now() [manual time audit]" for c in detected)
    refusal = [c.name for c in detected if c.disposition == "refuse"]
    if refusal:
        return None, [], refusal, now_flag
    try:
        sql, error, _ = _rewrite_casts(sql)
        if error:
            return None, [], ["::type → CAST", f"TODO: {error}"], now_flag
        sql, order = _replace_parameters(sql)
        final_tokens = significant(lex(sql))
    except SQLTokenError as exc:
        return None, [], [f"TODO unlexable SQL: {exc}"], now_flag
    remaining = [
        c.name for c in present(final_tokens)
        if c.disposition == "refuse" or (c.residual and c.residual(final_tokens))
    ]
    if remaining:
        return None, order, remaining, now_flag
    applied = [c.name for c in detected if c.disposition in {"rewrite", "flag"}]
    return sql, order, applied, now_flag


def rewrite(sql: str) -> tuple[str | None, list[int], list[str], bool]:
    return _rewrite(sql)


def _is_sql(tokens: list[Token]) -> bool:
    return any(word(t, w) for t in tokens for w in ("SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "WITH", "FROM", "WHERE"))


def go_string_literals(source: str) -> list[tuple[int, str]]:
    """Return real Go string literals, skipping comments and rune literals."""
    out: list[tuple[int, str]] = []
    i = 0
    while i < len(source):
        if source.startswith("//", i):
            newline = source.find("\n", i + 2)
            i = len(source) if newline < 0 else newline + 1
            continue
        if source.startswith("/*", i):
            close = source.find("*/", i + 2)
            i = len(source) if close < 0 else close + 2
            continue
        quote = source[i]
        if quote == "`":
            end = source.find("`", i + 1)
            if end < 0:
                break
            out.append((i, source[i:end + 1]))
            i = end + 1
            continue
        if quote in {"\"", "'"}:
            start = i
            i += 1
            while i < len(source):
                if source[i] == "\\":
                    i += 2
                    continue
                if source[i] == quote:
                    i += 1
                    break
                i += 1
            else:
                break
            if quote == "\"":
                out.append((start, source[start:i]))
            continue
        i += 1
    return out


def go_literals(source: str) -> list[Literal]:
    out: list[Literal] = []
    for start, raw in go_string_literals(source):
        try:
            value = raw[1:-1] if raw.startswith("`") else ast.literal_eval(raw)
            tokens = significant(lex(value))
        except (SyntaxError, ValueError, SQLTokenError):
            continue
        if _is_sql(tokens) and present(tokens):
            out.append(Literal(source.count("\n", 0, start) + 1, value))
    return out


def analyze(path: Path) -> tuple[list[dict], Counter]:
    source = path.read_text()
    results: list[dict] = []
    counts: Counter = Counter()
    for literal in go_literals(source):
        mysql, order, constructs, flagged_now = rewrite(literal.text)
        if flagged_now and "now() [manual time audit]" not in constructs:
            constructs.append("now() [manual time audit]")
        row = {"line": literal.line, "pg": literal.text, "mysql": mysql, "order": order, "constructs": constructs, "now": flagged_now}
        results.append(row)
        for construct in set(constructs):
            if construct.startswith("TODO:"):
                continue
            counts[("TODO" if mysql is None else "auto", construct)] += 1
    return results, counts


def _balanced(tokens: list[Token]) -> bool:
    try:
        matching_parens(tokens)
        return True
    except SQLTokenError:
        return False


def _corpus_auto_violations(root: Path) -> list[str]:
    violations: list[str] = []
    for path in sorted((root / "internal/store").glob("*.go")):
        rows, _ = analyze(path)
        for row in rows:
            if row["mysql"] is None:
                continue
            try:
                tokens = significant(lex(row["mysql"]))
            except SQLTokenError as exc:
                violations.append(f"{path}:{row['line']}: unlexable auto SQL: {exc}")
                continue
            residual = [
                c.name for c in present(tokens)
                if c.name != "now() [manual time audit]"
            ]
            residual.extend(c.name for c in CONSTRUCTS if c.residual and c.residual(tokens))
            if residual:
                violations.append(f"{path}:{row['line']}: residual {', '.join(residual)}")
            if not _balanced(tokens):
                violations.append(f"{path}:{row['line']}: unbalanced parentheses")
            if re.search(r"[A-Za-z_](?:CONCAT|CAST)\(|\)\s*\(", row["mysql"]):
                violations.append(f"{path}:{row['line']}: mangled call boundary")
    return violations


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("paths", nargs="+", type=Path, help="Go store file(s), or directories")
    parser.add_argument("--summary", action="store_true", help="print counts only")
    args = parser.parse_args()
    files: list[Path] = []
    for path in args.paths:
        if not path.exists():
            print(f"sqlport: no such path: {path}", file=sys.stderr)
            return 1
        if path.is_file():
            files.append(path)
        elif path.is_dir():
            files.extend(path.rglob("*.go"))
        else:
            print(f"sqlport: not a file or directory: {path}", file=sys.stderr)
            return 1
    total: Counter = Counter()
    for path in sorted(set(files)):
        rows, counts = analyze(path)
        total.update(counts)
        if not args.summary:
            for row in rows:
                print(f"{path}:{row['line']} [{'; '.join(row['constructs'])}]")
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

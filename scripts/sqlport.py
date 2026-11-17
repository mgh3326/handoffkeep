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
    "text": "CHAR", "varchar": "CHAR", "char": "CHAR",
    "json": "JSON", "jsonb": "JSON",
    "int": "SIGNED", "int2": "SIGNED", "int4": "SIGNED", "int8": "SIGNED",
    "smallint": "SIGNED", "integer": "SIGNED", "bigint": "SIGNED",
    "bool": "SIGNED", "boolean": "SIGNED",
    "numeric": "DECIMAL", "decimal": "DECIMAL",
    "float4": "DOUBLE", "float8": "DOUBLE", "real": "DOUBLE", "double": "DOUBLE",
    "date": "DATE", "timestamp": "DATETIME(6)", "timestamptz": "DATETIME(6)",
    "uuid": "CHAR(36)",
}
CAST_MULTI_TYPE_MAP = {
    ("double", "precision"): "DOUBLE",
    ("character", "varying"): "CHAR",
    ("timestamp", "with", "time", "zone"): "DATETIME(6)",
    ("timestamp", "without", "time", "zone"): "DATETIME(6)",
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
            while i < len(sql) and (sql[i] in OPERATORS or sql[i] == "?"):
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


def _contains_string_or_text_cast(tokens: list[Token], operand: Operand) -> tuple[bool, bool]:
    inner = tokens[operand.first:operand.after]
    has_json_cast = False
    has_text = operand.kind == "string"
    for i, token in enumerate(inner[:-1]):
        if word(token, "AS") and inner[i + 1].kind == "word":
            cast_type = inner[i + 1].value.lower()
            if cast_type == "json":
                has_json_cast = True
            if cast_type in {"char", "varchar"}:
                has_text = True
    if operand.kind == "function" and operand.name.lower() in {"concat", "json_unquote"}:
        has_text = True
    if operand.kind == "group" and any(t.kind == "string" for t in inner):
        has_text = True
    return has_text, has_json_cast


def _allowed_neighbor(token: Token | None, *, concat: bool) -> bool:
    if token is None:
        return True
    if token.kind == "punct" and token.value in {"(", ")", ","}:
        return True
    if token.kind == "word":
        allowed = {
            "SELECT", "FROM", "WHERE", "ON", "WHEN", "THEN", "ELSE", "AS", "SET",
            "AND", "OR", "NOT", "LIKE", "ILIKE", "IN", "BETWEEN", "IS",
            "GROUP", "ORDER", "HAVING", "LIMIT", "OFFSET", "UNION",
        }
        return token.value.upper() in allowed
    if token.kind == "operator":
        comparisons = {"=", "<>", "!=", "<", ">", "<=", ">="}
        return concat and token.value in comparisons
    return False


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
    for i, token in enumerate(tokens[:-1]):
        if token.kind == "operator" and token.value == "?" and tokens[i + 1].kind == "string":
            return True
    return False


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


CONSTRUCTS = [
    Construct("$n → ?", lambda ts: any(t.kind == "parameter" for t in ts), "rewrite"),
    Construct("::type → CAST", _has_type_cast, "rewrite"),
    Construct("|| → CONCAT", lambda ts: _has_operator_sequence(ts, "||"), "rewrite"),
    Construct("IS [NOT] DISTINCT FROM → <=>", _has_distinct, "rewrite"),
    Construct("FILTER → CASE", _has_filter, "rewrite"),
    Construct("? key-exists → JSON_CONTAINS_PATH", _has_question_key, "rewrite"),
    Construct("jsonb ->/->> → JSON path", _has_arrow, "rewrite", _has_invalid_arrow),
    Construct("now() [manual time audit]", _has_now, "flag", lambda ts: False),
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
            cursor = i
            expr = sql[operand.start:operand.end]
            while cursor < len(tokens) and tokens[cursor].kind == "operator" and tokens[cursor].value == "::":
                if cursor + 1 >= len(tokens) or tokens[cursor + 1].kind != "word":
                    return sql, "unsupported ::type cast", count
                type_name = tokens[cursor + 1].value.lower()
                after_type = cursor + 2
                multi = next((
                    (spellings, mapped)
                    for spellings, mapped in CAST_MULTI_TYPE_MAP.items()
                    if spellings[0] == type_name
                    and all(cursor + 1 + offset < len(tokens)
                            and tokens[cursor + 1 + offset].kind == "word"
                            and tokens[cursor + 1 + offset].value.lower() == part
                            for offset, part in enumerate(spellings))
                ), None)
                if multi:
                    spellings, target = multi
                    after_type = cursor + 1 + len(spellings)
                else:
                    target = CAST_TYPE_MAP.get(type_name)
                if after_type < len(tokens) and (
                    (tokens[after_type].kind == "punct" and tokens[after_type].value == "(")
                    or (tokens[after_type].kind == "other" and tokens[after_type].value == "[")
                ):
                    return sql, f"unsupported ::{type_name} cast modifier", count
                if target is None:
                    return sql, f"unsupported ::{type_name} cast", count
                expr = f"CAST({expr} AS {target})"
                cursor = after_type
            sql = sql[:operand.start] + expr + sql[tokens[cursor - 1].end:]
            count += 1
            break
        else:
            return sql, None, count
    return sql, "unsupported nested ::type casts", count


def _rewrite_json_arrows(sql: str) -> tuple[str, str | None, int]:
    tokens = significant(lex(sql))
    opens, closes = matching_parens(tokens)
    edits: list[tuple[int, int, str]] = []
    for i, token in enumerate(tokens):
        if token.kind != "operator" or token.value not in {"->", "->>"}:
            continue
        left = primary_before(tokens, i, opens, closes)
        right = tokens[i + 1] if i + 1 < len(tokens) else None
        if left is None or left.kind != "identifier" or right is None:
            return sql, "unsupported JSON arrow operand", 0
        if right.kind == "string":
            key = right.value[1:-1].replace("''", "'")
            if not re.fullmatch(r"\$(?:\.[A-Za-z_][A-Za-z0-9_]*|\[[0-9]+\])*", key):
                if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
                    return sql, "unsupported JSON arrow key", 0
                key = f"$.{key}"
            path = key
        elif right.kind == "number" and re.fullmatch(r"[0-9]+", right.value):
            path = f"$[{right.value}]"
        else:
            return sql, "unsupported JSON arrow path", 0
        extractor = f"JSON_EXTRACT({sql[left.start:left.end]}, '{path}')"
        if token.value == "->>":
            extractor = f"JSON_UNQUOTE({extractor})"
        edits.append((left.start, right.end, extractor))
    return apply_edits(sql, edits), None, len(edits)


def _rewrite_key_exists(sql: str) -> tuple[str, str | None, int]:
    tokens = significant(lex(sql))
    opens, closes = matching_parens(tokens)
    edits: list[tuple[int, int, str]] = []
    for i, token in enumerate(tokens):
        if token.kind != "operator" or token.value != "?":
            continue
        left = primary_before(tokens, i, opens, closes)
        if left is None or left.kind not in {"identifier", "group", "function"} or (left.kind == "function" and left.name.upper() != "JSON_EXTRACT") or i + 1 >= len(tokens) or tokens[i + 1].kind != "string":
            return sql, "unsupported ? key-exists expression", 0
        key = tokens[i + 1].value[1:-1].replace("''", "'")
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
            return sql, "unsupported ? key-exists key", 0
        replacement = f"JSON_CONTAINS_PATH({sql[left.start:left.end]}, 'one', '$.{key}')"
        edits.append((left.start, tokens[i + 1].end, replacement))
    return apply_edits(sql, edits), None, len(edits)


def _rewrite_concat(sql: str) -> tuple[str, str | None, int]:
    total = 0
    for _ in range(100):
        tokens = significant(lex(sql))
        opens, closes = matching_parens(tokens)
        for op_i, op in enumerate(tokens):
            if op.kind != "operator" or op.value != "||":
                continue
            for opening, closing in opens.items():
                if opening < op_i < closing:
                    before = opening - 1
                    after = closing + 1
                    if ((before >= 0 and tokens[before].kind == "operator" and tokens[before].value == "||") or
                            (after < len(tokens) and tokens[after].kind == "operator" and tokens[after].value == "||")):
                        return sql, "unsupported nested || boundary", total
        replaced = False
        for i, token in enumerate(tokens):
            if token.kind != "operator" or token.value != "||":
                continue
            left = primary_before(tokens, i, opens, closes)
            if left is None:
                return sql, "unsupported || operand", total
            operands = [left]
            last = i
            cursor = i
            while True:
                right = primary_at(tokens, cursor + 1, opens)
                if right is None:
                    return sql, "unsupported || operand", total
                operands.append(right)
                last = right.after - 1
                next_op = right.after
                if next_op >= len(tokens) or tokens[next_op].kind != "operator" or tokens[next_op].value != "||":
                    break
                cursor = next_op
            first = left.first
            before = tokens[first - 1] if first > 0 else None
            after = tokens[last + 1] if last + 1 < len(tokens) else None
            if not _allowed_neighbor(before, concat=True) or not _allowed_neighbor(after, concat=True):
                return sql, "unsupported || precedence boundary", total
            text_flags, json_flags = zip(*(_contains_string_or_text_cast(tokens, operand) for operand in operands))
            if any(json_flags) or not any(text_flags[:2]):
                return sql, "|| on non-text operands (jsonb merge?)", total
            replacement = "CONCAT(" + ", ".join(sql[o.start:o.end] for o in operands) + ")"
            sql = sql[:left.start] + replacement + sql[tokens[last].end:]
            total += 1
            replaced = True
            break
        if not replaced:
            return sql, None, total
    return sql, "unsupported nested || chain", total


def _rewrite_distinct(sql: str) -> tuple[str, str | None, int]:
    tokens = significant(lex(sql))
    opens, closes = matching_parens(tokens)
    edits: list[tuple[int, int, str]] = []
    i = 0
    while i < len(tokens):
        if not word(tokens[i], "IS"):
            i += 1
            continue
        start = i
        cursor = i + 1
        is_not = False
        if cursor < len(tokens) and word(tokens[cursor], "NOT"):
            is_not = True
            cursor += 1
        if cursor + 1 >= len(tokens) or not word(tokens[cursor], "DISTINCT") or not word(tokens[cursor + 1], "FROM"):
            i += 1
            continue
        left = primary_before(tokens, start, opens, closes)
        right = primary_at(tokens, cursor + 2, opens)
        if left is None or right is None:
            return sql, "unsupported IS [NOT] DISTINCT FROM operand", 0
        before = tokens[left.first - 1] if left.first > 0 else None
        after = tokens[right.after] if right.after < len(tokens) else None
        tighter_words = {"LIKE", "ILIKE", "SIMILAR", "IN", "BETWEEN"}
        tighter_ops = {"~", "~*", "!~", "!~*", "@", "#", "^"}
        if (
            not _allowed_neighbor(before, concat=False)
            or not _allowed_neighbor(after, concat=False)
            or (before is not None and (before.value.upper() in tighter_words if before.kind == "word" else before.value in tighter_ops))
            or (after is not None and (after.value.upper() in tighter_words if after.kind == "word" else after.value in tighter_ops))
        ):
            return sql, "unsupported IS [NOT] DISTINCT FROM boundary", 0
        expr = f"({sql[left.start:left.end]} <=> {sql[right.start:right.end]})"
        if not is_not:
            expr = "NOT " + expr
        edits.append((left.start, right.end, expr))
        i = right.after
    return apply_edits(sql, edits), None, len(edits)


def _rewrite_filter(sql: str) -> tuple[str, str | None, int]:
    tokens = significant(lex(sql))
    opens, closes = matching_parens(tokens)
    edits: list[tuple[int, int, str]] = []
    for i, token in enumerate(tokens):
        if not word(token, "FILTER"):
            continue
        if i == 0 or tokens[i - 1].kind != "punct" or tokens[i - 1].value != ")":
            return sql, "unsupported FILTER expression", 0
        aggregate_close = i - 1
        aggregate_open = closes.get(aggregate_close)
        if aggregate_open is None or aggregate_open == 0 or tokens[aggregate_open - 1].kind != "word":
            return sql, "unsupported FILTER aggregate", 0
        fn = tokens[aggregate_open - 1].value.upper()
        if fn not in {"COUNT", "SUM", "AVG", "MIN", "MAX"}:
            return sql, "unsupported FILTER aggregate", 0
        filter_open = i + 1
        if filter_open >= len(tokens) or tokens[filter_open].kind != "punct" or tokens[filter_open].value != "(":
            return sql, "unsupported FILTER expression", 0
        filter_close = opens.get(filter_open)
        if filter_close is None or filter_open + 1 >= filter_close or not word(tokens[filter_open + 1], "WHERE"):
            return sql, "unsupported FILTER expression", 0
        arg_tokens = tokens[aggregate_open + 1:aggregate_close]
        if arg_tokens and word(arg_tokens[0], "DISTINCT"):
            return sql, "unsupported DISTINCT FILTER aggregate", 0
        argument = sql[tokens[aggregate_open].end:tokens[aggregate_close].start].strip()
        condition = sql[tokens[filter_open + 1].end:tokens[filter_close].start].strip()
        if not condition:
            return sql, "unsupported empty FILTER condition", 0
        if fn == "COUNT" and argument == "*":
            case = f"COUNT(CASE WHEN {condition} THEN 1 END)"
        else:
            case = f"{fn}(CASE WHEN {condition} THEN {argument} END)"
        edits.append((tokens[aggregate_open - 1].start, tokens[filter_close].end, case))
    return apply_edits(sql, edits), None, len(edits)


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
    applied: list[str] = []
    transforms = [
        (_rewrite_casts, "::type → CAST"),
        (_rewrite_json_arrows, "jsonb ->/->> → JSON path"),
        (_rewrite_key_exists, "? key-exists → JSON_CONTAINS_PATH"),
        (_rewrite_concat, "|| → CONCAT"),
        (_rewrite_distinct, "IS [NOT] DISTINCT FROM → <=>"),
        (_rewrite_filter, "FILTER → CASE"),
    ]
    for transform, label in transforms:
        try:
            result = transform(sql)
        except SQLTokenError as exc:
            return None, [], [f"TODO unlexable SQL: {exc}"], now_flag
        sql, error, count = result
        if error:
            related = [c.name for c in detected if c.disposition == "rewrite"]
            return None, [], [*related, f"TODO: {error}"], now_flag
        if count:
            applied.append(label)
    try:
        sql, order = _replace_parameters(sql)
        final_tokens = significant(lex(sql))
    except SQLTokenError as exc:
        return None, [], [f"TODO unlexable SQL: {exc}"], now_flag
    remaining = [c.name for c in CONSTRUCTS if c.residual and c.residual(final_tokens)]
    if remaining:
        return None, order, remaining, now_flag
    if now_flag:
        applied.append("now() [manual time audit]")
    # Report constructs from the same registry that drives refusal and residual
    # checks, including successful rewrites whose spelling did not need edits.
    applied = [c.name for c in detected if c.disposition in {"rewrite", "flag"}]
    return sql, order, applied, now_flag


def rewrite(sql: str) -> tuple[str | None, list[int], list[str], bool]:
    return _rewrite(sql)


def _is_sql(tokens: list[Token]) -> bool:
    return any(word(t, w) for t in tokens for w in ("SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "WITH", "FROM", "WHERE"))


def go_literals(source: str) -> list[Literal]:
    out: list[Literal] = []
    for match in re.finditer(r"`[^`]*`|\"(?:\\[^\n]|[^\"\\\n])*\"", source, re.S):
        raw = match.group()
        try:
            value = raw[1:-1] if raw.startswith("`") else ast.literal_eval(raw)
            tokens = significant(lex(value))
        except (SyntaxError, ValueError, SQLTokenError):
            continue
        if _is_sql(tokens) and present(tokens):
            out.append(Literal(source.count("\n", 0, match.start()) + 1, value))
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
            if re.search(r"[A-Za-z_](?:CONCAT|CAST)\(|\)\(", row["mysql"]):
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

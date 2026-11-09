# hk 3536 — search engine bench (report)

Status: **local engines measured on the oci host (§3); database engines: MySQL query construction fixed and rerun pending, Oracle connect fixed and rerun pending (§6).** The timings that count are the oci run's: `run-all.sh` on the oci host (2 cores, linux arm64), with the local and DB engines on the same host. The development-host (macOS arm64) numbers in §3 are a quality cross-check only; they are not timings.

## 1. Corpus

Exported with the handoffkeep CLI (`handoffkeep` v0.0.113, rev `c77449dc`), 2026-11-08. Files are JSONL under `corpus/` and are **not in git** (see `.gitignore`); `corpus/manifest.json` records the counts.

| set | rows | bytes | notes |
|---|---:|---:|---|
| tasks | 3,536 | 1,285,356 | `tasks export --limit 10000`, `complete=true`; fields id, title, state, project |
| comments | 12,859 | 7,650,276 | `tasks comments <id>` for every task; 2,189 tasks have comments |
| documents | 11,075 | 109,371,714 | all documents; report 4,665, note 3,122, brief 2,515, answer 391, other 318, handoff 63, decision 1 |

Method notes:
- `tasks export` default limit is 1000; the full set needed `--limit 10000`.
- `doc list` returns at most 100 rows with no paging. Documents were enumerated by prefix recursion (split a saturated prefix by the next key character, with LIKE metacharacters escaped), then bodies fetched with `doc get --id`. Cross-check: all 4,342 documents found by a separate id scan (ids up to 30,000) are in the enumerated set.
- A record is one task (title plus its comments) or one document (key plus body). Gold keys are `task:<id>` and `doc:<id>`.
- The document set includes this bench's own brief documents (ids 61683, 61686). They contain the query text, so gold for `배포검증` and `deploy-ok 검증` includes them.
- Corpus path: the job brief named `~/work/searchbench/bench/searchbench/corpus/`. The corpus was written to `bench/searchbench/corpus/` inside this worktree instead, because the brief says to keep everything under `bench/searchbench/`. It is not committed, so it must be copied to the oci host with the scripts.

## 2. Queries

`queries.jsonl`: 30 queries, built by `tools/gen_queries.py` from the corpus. 27 have gold; 3 are no-answer.

- **Curated** gold: the record the query was written from. Used for most queries (title phrases, spacing and particle variants, English terms in Korean text, English-only titles, document keys and headings).
- **Lexical** gold: every record whose text contains the stated string (`배포를`, `배포가`, `배포검증`, `deploy-ok` with `검증`, `캡틴`).
- Categories cover the brief's list: particles (`배포를`, `배포가`, `결정 답변 경로를`, `워커 정지를 감지하는`), compounds (`배포검증`, `태스크작성규칙`), 2-character queries (`캡틴`, `AT 재시작 계획`'s `AT`), spacing variants (`배포 검증`, `적대 검증`, `태스크 작성 규칙`), English terms inside Korean (`deploy-ok 검증`, `panewire 허브 chat`), and three no-answer queries (`바나나 우유 재고 정산`, `해저 케이블 절단 복구`, `김치냉장고 온도 센서 교정`).

Metrics (top 10 keys per query):
- `hit@10`: some gold key is in the top 10 (answered queries only).
- `recall@10`: |gold ∩ top10| / min(|gold|, 10), averaged over answered queries.
- `MRR@10`: 1 / rank of the first gold key, averaged over answered queries.
- `no-answer with hits`: no-answer queries that returned at least one result.
- Latency: 3 warm-up runs, then 100 timed runs per query (3,000 samples per engine and mode).

## 3. Results — local engines (oci host, same host as the DB engines)

Source: the oci rerun at `fa5a4f8` (2026-11-09, `run-all.sh`, 2 cores, linux arm64; the local-engine rows of its `out/all/summary.md`). Ingest includes index creation; index size is the tantivy directory or the SQLite file; latency is 3 warm-ups and 100 timed runs per query. The local engines do not depend on the MySQL or Oracle changes.

| engine | mode | ingest s | index MB | hit@10 | recall@10 | MRR@10 | no-answer with hits | p50 ms | p95 ms |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| tantivy-default (`default` tokenizer) | or-bm25 | 2.2 | 36.2 | 0.630 | 0.584 | 0.411 | 3 / 3 | 0.04 | 0.11 |
| tantivy-lindera-ko-dic (Normal, no POS filter) | or-bm25 | 7.6 | 52.5 | 0.630 | 0.548 | 0.336 | 3 / 3 | 0.08 | 0.19 |
| sqlite-fts5-unicode61 | or-bm25 | 4.0 | 165.1 | 0.667 | 0.589 | 0.430 | 3 / 3 | 1.05 | 10.51 |
| sqlite-fts5-trigram | or-bm25 | 26.3 | 410.2 | 0.519 | 0.519 | 0.300 | 1 / 3 | 0.44 | 12.96 |

Development-host cross-check (macOS arm64). Quality metrics only; these should match the oci run, since retrieval does not depend on the host. Latency and ingest are omitted because they came from a different machine:

| engine | index MB | hit@10 | recall@10 | MRR@10 | no-answer with hits |
|---|---:|---:|---:|---:|---:|
| tantivy-default | 40.2 | 0.630 | 0.584 | 0.405 | 3 / 3 |
| tantivy-lindera-ko-dic | 56.2 | 0.630 | 0.548 | 0.354 | 3 / 3 |
| sqlite-fts5-unicode61 | 165.1 | 0.667 | 0.589 | 0.430 | 3 / 3 |
| sqlite-fts5-trigram | 410.2 | 0.519 | 0.519 | 0.300 | 1 / 3 |

The 27 answered queries give a granularity of 1/27 ≈ 0.037 on hit@10.

## 4. Per-query misses (answered queries with no gold in the top 10)

Taken from the development host. The oci rerun (`out/all/summary.md`, `fa5a4f8`) gives the same miss list for each of the four local engines.

- **tantivy-default** (10 misses): q02 `태스크작성규칙`, q04 `결정 답변 경로를`, q07 `워커 정지를 감지하는`, q09 `적대 검증`, q10 `parked-car 자산 리뷰`, q11 `스폰 선택 시뮬레이션`, q16 `AT 재시작 계획`, q21 `배포 검증`, q22 `deploy-ok 검증`, q26 `fleet-ci-three-tiers`.
- **tantivy-lindera-ko-dic** (10 misses): q04, q07, q09, q10, q11, q16, q18 `배포를`, q19 `배포가`, q23 `캡틴`, q26.
- **sqlite-fts5-unicode61** (9 misses): q02, q04, q07, q09, q10, q11, q16, q21, q26.
- **sqlite-fts5-trigram** (13 misses): q01 `태스크 작성 규칙`, q02, q04, q05 `panewire 허브 chat`, q06 `워커 정지 감지`, q07, q11, q12 `rt-parity probe grade A+ T1`, q14 `body_doc 연결 보정`, q16, q21, q22, q26.

Observations (facts from the runs):
- Trigram drops tokens shorter than 3 characters from MATCH. q01 `태스크 작성 규칙` therefore matches on `태스크` alone (`작성`, `규칙` are 2 characters). q02 `태스크작성규칙` is one token with no spaces, which is not a substring of the spaced title. Trigram's no-answer hit (q29) came from `케이블` alone; `해저`, `절단`, `복구` are 2 characters.
- Unicode61 and the default tantivy tokenizer do not segment Hangul, so `배포를` and `배포가` match documents containing that exact token. q18 and q19 hit with both (their gold tasks contain those exact strings).
- Lindera segments particles as separate tokens (no POS filter in this variant). q18 `배포를` and q19 `배포가` miss with lindera but hit with the default tokenizer, and q23 `캡틴` (15 gold tasks) misses in the top 10 with lindera. The POS-filtered variant was not run.
- q26 `fleet-ci-three-tiers`: gold `doc:60861` is outside the top 10 for all four local engines. For tantivy-default the top three are sibling documents `60864`–`60866`.
- The three no-answer queries return 10 results each under OR-BM25 (tantivy, unicode61) because a single token matches. Under trigram the only result came from one 3-character token.

Full per-query lists are in `out/local/summary.md` (not committed; regenerate with `searchbench summarize`).

## 5. Access-path findings

- **Rust ↔ MySQL HeatWave:** `mysql` 28.0.4 with `default-features = false` and features `buffer-pool`, `minimal-rust` (pure-Rust flate2, no zlib) and `native-tls`. TLS is OpenSSL on Linux, so the oci host needs `libssl-dev` and `pkg-config` to build. `MYSQL_SSL_CA` is the trust root. Why native-tls and not rustls: on the oci run (2026-11-09) HeatWave served an X.509 v1 certificate and rustls/webpki rejected it (`UnsupportedCertVersion`); OpenSSL accepts v1 certificates. `MYSQL_SSL=disabled` turns TLS off and is for the in-VCN private address only. Not yet run against HeatWave.
- **Rust ↔ Oracle ADB:** `oracle` 0.6.3 (rust-oracle, ODPI-C) with `--features oracle`. It compiles on macOS arm64 without Oracle Instant Client, because ODPI-C loads the client library at run time. Running needs Instant Client on the library path (`LD_LIBRARY_PATH` on the oci host) and the ADB wallet: `TNS_ADMIN=$RB_WALLET_DIR`. The connect string is a full TCPS descriptor built from `RB_DB_HOST`, `RB_DB_PORT` and `RB_DB_SERVICE` with `SSL_SERVER_DN_MATCH=yes`. `RB_DB_SERVICE` is a service name, not a wallet alias: the first oci run (fa5a4f8, run 2) failed with ORA-12154 when it was passed as an alias. Connect errors have the connect values redacted. CLOB bodies are written through `oracle::sql_type::Clob::new` and `std::io::Write`. No ORDS path was built.
- **Oracle Text:** one table and one CONTEXT index per lexer (`SB_KM` / `KOREAN_MORPH_LEXER`, `SB_BASIC` / `BASIC_LEXER`, `SB_WORLD` / `WORLD_LEXER`). Queries are `{term} OR {term}` with `SCORE(1)`. Whether ADB exposes `KOREAN_MORPH_LEXER` is not yet verified.
- **Build:** the workspace builds in release mode with and without `--features oracle`. Scripts use `CARGO_BUILD_JOBS=2` by default.

## 6. Database engines — pending

Not yet run against HeatWave or ADB. `run-db.sh` runs each DB engine in its own process, so one failure does not stop the others. A failed engine is written to `<OUT>/<engine>__failed.json` and listed under "Failures" in `summary.md`. Results from an earlier run of that engine are removed first, so a failed rerun cannot show stale numbers. RUNS is 30 for DB engines by default (`run-all.sh` takes `RUNS_DB`; local engines keep 100). Isolation was checked with closed-port dummy settings and no database: all four engines failed at connect, each was recorded, and the run finished with the Failures section.

The DB run needs these environment variables (names only; values are never printed): `MYSQL_HOST MYSQL_PORT MYSQL_ADMIN_USER MYSQL_ADMIN_PASSWORD RB_DB_HOST RB_DB_PORT RB_DB_SERVICE RB_DB_USER RB_DB_PASSWORD RB_WALLET_DIR`, plus optional `MYSQL_SSL` (`required`, the default, or `disabled`) and `MYSQL_SSL_CA`.

Objects created and dropped by the run:
- MySQL database `searchbench` (table `sb_docs`, FULLTEXT ngram index).
- ADB tables `SB_KM`, `SB_BASIC`, `SB_WORLD` and their CONTEXT indexes in the `RB_DB_USER` schema.

Run 2 on oci (`fa5a4f8`, `MYSQL_SSL=disabled`, 2026-11-09 00:00–00:11 UTC). **These numbers are not valid results; they are kept as the record of why the adapters changed.** MySQL connected and ran; the Oracle engines did not connect (ORA-12154, service name used as an alias; fixed to a descriptor).

| engine | mode | ingest s | index MB | hit@10 | recall@10 | MRR@10 | no-answer with hits | p50 ms | p95 ms | errors |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| mysql-heatwave-ngram (run 2) | boolean | 440.0 | 140.8 | 0.148 | 0.148 | 0.053 | 0 / 3 | 6.72 | 241.06 | 6 |
| mysql-heatwave-ngram (run 2) | natural | 440.0 | 140.8 | 0.000 | 0.000 | 0.000 | 3 / 3 | 18.73 | 200.38 | 0 |
| oracle-text-korean-morph | or-score | | | | | | | | | connect failed |
| oracle-text-basic | or-score | | | | | | | | | connect failed |
| oracle-text-world | or-score | | | | | | | | | connect failed |

Run 2 causes found in the adapter (fixed in the next commit, to be rerun):
- **Boolean operators were parsed.** Tokens went into BOOLEAN MODE unquoted, so `-`, `+` and `"` in the text were operators. Now each token is a quoted phrase, `+"deploy-ok"`, so it tokenizes the way the body did.
- **One-character tokens cannot match.** The server's `ngram_token_size` is the default 2, so a token like `A` can never be indexed, and in an AND query it empties the result (q12 `rt-parity probe grade A+ T1` has `A`, the only one-character token in the set). Tokens with fewer letters or digits than the server's ngram size are now dropped from BOOLEAN queries, as the trigram adapter drops its short tokens. The size is read from the server at build time and written to the setup notes.
- **The 6 errors are not identified yet.** The run-2 summary had no error text. The summary now lists the first five errors per engine and mode, and the setup notes, so the rerun shows them.
- **Natural mode is not yet explained.** Its zero hit rate and 3/3 no-answer hits (the words match some document) may be engine behaviour (InnoDB relevance favours long documents, and natural mode ORs every bigram). This is a hypothesis; the rerun's top-10 lists must confirm it before it is reported as a finding.

MySQL modes: `boolean` requires every token as a quoted phrase (`+"tok"`); `natural` is the raw query in NATURAL LANGUAGE MODE (its 50% row threshold applies).

## 7. Not measured

- NoSQL: no full-text search; skipped as the brief says. No kv latency (no credentials).
- Setup burden is described in §5 and in the crate notes; no separate timing.

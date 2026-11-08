# hk 3536 — search engine bench (report)

Status: **local engines measured; database engines built and not yet run.** The timings that count come from the OCI run (`run-all.sh` on the oci host, 2 cores, linux arm64). Latency numbers in this file are from the development host (macOS arm64) and are not the final timings.

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

## 3. Results — local engines (development host)

| engine | mode | ingest s | index MB | hit@10 | recall@10 | MRR@10 | no-answer with hits | p50 ms | p95 ms |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| tantivy-default (`default` tokenizer) | or-bm25 | 1.0 | 40.2 | 0.630 | 0.584 | 0.405 | 3 / 3 | 0.05 | 0.16 |
| tantivy-lindera-ko-dic (Normal, no POS filter) | or-bm25 | 1.6 | 56.2 | 0.630 | 0.548 | 0.354 | 3 / 3 | 0.07 | 0.21 |
| sqlite-fts5-unicode61 | or-bm25 | 2.4 | 165.1 | 0.667 | 0.589 | 0.430 | 3 / 3 | 0.53 | 5.38 |
| sqlite-fts5-trigram | or-bm25 | 14.0 | 410.2 | 0.519 | 0.519 | 0.300 | 1 / 3 | 0.20 | 7.92 |

Ingest includes index creation. Index size is the tantivy directory or the SQLite file. The 27 answered queries give a granularity of 1/27 ≈ 0.037 on hit@10.

## 4. Per-query misses (answered queries with no gold in the top 10)

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

- **Rust ↔ MySQL HeatWave:** `mysql` 28.0.4 with `default-features = false` and features `minimal-rust` (pure-Rust flate2) and `rustls-tls-ring`. With default features the build failed (`flate2` has no backend selected). TLS uses the default verifier; if the HeatWave certificate chain is not in the webpki roots, set `MYSQL_SSL_CA` to a CA bundle path. Not yet run against HeatWave.
- **Rust ↔ Oracle ADB:** `oracle` 0.6.3 (rust-oracle, ODPI-C) with `--features oracle`. It compiles on macOS arm64 without Oracle Instant Client, because ODPI-C loads the client library at run time. Running needs Instant Client on the library path (`LD_LIBRARY_PATH` on the oci host) and the ADB wallet: `TNS_ADMIN=$RB_WALLET_DIR` and the service name as the connect string. CLOB bodies are written through `oracle::sql_type::Clob::new` and `std::io::Write`. No ORDS path was built. Not yet run against ADB.
- **Oracle Text:** one table and one CONTEXT index per lexer (`SB_KM` / `KOREAN_MORPH_LEXER`, `SB_BASIC` / `BASIC_LEXER`, `SB_WORLD` / `WORLD_LEXER`). Queries are `{term} OR {term}` with `SCORE(1)`. Whether ADB exposes `KOREAN_MORPH_LEXER` is not yet verified.
- **Build:** the workspace builds in release mode with and without `--features oracle`. Scripts use `CARGO_BUILD_JOBS=2` by default.

## 6. Database engines — pending

Not yet run. `run-db.sh` and `run-all.sh` are in place and were checked for missing-environment refusal only. The DB run needs these environment variables (names only; values are never printed): `MYSQL_HOST MYSQL_PORT MYSQL_ADMIN_USER MYSQL_ADMIN_PASSWORD RB_DB_HOST RB_DB_PORT RB_DB_SERVICE RB_DB_USER RB_DB_PASSWORD RB_WALLET_DIR`, plus optional `MYSQL_SSL_CA`.

Objects created and dropped by the run:
- MySQL database `searchbench` (table `sb_docs`, FULLTEXT ngram index).
- ADB tables `SB_KM`, `SB_BASIC`, `SB_WORLD` and their CONTEXT indexes in the `RB_DB_USER` schema.

Results to be filled in from `out/db/summary.md` after the oci run:

| engine | mode | ingest s | index MB | hit@10 | recall@10 | MRR@10 | no-answer with hits | p50 ms | p95 ms |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| mysql-heatwave-ngram | boolean | | | | | | | | |
| mysql-heatwave-ngram | natural | | | | | | | | |
| oracle-text-korean-morph | or-score | | | | | | | | |
| oracle-text-basic | or-score | | | | | | | | |
| oracle-text-world | or-score | | | | | | | | |

MySQL modes: `boolean` requires every whitespace token (`+tok`); `natural` is the raw query in NATURAL LANGUAGE MODE (its 50% row threshold applies).

## 7. Not measured

- NoSQL: no full-text search; skipped as the brief says. No kv latency (no credentials).
- Setup burden is described in §5 and in the crate notes; no separate timing.

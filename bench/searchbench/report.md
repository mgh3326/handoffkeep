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
- **Rust ↔ YouTrack (remote, read-only):** `ureq` 3 with native-tls (OpenSSL, same as MySQL), GET requests only, bearer token from `YOUTRACK_API_KEY` in a header. Search is `/api/issues`, then `/api/articles` to fill up to k. Terms are braced, so `-` and `:` are literal. An issue maps to `task:<hk ID>` and an article to `doc:<hk ID>` through the `hk ID` field (`YOUTRACK_HK_FIELD`, default `hk ID`), the field `internal/youtrack` writes. Coverage: one query on `{hk ID}: N` per gold key. The `docs` mirror is not on yet, so doc coverage is expected to be low.
- **Rust ↔ hk production (remote, read-only):** the hk CLI as a subprocess, `handoffkeep ctx search <q> --scope <S> --limit 10`, parsing the JSON rows. Task rows map to `task:<key>`; document rows map to `doc:<id>` through the document key, which is the first line of each document record. Coverage: `handoffkeep tasks show <id>` and `handoffkeep doc get --id <id>`; exit 0 means the record exists and `not_found` means it does not. `HK_BIN` selects the binary; the CLI keeps its own configuration.
- **Finding to confirm on v0.0.136 (from the source at this branch):** the store's search for scope `all` covers ctx, memory and docs only. Tasks come back only from scope `tasks`. So mode `all` can never return a task gold key, whatever its ranking. Mode `tasks` is measured as well, to show what the task gold can reach.
- **Build:** the workspace builds in release mode with and without `--features oracle`. Scripts use `CARGO_BUILD_JOBS=2` by default.

## 6. Database engines — pending

Not yet run against HeatWave or ADB. `run-db.sh` runs each DB engine in its own process, so one failure does not stop the others. A failed engine is written to `<OUT>/<engine>__failed.json` and listed under "Failures" in `summary.md`. Results from an earlier run of that engine are removed first, so a failed rerun cannot show stale numbers. RUNS is 30 for DB engines by default (`run-all.sh` takes `RUNS_DB`; local engines keep 100). Isolation was checked with closed-port dummy settings and no database: all four engines failed at connect, each was recorded, and the run finished with the Failures section.

The DB run needs these environment variables (names only; values are never printed): `MYSQL_HOST MYSQL_PORT MYSQL_ADMIN_USER MYSQL_ADMIN_PASSWORD RB_DB_HOST RB_DB_PORT RB_DB_SERVICE RB_DB_USER RB_DB_PASSWORD RB_WALLET_DIR`, plus optional `MYSQL_SSL` (`required`, the default, or `disabled`) and `MYSQL_SSL_CA`.

Objects created and dropped by the run:
- MySQL database `searchbench` (table `sb_docs`, FULLTEXT ngram index).
- ADB tables `SB_KM`, `SB_BASIC`, `SB_WORLD` and their CONTEXT indexes in the `RB_DB_USER` schema.
- ADB lexer preference `SB_KM_LEX` (KOREAN_MORPH_LEXER), created before the `SB_KM` index and dropped after it.

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

Run 3 on oci (`54cc29a`, the operator's run, summary kept with the handoff). **Not valid as DB results.** It found these harness faults, fixed in the next commit:
- **youtrack** ended in a panic: `uri scheme is https, provider is Rustls but feature is not enabled: rustls`. ureq's default TLS provider is rustls, which this build does not compile. The agent now pins native-tls. A regression test reproduces the panic without the pin and passes with it.
- **oracle-text-korean-morph** did not build: `DRG-10700: preference does not exist: CTXSYS.KOREAN_MORPH_LEXER`. ADB has no such preference. The bench now creates `SB_KM_LEX` (KOREAN_MORPH_LEXER) in its own schema before the index and drops it after the index exists.
- **hk-pg all** had 29 unmapped results in the top 10. Checked: the unmapped documents (for example `review/2026-11-09/yt-linear-transition-status`, `briefs/hk3654-search-match.md`) are absent from the corpus snapshot. The snapshot was generated 2026-11-08 07:05 UTC and has no 2026-11-09 keys, so these are documents newer than the corpus. Memory and ctx results have no corpus record by design. The summary now splits unmapped results by scope (docs not in corpus, memory, ctx, other).

Still open. The query construction was checked by reading the code; no harness fault was found in it. The rerun's `--smoke` self-probe decides whether the cause is in the index or in ranking.
- **oracle-text-basic and oracle-text-world, hit@10 0.074.** Each token is `{tok}`, joined with OR, ordered by SCORE(1). tantivy-default uses the same whitespace split of Hangul and scores 0.630, so the tokenizer alone does not explain the gap. The build now reads back about 25 evenly spaced stored bodies and fails if one differs from its record.
- **mysql-heatwave-ngram boolean, hit@10 0.185, 6 errors.** The errors are `ERROR 188: FTS query exceeds result cache limit`, a server limit, not a parse error. The five printed are q12, q14, q17, q24 and q26, and each has a hyphen or underscore identifier (`rt-parity`, `body_doc`, `decision_table_validate`, `stalldetect`, `fleet-ci-three-tiers`). An errored query scores as a miss, so 0.185 is a lower bound.
- **mysql-heatwave-ngram natural, hit@10 0.000.** The query is the raw text against the same indexed column and key as boolean. One candidate, not verified: InnoDB natural mode ignores terms that appear in more than half the rows.

Run 3 numbers, status as above:

| engine | mode | hit@10 | errors | status |
|---|---|---:|---:|---|
| mysql-heatwave-ngram | boolean | 0.185 | 6 | open; errors count as misses |
| mysql-heatwave-ngram | natural | 0.000 | 0 | open |
| oracle-text-korean-morph | or-score | — | — | build failed (DRG-10700); fixed |
| oracle-text-basic | or-score | 0.074 | 0 | open |
| oracle-text-world | or-score | 0.074 | 0 | open |
| youtrack | native | — | — | panicked (TLS); fixed |
| hk-pg | all | 0.074 | 1 | live data, newer than the corpus; 29 unmapped in top 10 |
| hk-pg | tasks | 0.481 | 0 | live data; coverage 46 of 46 gold keys exist |

The one hk-pg error is the production search timing out (`context deadline exceeded`) on q23 (`캡틴`). The hk-pg numbers are live results and are not directly comparable with the local snapshot, because the corpus is older than the production search.

Before the next full run, `./run-db.sh --smoke` builds each engine, runs one query per mode (top 3 printed, gold in top 10 yes/no), and, for local engines, self-probes with the first two words of a task title, which must find that task. It writes no results and exits non-zero if any engine fails. Build time is still paid (MySQL ingest is minutes), but the timed query runs are not.

Smoke on oci at `f958e32` (`./run-db.sh --smoke`; 4 of 6 engines failed; log kept with the handoff):
- **oracle-text-korean-morph, basic, world:** the self-probe (`smoke: queue`, an OR of two words) did not find task:1 in the top 10. That probe is weak: under OR, any record with `queue` competes, so a miss does not show whether the index holds the word. The probe is now one word that occurs in no other record, and the smoke prints how many records the engine matches for it (`matches N`). Zero matches means the index or query text does not reach the word; more than zero with a miss means ranking. The query path reads the indexed column BODY, the probe uses the same `text_query` as search, and the query orders by `SCORE(1)` on the same CONTAINS label, so the code path matches the design. Which of the two it is, is for the next oci smoke to say.
- **mysql-heatwave-ngram:** the probe failed with `ERROR 188: FTS query exceeds result cache limit`. A word that is common in the corpus (its bigrams appear in many long documents) has a result set that exceeds `innodb_ft_result_cache_limit`. The new probe is a rare word, which keeps the probe inside the limit. The real queries are unchanged. `LIMIT` cannot bound this: the cache is filled while the match is evaluated. Restricting the query changes what is measured, and raising the limit is a server setting (not changed here). The report now gives hit@10 both with errored queries counted as misses and without them, so the cost is visible.
- **youtrack:** `q01`'s gold `task:629` is absent (expected: the Python mirror is the only content today), and its top hit `HK-A-17` was an article with no hk ID, so it stayed unmapped. Articles now map to `doc:<id>` by, in order: the hk ID field; an optional field named by `YOUTRACK_DOC_KEY_FIELD`; the first corpus document key named in the article's summary, then its content. Doc coverage checks an article that names the document key, then the hk ID field. Verified against a loopback fake whose article names a corpus key (mapped, coverage present, gold hit). **Not verified against YouTrack:** that the mirror's articles name their document key. If they do not, they stay unmapped, and the mapping convention is the one thing the operator must confirm. The full YouTrack import (hk 3670) will change the contents, so the fair YouTrack run waits for it.
- **hk-pg:** fine (the tasks mode finds task:629 first).

Remote engines (read-only; added by the operator's scope change; run 3 is above):
- `youtrack` (mode `native`) and `hk-pg` (modes `all`, `tasks`) report two things. **Coverage:** how many of the gold keys exist in that system at all. **Subset:** the scores on answered queries with at least one gold key present, using only the present gold keys. All-answered scores are reported too, so a missing mirror is not read as bad ranking.
- Unmapped results (a YouTrack issue with no hk ID, or an hk ctx/memory row) keep their rank as `unmapped:<id>`, and the count per engine is reported.
- Checked with a local fake YouTrack server and a fake hk CLI only (no YouTrack or production calls). The plumbing, coverage and subset accounting ran end to end; the numbers from the fakes are meaningless and are not results.

MySQL modes: `boolean` requires every token as a quoted phrase (`+"tok"`); `natural` is the raw query in NATURAL LANGUAGE MODE (its 50% row threshold applies).

## 7. Not measured

- NoSQL: no full-text search; skipped as the brief says. No kv latency (no credentials).
- Remote latency is not comparable to the local or DB latency. The YouTrack numbers include the network round trip, and the hk-pg numbers include starting the CLI for every query. Quality is the measure for the remote engines.
- Setup burden is described in §5 and in the crate notes; no separate timing.

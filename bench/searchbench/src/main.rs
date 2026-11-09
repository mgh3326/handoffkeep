//! searchbench: one harness, one adapter per engine, the same corpus and queries.
//!
//!   searchbench run local [--only a,b] [--runs N] [--out DIR] [--work DIR]
//!   searchbench run db    [--only a,b] [--runs N] [--out DIR] [--work DIR]
//!   searchbench run all   ...
//!   searchbench run db --smoke [--only a]   (build, one query per mode, no results written)
//!   searchbench summarize [--out DIR]
//!
//! `run local` needs no network. `run db` needs the database settings in the
//! environment (see run-db.sh) and creates/drops only the bench objects.
//! Results: <out>/<engine>__<mode>.json and <out>/summary.md.

mod corpus;
mod engines;
mod metrics;

use anyhow::{anyhow, bail, Context, Result};
use corpus::{Query, Record};
use engines::{mysql_engine::MysqlEngine, sqlite_engine, tantivy_engine, Engine};
use serde_json::{json, Value};
use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::time::Instant;

const K: usize = 10;
const WARMUP: usize = 3;

struct Opts {
    corpus: PathBuf,
    queries: PathBuf,
    out: PathBuf,
    work: PathBuf,
    runs: usize,
    only: Option<Vec<String>>,
    smoke: bool,
}

fn parse_opts(args: &[String]) -> Result<Opts> {
    let mut o = Opts {
        corpus: "corpus".into(),
        queries: "queries.jsonl".into(),
        out: "out".into(),
        work: "work".into(),
        runs: 100,
        only: None,
        smoke: false,
    };
    let mut it = args.iter();
    while let Some(flag) = it.next() {
        let mut val = || it.next().cloned().ok_or_else(|| anyhow!("{flag} needs a value"));
        match flag.as_str() {
            "--smoke" => o.smoke = true,
            "--corpus" => o.corpus = val()?.into(),
            "--queries" => o.queries = val()?.into(),
            "--out" => o.out = val()?.into(),
            "--work" => o.work = val()?.into(),
            "--runs" => o.runs = val()?.parse().context("--runs")?,
            "--only" => o.only = Some(val()?.split(',').map(str::to_string).collect()),
            other => bail!("unknown flag {other}"),
        }
    }
    Ok(o)
}

/// Names only; values are never printed.
const DB_ENV: &[&str] = &[
    "MYSQL_HOST", "MYSQL_PORT", "MYSQL_ADMIN_USER", "MYSQL_ADMIN_PASSWORD",
    "RB_DB_HOST", "RB_DB_PORT", "RB_DB_SERVICE", "RB_DB_USER", "RB_DB_PASSWORD",
];

fn check_db_env() -> Result<()> {
    let missing: Vec<&str> = DB_ENV.iter().copied().filter(|n| std::env::var(n).is_err()).collect();
    if !missing.is_empty() {
        bail!("missing database environment: {}", missing.join(", "));
    }
    Ok(())
}

fn engines_for(group: &str, work: &Path) -> Result<Vec<Box<dyn Engine>>> {
    let local: Vec<Box<dyn Engine>> = vec![
        Box::new(tantivy_engine::TantivyEngine::new(tantivy_engine::Variant::Default, work)),
        Box::new(tantivy_engine::TantivyEngine::new(tantivy_engine::Variant::LinderaKoDic, work)),
        Box::new(sqlite_engine::SqliteEngine::new(sqlite_engine::Variant::Unicode61, work)),
        Box::new(sqlite_engine::SqliteEngine::new(sqlite_engine::Variant::Trigram, work)),
    ];
    #[allow(unused_mut)]
    let mut db: Vec<Box<dyn Engine>> = vec![Box::new(MysqlEngine::new())];
    #[cfg(feature = "oracle")]
    for lex in [
        engines::oracle_engine::Lexer::KoreanMorph,
        engines::oracle_engine::Lexer::Basic,
        engines::oracle_engine::Lexer::World,
    ] {
        db.push(Box::new(engines::oracle_engine::OracleEngine::new(lex)));
    }
    #[cfg(not(feature = "oracle"))]
    eprintln!("note: built without --features oracle; Oracle Text engines are skipped");
    // Remote systems (read-only): YouTrack REST and the hk production search.
    db.push(Box::new(engines::youtrack_engine::YoutrackEngine::new()));
    db.push(Box::new(engines::hk_pg_engine::HkPgEngine::new()));
    match group {
        "local" => Ok(local),
        "db" => Ok(db),
        "all" => Ok(local.into_iter().chain(db).collect()),
        other => bail!("unknown group {other} (local|db|all)"),
    }
}

fn run(args: &[String]) -> Result<()> {
    let group = args.first().context("usage: run local|db|all")?.as_str();
    let o = parse_opts(&args[1..])?;
    if group != "local" {
        check_db_env()?;
    }
    fs::create_dir_all(&o.out)?;
    fs::create_dir_all(&o.work)?;

    let t = Instant::now();
    let recs: Vec<Record> = corpus::load_records(&o.corpus)?;
    let queries: Vec<Query> = corpus::load_queries(&o.queries)?;
    eprintln!("corpus: {} records, {} queries (loaded in {:.1}s)", recs.len(), queries.len(), t.elapsed().as_secs_f64());

    let mut smoke_failures = 0;
    for mut engine in engines_for(group, &o.work)? {
        let name = engine.name();
        if let Some(only) = &o.only {
            if !only.iter().any(|n| n == name) {
                continue;
            }
        }
        eprintln!("[{name}] build");
        let t = Instant::now();
        if let Err(e) = engine.build(&recs) {
            // One failing engine must not stop the others: record it and move on.
            let msg = format!("{e:#}");
            eprintln!("[{name}] FAILED build: {msg}");
            if o.smoke {
                println!("[{name}] SMOKE FAILED (build): {msg}");
                smoke_failures += 1;
                continue;
            }
            let doc = json!({ "engine": name, "stage": "build", "error": msg });
            fs::write(o.out.join(format!("{name}__failed.json")), serde_json::to_string_pretty(&doc)?)?;
            continue;
        }
        let ingest_s = t.elapsed().as_secs_f64();
        let index_bytes = engine.index_bytes().unwrap_or(0);
        eprintln!("[{name}] built in {ingest_s:.1}s, index {index_bytes} bytes");
        if o.smoke {
            if let Err(e) = smoke_engine(engine.as_mut(), &recs, &queries) {
                println!("[{name}] SMOKE FAILED: {e:#}");
                smoke_failures += 1;
            }
            continue;
        }
        let coverage = measure_coverage(engine.as_mut(), &queries);
        if let Some(c) = &coverage {
            let exist = c.exists.values().filter(|v| **v == Some(true)).count();
            eprintln!("[{name}] coverage: {exist} of {} gold keys exist in the system", c.exists.len());
        }

        for mode in engine.modes() {
            eprintln!("[{name}/{mode}] measuring {} queries", queries.len());
            let mut per_query = Vec::new();
            let mut errors = Vec::new();
            let mut lat_us: Vec<u64> = Vec::new();
            for q in &queries {
                let mut errored = false;
                let top = match engine.search(mode, &q.text, K) {
                    Ok(v) => v,
                    Err(e) => {
                        errored = true;
                        errors.push(format!("{}: {e:#}", q.id));
                        Vec::new()
                    }
                };
                let s = metrics::score(&q.gold, &top);
                // Subset: only the gold keys that exist in the system (remote engines).
                let present: Vec<String> = match &coverage {
                    Some(c) => q.gold.iter().filter(|g| c.exists.get(*g) == Some(&Some(true))).cloned().collect(),
                    None => Vec::new(),
                };
                let sub = if coverage.is_some() && !present.is_empty() { metrics::score(&present, &top) } else { None };
                let unmapped = top.iter().filter(|k| k.starts_with("unmapped:")).count();
                let unmapped_scope = unmapped_by_scope(&top);
                for _ in 0..WARMUP {
                    let _ = engine.search(mode, &q.text, K);
                }
                for _ in 0..o.runs {
                    let t = Instant::now();
                    let _ = engine.search(mode, &q.text, K);
                    lat_us.push(t.elapsed().as_micros() as u64);
                }
                per_query.push(json!({
                    "id": q.id,
                    "text": q.text,
                    "category": q.category,
                    "gold": q.gold,
                    "gold_basis": q.gold_basis,
                    "answered": !q.gold.is_empty(),
                    "returned": top.len(),
                    "top10": top,
                    "hit": s.as_ref().map(|s| s.hit),
                    "mrr": s.as_ref().map(|s| s.mrr),
                    "recall": s.as_ref().map(|s| s.recall),
                    "first_rank": s.as_ref().and_then(|s| s.first_rank),
                    "gold_present": if coverage.is_some() { json!(present.len()) } else { Value::Null },
                    "subset_hit": sub.as_ref().map(|s| s.hit),
                    "subset_mrr": sub.as_ref().map(|s| s.mrr),
                    "subset_recall": sub.as_ref().map(|s| s.recall),
                    "unmapped_in_top10": unmapped,
                    "unmapped_by_scope": unmapped_scope,
                    "error": errored,
                }));
            }
            let answered: Vec<&Value> = per_query.iter().filter(|p| p["answered"] == true).collect();
            let n = answered.len().max(1) as f64;
            let mean = |f: &str| {
                answered
                    .iter()
                    .map(|p| p[f].as_f64().or_else(|| p[f].as_bool().map(|b| b as u8 as f64)).unwrap_or(0.0))
                    .sum::<f64>()
                    / n
            };
            let no_answer_hits = per_query.iter().filter(|p| p["answered"] == false && p["returned"].as_u64().unwrap_or(0) > 0).count();
            lat_us.sort_unstable();
            let mean_us = lat_us.iter().sum::<u64>() as f64 / lat_us.len().max(1) as f64;
            let subset_rows: Vec<&Value> = per_query.iter().filter(|p| p["subset_hit"].is_boolean()).collect();
            let coverage_json = match &coverage {
                Some(c) => {
                    let missing: Vec<&String> = c.exists.iter().filter(|(_, v)| **v == Some(false)).map(|(k, _)| k).collect();
                    json!({
                        "gold_keys": c.exists.len(),
                        "exist": c.exists.values().filter(|v| **v == Some(true)).count(),
                        "missing": missing.len(),
                        "unknown": c.exists.values().filter(|v| v.is_none()).count(),
                        "missing_keys": missing,
                        "errors": c.errors.iter().take(10).collect::<Vec<_>>(),
                    })
                }
                None => Value::Null,
            };
            let subset_json = if coverage.is_some() {
                json!({
                    "queries": subset_rows.len(),
                    "hit_at_10": mean_field(&subset_rows, "subset_hit"),
                    "recall_at_10": mean_field(&subset_rows, "subset_recall"),
                    "mrr_at_10": mean_field(&subset_rows, "subset_mrr"),
                })
            } else {
                Value::Null
            };
            let unmapped_total: u64 = per_query.iter().filter_map(|p| p["unmapped_in_top10"].as_u64()).sum();
            let mut unmapped_scope_total = json!({"docs": 0, "memory": 0, "ctx": 0, "other": 0});
            for p in &per_query {
                for s in ["docs", "memory", "ctx", "other"] {
                    let sum = unmapped_scope_total[s].as_u64().unwrap_or(0) + p["unmapped_by_scope"][s].as_u64().unwrap_or(0);
                    unmapped_scope_total[s] = json!(sum);
                }
            }
            let errored_queries = errors.len();
            let clean: Vec<&Value> = answered.iter().copied().filter(|p| p["error"] != true).collect();
            let doc = json!({
                "engine": name,
                "mode": mode,
                "coverage": coverage_json,
                "subset": subset_json,
                "corpus_records": recs.len(),
                "queries": queries.len(),
                "k": K,
                "warm_runs_per_query": o.runs,
                "ingest_seconds": ingest_s,
                "index_bytes": index_bytes,
                "setup_notes": engine.setup_notes(),
                "errors": errors,
                "aggregate": {
                    "answered_queries": answered.len(),
                    "hit_at_10": mean("hit"),
                    "recall_at_10": mean("recall"),
                    "mrr_at_10": mean("mrr"),
                    "no_answer_queries_with_results": no_answer_hits,
                    "unmapped_results_in_top10": unmapped_total,
                    "unmapped_results_by_scope": unmapped_scope_total,
                    "errored_queries": errored_queries,
                    "answered_without_errors": clean.len(),
                    "hit_at_10_without_errors": mean_field(&clean, "hit"),
                },
                "latency_ms": {
                    "samples": lat_us.len(),
                    "p50": metrics::percentile(&lat_us, 0.50) as f64 / 1000.0,
                    "p95": metrics::percentile(&lat_us, 0.95) as f64 / 1000.0,
                    "mean": mean_us / 1000.0,
                },
                "per_query": per_query,
            });
            let path = o.out.join(format!("{name}__{mode}.json"));
            fs::write(&path, serde_json::to_string_pretty(&doc)?)?;
            eprintln!("[{name}/{mode}] wrote {}", path.display());
        }
    }
    if smoke_failures > 0 {
        bail!("smoke: {smoke_failures} engine(s) failed");
    }
    Ok(())
}

/// Smoke check (`run db --smoke`): builds the engine, then runs the first query in
/// each mode and prints its top 3 mapped keys and whether its gold is in the top 10.
/// For remote engines it also checks coverage of that query's gold keys. For local
/// engines it runs a self-probe: one word of a task title that occurs in no other
/// record must find that task in the top 10, and the number of records it matches is
/// printed (0 means the engine does not index the word; more than 0 with a miss
/// means ranking). Writes no results; a failed search or a self-probe miss is an error.
fn smoke_engine(engine: &mut dyn Engine, recs: &[Record], queries: &[Query]) -> Result<()> {
    let name = engine.name();
    let q = queries.first().context("queries.jsonl is empty")?;
    if engine.is_remote() {
        let mut exist = Vec::new();
        for g in &q.gold {
            exist.push(format!("{g}={:?}", engine.key_exists(g)?));
        }
        if !exist.is_empty() {
            println!("[{name}] gold of {} exists: {}", q.id, exist.join(" "));
        }
    }
    for mode in engine.modes() {
        let top = engine.search(mode, &q.text, K)?;
        let gold_hit = match metrics::score(&q.gold, &top) {
            Some(s) => if s.hit { "yes" } else { "no" },
            None => "n/a (no gold)",
        };
        let top3: Vec<&str> = top.iter().take(3).map(String::as_str).collect();
        println!("[{name}/{mode}] {} {:?} top3 {:?} gold in top10: {gold_hit}", q.id, q.text, top3);
    }
    if engine.is_remote() {
        return Ok(());
    }
    let (key, probe) = self_probe(recs).context("no task title has a word that occurs in only one record")?;
    for mode in engine.modes() {
        let top = engine.search(mode, &probe, K)?;
        let matches = match engine.match_count(mode, &probe)? {
            Some(m) => m.to_string(),
            None => "n/a".into(),
        };
        match top.iter().position(|k| *k == key) {
            Some(p) => println!("[{name}/{mode}] self-probe {probe:?} -> {key} at rank {} (matches {matches})", p + 1),
            None => bail!(
                "self-probe {probe:?} did not find {key} in mode {mode} (matches {matches}; top3 {:?})",
                top.iter().take(3).collect::<Vec<_>>()
            ),
        }
    }
    Ok(())
}

/// A distinctive probe: the first word of a task title (at least four characters,
/// letters and digits only) that occurs in no other record, and that task's key.
/// Substring matching makes the check stricter, never looser.
fn self_probe(recs: &[Record]) -> Option<(String, String)> {
    for r in recs.iter().filter(|r| r.key.starts_with("task:")) {
        let title = r.text.lines().next().unwrap_or("");
        for w in title.split_whitespace() {
            if w.chars().count() < 4 || !w.chars().all(char::is_alphanumeric) {
                continue;
            }
            let mut found = 0;
            for other in recs {
                if other.text.contains(w) {
                    found += 1;
                    if found > 1 {
                        break;
                    }
                }
            }
            if found == 1 {
                return Some((r.key.clone(), w.to_string()));
            }
        }
    }
    None
}

/// Unmapped results in a top list, by the scope named in the key (`unmapped:<scope>:<key>`).
fn unmapped_by_scope(top: &[String]) -> Value {
    let mut c = json!({"docs": 0, "memory": 0, "ctx": 0, "other": 0});
    for scope in top.iter().filter_map(|k| k.strip_prefix("unmapped:")).map(|k| k.split(':').next().unwrap_or("")) {
        let slot = match scope {
            "docs" | "memory" | "ctx" => scope,
            _ => "other",
        };
        c[slot] = json!(c[slot].as_u64().unwrap_or(0) + 1);
    }
    c
}

/// Whether each gold key of the query set exists in a remote system. None when the
/// engine indexed the corpus itself (coverage does not apply) or there is no gold.
struct Coverage {
    exists: BTreeMap<String, Option<bool>>,
    errors: Vec<String>,
}

fn measure_coverage(engine: &mut dyn Engine, queries: &[Query]) -> Option<Coverage> {
    let mut keys: Vec<String> = queries.iter().flat_map(|q| q.gold.iter().cloned()).collect();
    keys.sort();
    keys.dedup();
    if keys.is_empty() {
        return None;
    }
    let mut exists = BTreeMap::new();
    let mut errors = Vec::new();
    for key in &keys {
        match engine.key_exists(key) {
            Ok(Some(b)) => {
                exists.insert(key.clone(), Some(b));
            }
            Ok(None) => return None,
            Err(e) => {
                // Unknown, not missing: an error must not lower coverage.
                errors.push(format!("{key}: {e:#}"));
                exists.insert(key.clone(), None);
            }
        }
    }
    Some(Coverage { exists, errors })
}

fn mean_field(rows: &[&Value], field: &str) -> f64 {
    if rows.is_empty() {
        return 0.0;
    }
    rows.iter()
        .map(|p| p[field].as_f64().or_else(|| p[field].as_bool().map(|b| b as u8 as f64)).unwrap_or(0.0))
        .sum::<f64>()
        / rows.len() as f64
}

fn summarize(args: &[String]) -> Result<()> {
    let o = parse_opts(args)?;
    let mut files: Vec<PathBuf> = fs::read_dir(&o.out)?
        .filter_map(|e| e.ok().map(|e| e.path()))
        .filter(|p| p.extension().map(|x| x == "json").unwrap_or(false))
        .collect();
    files.sort();
    let is_failed = |p: &PathBuf| p.file_name().map(|n| n.to_string_lossy().ends_with("__failed.json")).unwrap_or(false);
    let (failed_files, result_files): (Vec<PathBuf>, Vec<PathBuf>) = files.into_iter().partition(is_failed);
    let docs: Vec<Value> = result_files
        .iter()
        .map(|p| Ok(serde_json::from_str(&fs::read_to_string(p)?)?))
        .collect::<Result<_>>()?;
    let failures: Vec<Value> = failed_files
        .iter()
        .map(|p| Ok(serde_json::from_str(&fs::read_to_string(p)?)?))
        .collect::<Result<_>>()?;

    let mut md = String::from("# Results\n\n");
    md.push_str("| engine | mode | ingest s | index MB | answered | hit@10 | hit@10 without errored queries | recall@10 | MRR@10 | no-answer with hits | p50 ms | p95 ms | errors |\n");
    md.push_str("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n");
    for d in &docs {
        let a = &d["aggregate"];
        let l = &d["latency_ms"];
        md.push_str(&format!(
            "| {} | {} | {:.1} | {:.1} | {} | {:.3} | {:.3} ({} answered) | {:.3} | {:.3} | {} | {:.2} | {:.2} | {} |\n",
            d["engine"].as_str().unwrap_or(""),
            d["mode"].as_str().unwrap_or(""),
            d["ingest_seconds"].as_f64().unwrap_or(0.0),
            d["index_bytes"].as_f64().unwrap_or(0.0) / 1_048_576.0,
            a["answered_queries"],
            a["hit_at_10"].as_f64().unwrap_or(0.0),
            a["hit_at_10_without_errors"].as_f64().unwrap_or(0.0),
            a["answered_without_errors"],
            a["recall_at_10"].as_f64().unwrap_or(0.0),
            a["mrr_at_10"].as_f64().unwrap_or(0.0),
            a["no_answer_queries_with_results"],
            l["p50"].as_f64().unwrap_or(0.0),
            l["p95"].as_f64().unwrap_or(0.0),
            d["errors"].as_array().map(|e| e.len()).unwrap_or(0),
        ));
    }
    md.push_str("\n## Failures (engine did not build; the run continued)\n\n");
    if failures.is_empty() {
        md.push_str("- none\n");
    }
    for f in &failures {
        md.push_str(&format!(
            "- {} ({}): {}\n",
            f["engine"].as_str().unwrap_or(""),
            f["stage"].as_str().unwrap_or(""),
            f["error"].as_str().unwrap_or("").replace('\n', " ")
        ));
    }
    md.push_str("\n## Coverage and subset (remote systems)\n\n");
    md.push_str("Subset = answered queries with at least one gold key that exists in the system; its scores use only those gold keys, so a missing mirror is not read as bad ranking.\n\n");
    let cov_rows: Vec<&Value> = docs.iter().filter(|d| !d["coverage"].is_null()).collect();
    if cov_rows.is_empty() {
        md.push_str("- none (local engines only)\n");
    } else {
        md.push_str("| engine | mode | gold keys | exist | missing | unknown | all answered: hit@10 | subset queries | subset hit@10 | subset recall@10 | subset MRR@10 | unmapped in top10 |\n");
        md.push_str("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n");
        for d in cov_rows {
            let c = &d["coverage"];
            let s = &d["subset"];
            md.push_str(&format!(
                "| {} | {} | {} | {} | {} | {} | {:.3} | {} | {:.3} | {:.3} | {:.3} | {} ({} docs not in corpus, {} memory, {} ctx, {} other) |\n",
                d["engine"].as_str().unwrap_or(""),
                d["mode"].as_str().unwrap_or(""),
                c["gold_keys"],
                c["exist"],
                c["missing"],
                c["unknown"],
                d["aggregate"]["hit_at_10"].as_f64().unwrap_or(0.0),
                s["queries"],
                s["hit_at_10"].as_f64().unwrap_or(0.0),
                s["recall_at_10"].as_f64().unwrap_or(0.0),
                s["mrr_at_10"].as_f64().unwrap_or(0.0),
                d["aggregate"]["unmapped_results_in_top10"],
                d["aggregate"]["unmapped_results_by_scope"]["docs"],
                d["aggregate"]["unmapped_results_by_scope"]["memory"],
                d["aggregate"]["unmapped_results_by_scope"]["ctx"],
                d["aggregate"]["unmapped_results_by_scope"]["other"],
            ));
        }
        for d in docs.iter().filter(|d| !d["coverage"].is_null()) {
            let errs = d["coverage"]["errors"].as_array().cloned().unwrap_or_default();
            if !errs.is_empty() {
                md.push_str(&format!("- {} coverage errors (first {}):\n", d["engine"].as_str().unwrap_or(""), errs.len()));
                for e in &errs {
                    md.push_str(&format!("  - {}\n", e.as_str().unwrap_or("").replace('\n', " ")));
                }
            }
        }
    }
    md.push_str("\n## Query errors (first 5 per engine and mode; an errored query scores as a miss)\n\n");
    let mut any_err = false;
    for d in &docs {
        let errs = d["errors"].as_array().cloned().unwrap_or_default();
        if errs.is_empty() {
            continue;
        }
        any_err = true;
        md.push_str(&format!("- {} / {}: {} errors\n", d["engine"].as_str().unwrap_or(""), d["mode"].as_str().unwrap_or(""), errs.len()));
        for e in errs.iter().take(5) {
            md.push_str(&format!("  - {}\n", e.as_str().unwrap_or("").replace('\n', " ")));
        }
    }
    if !any_err {
        md.push_str("- none\n");
    }
    md.push_str("\n## Setup notes\n\n");
    for d in &docs {
        let notes: Vec<&str> = d["setup_notes"].as_array().map(|n| n.iter().filter_map(|v| v.as_str()).collect()).unwrap_or_default();
        md.push_str(&format!("- {} / {}: {}\n", d["engine"].as_str().unwrap_or(""), d["mode"].as_str().unwrap_or(""), notes.join("; ")));
    }
    md.push_str("\n## Per-query misses (answered queries with no gold in top 10)\n\n");
    for d in &docs {
        let label = format!("{} / {}", d["engine"].as_str().unwrap_or(""), d["mode"].as_str().unwrap_or(""));
        md.push_str(&format!("### {label}\n\n"));
        let mut any = false;
        for p in d["per_query"].as_array().unwrap_or(&Vec::new()) {
            if p["answered"] == true && p["hit"] == false {
                any = true;
                let empty = Vec::new();
                let top3: Vec<&str> = p["top10"].as_array().unwrap_or(&empty).iter().take(3).filter_map(|v| v.as_str()).collect();
                let gold: Vec<&str> = p["gold"].as_array().map(|g| g.iter().filter_map(|v| v.as_str()).collect()).unwrap_or_default();
                md.push_str(&format!("- {} `{}` ({}), gold {}, top3 {}\n", p["id"].as_str().unwrap_or(""), p["text"].as_str().unwrap_or(""), p["category"].as_str().unwrap_or(""), gold.join(" "), top3.join(" ")));
            }
        }
        if !any {
            md.push_str("- none\n");
        }
        md.push('\n');
    }
    let path = o.out.join("summary.md");
    fs::write(&path, &md)?;
    print!("{md}");
    eprintln!("wrote {}", path.display());
    Ok(())
}

fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str) {
        Some("run") => run(&args[1..]),
        Some("summarize") => summarize(&args[1..]),
        _ => bail!("usage: searchbench run local|db|all [flags] | searchbench summarize [--out DIR]"),
    }
}

//! searchbench: one harness, one adapter per engine, the same corpus and queries.
//!
//!   searchbench run local [--only a,b] [--runs N] [--out DIR] [--work DIR]
//!   searchbench run db    [--only a,b] [--runs N] [--out DIR] [--work DIR]
//!   searchbench run all   ...
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
}

fn parse_opts(args: &[String]) -> Result<Opts> {
    let mut o = Opts {
        corpus: "corpus".into(),
        queries: "queries.jsonl".into(),
        out: "out".into(),
        work: "work".into(),
        runs: 100,
        only: None,
    };
    let mut it = args.iter();
    while let Some(flag) = it.next() {
        let mut val = || it.next().cloned().ok_or_else(|| anyhow!("{flag} needs a value"));
        match flag.as_str() {
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
            let doc = json!({ "engine": name, "stage": "build", "error": msg });
            fs::write(o.out.join(format!("{name}__failed.json")), serde_json::to_string_pretty(&doc)?)?;
            continue;
        }
        let ingest_s = t.elapsed().as_secs_f64();
        let index_bytes = engine.index_bytes().unwrap_or(0);
        eprintln!("[{name}] built in {ingest_s:.1}s, index {index_bytes} bytes");

        for mode in engine.modes() {
            eprintln!("[{name}/{mode}] measuring {} queries", queries.len());
            let mut per_query = Vec::new();
            let mut errors = Vec::new();
            let mut lat_us: Vec<u64> = Vec::new();
            for q in &queries {
                let top = match engine.search(mode, &q.text, K) {
                    Ok(v) => v,
                    Err(e) => {
                        errors.push(format!("{}: {e:#}", q.id));
                        Vec::new()
                    }
                };
                let s = metrics::score(&q.gold, &top);
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
            let doc = json!({
                "engine": name,
                "mode": mode,
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
    Ok(())
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
    md.push_str("| engine | mode | ingest s | index MB | answered | hit@10 | recall@10 | MRR@10 | no-answer with hits | p50 ms | p95 ms | errors |\n");
    md.push_str("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n");
    for d in &docs {
        let a = &d["aggregate"];
        let l = &d["latency_ms"];
        md.push_str(&format!(
            "| {} | {} | {:.1} | {:.1} | {} | {:.3} | {:.3} | {:.3} | {} | {:.2} | {:.2} | {} |\n",
            d["engine"].as_str().unwrap_or(""),
            d["mode"].as_str().unwrap_or(""),
            d["ingest_seconds"].as_f64().unwrap_or(0.0),
            d["index_bytes"].as_f64().unwrap_or(0.0) / 1_048_576.0,
            a["answered_queries"],
            a["hit_at_10"].as_f64().unwrap_or(0.0),
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
    md.push_str("\n## Query errors (first 5 per engine and mode)\n\n");
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

//! hk production search (remote, read-only). Runs the hk CLI as a subprocess:
//! `handoffkeep ctx search --scope <S> --limit <k> <query>`, and parses the JSON array
//! it prints (one `SearchResult` per row). Read-only commands only.
//!
//! Modes:
//! - `all`: scope all, as the operator asked. In the store, scope all covers ctx,
//!   memory and docs; tasks are only returned by scope tasks.
//! - `tasks`: scope tasks, to show what the task gold can reach at all.
//!
//! Keys: a `tasks` row is `task:<key>`; a `docs` row is mapped to the corpus key
//! `doc:<id>` through the document key (the first line of each document record).
//! `ctx` and `memory` rows have no corpus key and stay as `unmapped:<scope>:<key>`,
//! so they keep their rank.
//!
//! Coverage (`key_exists`): `handoffkeep tasks show <id>` or `handoffkeep doc get --id
//! <id>`. Exit 0 means the record exists; a `not_found` error means it does not.
//!
//! Environment: HK_BIN (default `handoffkeep`). The CLI reads its own configuration;
//! this adapter never reads it.

use super::Engine;
use crate::corpus::Record;
use anyhow::{anyhow, bail, Context, Result};
use serde::Deserialize;
use std::collections::HashMap;
use std::io::Read;
use std::process::{Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

const CMD_TIMEOUT: Duration = Duration::from_secs(120);

#[derive(Deserialize)]
pub struct Row {
    pub scope: String,
    #[serde(default)]
    pub key: String,
}

pub struct HkPgEngine {
    bin: String,
    /// Document key (as stored in hk) to corpus key `doc:<id>`.
    docs: HashMap<String, String>,
}

/// Maps one search row to a corpus-style key (see the module comment).
pub fn map_row(scope: &str, key: &str, docs: &HashMap<String, String>) -> String {
    match scope {
        "tasks" if key.parse::<i64>().is_ok() => format!("task:{key}"),
        "docs" => docs.get(key).cloned().unwrap_or_else(|| format!("unmapped:docs:{key}")),
        _ => format!("unmapped:{scope}:{key}"),
    }
}

fn first_line(s: &str) -> String {
    let line = s.lines().find(|l| !l.trim().is_empty()).unwrap_or("").trim();
    line.chars().take(200).collect()
}

impl HkPgEngine {
    pub fn new() -> Self {
        Self { bin: String::new(), docs: HashMap::new() }
    }

    /// Runs the CLI with a timeout. Returns (exit success, stdout, stderr).
    fn run(&self, args: &[String]) -> Result<(bool, Vec<u8>, String)> {
        let mut child = Command::new(&self.bin)
            .args(args)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .with_context(|| format!("cannot start {}", self.bin))?;
        let mut so = child.stdout.take().context("no stdout pipe")?;
        let mut se = child.stderr.take().context("no stderr pipe")?;
        let t_out = thread::spawn(move || {
            let mut b = Vec::new();
            let _ = so.read_to_end(&mut b);
            b
        });
        let t_err = thread::spawn(move || {
            let mut b = Vec::new();
            let _ = se.read_to_end(&mut b);
            b
        });
        let deadline = Instant::now() + CMD_TIMEOUT;
        let status = loop {
            if let Some(s) = child.try_wait()? {
                break s;
            }
            if Instant::now() > deadline {
                let _ = child.kill();
                let _ = child.wait();
                bail!("hk CLI timed out after {}s", CMD_TIMEOUT.as_secs());
            }
            thread::sleep(Duration::from_millis(5));
        };
        let out = t_out.join().unwrap_or_default();
        let err = String::from_utf8_lossy(&t_err.join().unwrap_or_default()).into_owned();
        Ok((status.success(), out, err))
    }
}

impl Engine for HkPgEngine {
    fn name(&self) -> &'static str {
        "hk-pg"
    }

    fn modes(&self) -> Vec<&'static str> {
        vec!["all", "tasks"]
    }

    fn build(&mut self, recs: &[Record]) -> Result<()> {
        self.bin = std::env::var("HK_BIN").ok().filter(|s| !s.trim().is_empty()).unwrap_or_else(|| "handoffkeep".into());
        // Fail this engine alone if the CLI cannot start at all (any exit status is fine here).
        Command::new(&self.bin)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status()
            .with_context(|| format!("cannot start hk CLI ({})", self.bin))?;
        // Document records are "<key>\n<body>"; the first line is the document key.
        self.docs = recs
            .iter()
            .filter(|r| r.key.starts_with("doc:"))
            .filter_map(|r| r.text.lines().next().map(|k| (k.to_string(), r.key.clone())))
            .collect();
        Ok(())
    }

    fn index_bytes(&mut self) -> Result<u64> {
        Ok(0)
    }

    fn search(&mut self, mode: &str, q: &str, k: usize) -> Result<Vec<String>> {
        let scope = match mode {
            "all" | "tasks" => mode,
            other => bail!("unknown mode {other}"),
        };
        if q.trim().is_empty() {
            return Ok(Vec::new());
        }
        if q.starts_with('-') {
            bail!("query starts with '-' and would be read as a flag");
        }
        let args = vec![
            "ctx".to_string(),
            "search".to_string(),
            "--scope".to_string(),
            scope.to_string(),
            "--limit".to_string(),
            k.to_string(),
            q.to_string(),
        ];
        let (ok, out, err) = self.run(&args)?;
        if !ok {
            bail!("ctx search exited non-zero: {}", first_line(&err));
        }
        let rows: Vec<Row> = serde_json::from_slice(&out).context("ctx search output is not a JSON array of rows")?;
        Ok(rows.iter().take(k).map(|r| map_row(&r.scope, &r.key, &self.docs)).collect())
    }

    fn key_exists(&mut self, key: &str) -> Result<Option<bool>> {
        let (kind, id) = key.split_once(':').context("key has no kind")?;
        let n: i64 = id.parse().context("key id is not numeric")?;
        let args: Vec<String> = match kind {
            "task" => vec!["tasks".into(), "show".into(), n.to_string()],
            "doc" => vec!["doc".into(), "get".into(), "--id".into(), n.to_string()],
            _ => return Ok(None),
        };
        let (ok, _out, err) = self.run(&args)?;
        if ok {
            return Ok(Some(true));
        }
        if err.contains("not_found") {
            return Ok(Some(false));
        }
        Err(anyhow!("existence check failed: {}", first_line(&err)))
    }

    fn setup_notes(&self) -> Vec<String> {
        vec![
            "hk production search through the handoffkeep CLI (ctx search, read-only); the CLI's own configuration is used".into(),
            "scope all covers ctx, memory and docs; tasks come only from scope tasks (mode tasks)".into(),
            "coverage: tasks show / doc get; exit 0 = exists, not_found = missing".into(),
        ]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rows_map_to_corpus_keys() {
        let mut docs = HashMap::new();
        docs.insert("auto_brief_x".to_string(), "doc:61683".to_string());
        assert_eq!(map_row("tasks", "629", &docs), "task:629");
        assert_eq!(map_row("docs", "auto_brief_x", &docs), "doc:61683");
        assert_eq!(map_row("docs", "unknown_key", &docs), "unmapped:docs:unknown_key");
        assert_eq!(map_row("ctx", "", &docs), "unmapped:ctx:");
        assert_eq!(map_row("tasks", "x", &docs), "unmapped:tasks:x");
    }

    #[test]
    fn search_output_parses_as_rows() {
        let raw = br#"[{"scope":"docs","key":"auto_brief_x","session":"","kind":"brief","title":"auto_brief_x","snippet":"","created_at":"2026-11-09T00:00:00Z"},{"scope":"ctx","key":"","session":"s","kind":"checkpoint","title":"t","snippet":""}]"#;
        let rows: Vec<Row> = serde_json::from_slice(raw).unwrap();
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[0].scope, "docs");
        assert_eq!(rows[1].key, "");
    }
}

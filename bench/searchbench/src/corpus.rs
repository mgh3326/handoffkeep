//! Corpus and query loading. Records are the exported handoffkeep tasks (title
//! plus comments) and documents (key plus body); a record's key is "task:<id>"
//! or "doc:<id>" and is the same key used by the gold sets in queries.jsonl.

use anyhow::{Context, Result};
use serde::Deserialize;
use std::collections::BTreeMap;
use std::fs;
use std::path::Path;

#[derive(Clone, Debug)]
pub struct Record {
    pub key: String,
    pub text: String,
}

#[derive(Deserialize)]
struct TaskRow {
    id: i64,
    title: String,
}

#[derive(Deserialize)]
struct CommentRow {
    task_id: i64,
    body: String,
}

#[derive(Deserialize)]
struct DocRow {
    id: i64,
    key: String,
    #[serde(default)]
    body: String,
}

#[derive(Deserialize, Clone, Debug)]
pub struct Query {
    pub id: String,
    pub text: String,
    pub category: String,
    pub gold: Vec<String>,
    pub gold_basis: String,
}

fn lines<T: for<'de> Deserialize<'de>>(path: &Path) -> Result<Vec<T>> {
    let raw = fs::read_to_string(path).with_context(|| format!("read {}", path.display()))?;
    raw.lines()
        .filter(|l| !l.trim().is_empty())
        .enumerate()
        .map(|(i, l)| serde_json::from_str(l).with_context(|| format!("{}:{}", path.display(), i + 1)))
        .collect()
}

/// Loads every searchable record: one per task (title + its comments in id
/// order) and one per document (key + body).
pub fn load_records(dir: &Path) -> Result<Vec<Record>> {
    let tasks: Vec<TaskRow> = lines(&dir.join("tasks.jsonl"))?;
    let comments: Vec<CommentRow> = lines(&dir.join("comments.jsonl"))?;
    let docs: Vec<DocRow> = lines(&dir.join("documents.jsonl"))?;

    let mut by_task: BTreeMap<i64, Vec<String>> = BTreeMap::new();
    for c in comments {
        by_task.entry(c.task_id).or_default().push(c.body);
    }

    let mut out = Vec::with_capacity(tasks.len() + docs.len());
    for t in tasks {
        let mut text = t.title;
        for body in by_task.remove(&t.id).unwrap_or_default() {
            text.push('\n');
            text.push_str(&body);
        }
        out.push(Record { key: format!("task:{}", t.id), text });
    }
    for d in docs {
        out.push(Record { key: format!("doc:{}", d.id), text: format!("{}\n{}", d.key, d.body) });
    }
    Ok(out)
}

pub fn load_queries(path: &Path) -> Result<Vec<Query>> {
    lines(path)
}

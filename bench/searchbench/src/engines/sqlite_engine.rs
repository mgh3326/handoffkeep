//! SQLite FTS5 adapter (rusqlite, bundled SQLite). Two variants:
//! - unicode61: default FTS5 tokenizer, whole-token match, bm25 ranking.
//! - trigram:   FTS5 trigram tokenizer, substring match. FTS5 cannot MATCH a
//!              token shorter than 3 characters, so short tokens are handled by
//!              a LIKE scan when a query has no longer token, and are dropped
//!              from the MATCH expression otherwise (both documented in notes).

use super::Engine;
use crate::corpus::Record;
use anyhow::{Context, Result};
use rusqlite::{params, Connection};
use std::fs;
use std::path::{Path, PathBuf};

#[derive(Clone, Copy)]
pub enum Variant {
    Unicode61,
    Trigram,
}

pub struct SqliteEngine {
    variant: Variant,
    path: PathBuf,
    conn: Option<Connection>,
}

impl SqliteEngine {
    pub fn new(variant: Variant, work: &Path) -> Self {
        let file = match variant {
            Variant::Unicode61 => "fts5-unicode61.db",
            Variant::Trigram => "fts5-trigram.db",
        };
        Self { variant, path: work.join(file), conn: None }
    }
}

fn quote(tok: &str) -> String {
    format!("\"{}\"", tok.replace('"', "\"\""))
}

impl Engine for SqliteEngine {
    fn name(&self) -> &'static str {
        match self.variant {
            Variant::Unicode61 => "sqlite-fts5-unicode61",
            Variant::Trigram => "sqlite-fts5-trigram",
        }
    }

    fn modes(&self) -> Vec<&'static str> {
        vec!["or-bm25"]
    }

    fn build(&mut self, recs: &[Record]) -> Result<()> {
        let _ = fs::remove_file(&self.path);
        let mut conn = Connection::open(&self.path)?;
        conn.execute_batch("PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF;")?;
        let tok = match self.variant {
            Variant::Unicode61 => "unicode61",
            Variant::Trigram => "trigram",
        };
        conn.execute_batch(&format!("CREATE VIRTUAL TABLE docs USING fts5(rec_key UNINDEXED, body, tokenize='{tok}');"))?;
        {
            let tx = conn.transaction()?;
            {
                let mut st = tx.prepare("INSERT INTO docs(rec_key, body) VALUES (?1, ?2)")?;
                for r in recs {
                    st.execute(params![r.key, r.text])?;
                }
            }
            tx.commit()?;
        }
        conn.execute_batch("INSERT INTO docs(docs) VALUES('optimize'); PRAGMA wal_checkpoint;")?;
        self.conn = Some(conn);
        Ok(())
    }

    fn index_bytes(&mut self) -> Result<u64> {
        Ok(fs::metadata(&self.path)?.len())
    }

    fn search(&mut self, _mode: &str, q: &str, k: usize) -> Result<Vec<String>> {
        let conn = self.conn.as_ref().context("not built")?;
        let toks: Vec<&str> = q.split_whitespace().collect();
        if toks.is_empty() {
            return Ok(Vec::new());
        }
        let usable: Vec<&str> = match self.variant {
            Variant::Unicode61 => toks.clone(),
            Variant::Trigram => toks.iter().copied().filter(|t| t.chars().count() >= 3).collect(),
        };

        if usable.is_empty() {
            // Trigram fallback: every token is shorter than 3 characters.
            let clause = toks.iter().map(|_| "body LIKE ?").collect::<Vec<_>>().join(" OR ");
            let sql = format!("SELECT rec_key FROM docs WHERE {clause} LIMIT ?");
            let pats: Vec<String> = toks.iter().map(|t| format!("%{t}%")).collect();
            let mut st = conn.prepare(&sql)?;
            let mut args: Vec<&dyn rusqlite::ToSql> = pats.iter().map(|p| p as &dyn rusqlite::ToSql).collect();
            let lim = k as i64;
            args.push(&lim);
            let rows = st.query_map(args.as_slice(), |r| r.get::<_, String>(0))?;
            return Ok(rows.collect::<rusqlite::Result<Vec<_>>>()?);
        }

        let expr = usable.iter().map(|t| quote(t)).collect::<Vec<_>>().join(" OR ");
        let mut st = conn.prepare("SELECT rec_key FROM docs WHERE docs MATCH ?1 ORDER BY rank LIMIT ?2")?;
        let rows = st.query_map(params![expr, k as i64], |r| r.get::<_, String>(0))?;
        Ok(rows.collect::<rusqlite::Result<Vec<_>>>()?)
    }

    fn setup_notes(&self) -> Vec<String> {
        match self.variant {
            Variant::Unicode61 => vec!["FTS5 unicode61 default tokenizer; Hangul is not segmented, so particles/compounds only match as whole tokens".into()],
            Variant::Trigram => vec![
                "FTS5 trigram tokenizer (substring match, case-insensitive)".into(),
                "tokens shorter than 3 chars are dropped from MATCH; if every token is short, falls back to LIKE scan (unranked)".into(),
            ],
        }
    }
}

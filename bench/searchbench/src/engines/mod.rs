//! One adapter per engine. Every adapter builds its index from the same records
//! and answers the same queries with the top 10 record keys.

use crate::corpus::Record;
use anyhow::Result;

pub mod hk_pg_engine;
pub mod mysql_engine;
#[cfg(feature = "oracle")]
pub mod oracle_engine;
pub mod sqlite_engine;
pub mod tantivy_engine;
pub mod youtrack_engine;

pub trait Engine {
    fn name(&self) -> &'static str;
    /// Query modes measured against the one built index.
    fn modes(&self) -> Vec<&'static str>;
    fn build(&mut self, recs: &[Record]) -> Result<()>;
    fn index_bytes(&mut self) -> Result<u64>;
    fn search(&mut self, mode: &str, q: &str, k: usize) -> Result<Vec<String>>;
    fn setup_notes(&self) -> Vec<String>;
    /// Remote systems only: whether a corpus key (`task:<id>`, `doc:<id>`) exists in
    /// the system at all. Ok(None) means coverage does not apply (the engine indexed
    /// the corpus itself).
    fn key_exists(&mut self, _key: &str) -> Result<Option<bool>> {
        Ok(None)
    }
    /// Remote systems hold data the harness did not ingest, so the smoke check
    /// does not self-probe them.
    fn is_remote(&self) -> bool {
        false
    }
    /// How many records the query matches at any rank. Used by the smoke check to
    /// tell "not matched" (index or query fault) from "matched but ranked low".
    /// None when the engine does not count matches.
    fn match_count(&mut self, _mode: &str, _q: &str) -> Result<Option<u64>> {
        Ok(None)
    }
}

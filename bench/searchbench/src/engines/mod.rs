//! One adapter per engine. Every adapter builds its index from the same records
//! and answers the same queries with the top 10 record keys.

use crate::corpus::Record;
use anyhow::Result;

pub mod mysql_engine;
#[cfg(feature = "oracle")]
pub mod oracle_engine;
pub mod sqlite_engine;
pub mod tantivy_engine;

pub trait Engine {
    fn name(&self) -> &'static str;
    /// Query modes measured against the one built index.
    fn modes(&self) -> Vec<&'static str>;
    fn build(&mut self, recs: &[Record]) -> Result<()>;
    fn index_bytes(&mut self) -> Result<u64>;
    fn search(&mut self, mode: &str, q: &str, k: usize) -> Result<Vec<String>>;
    fn setup_notes(&self) -> Vec<String>;
}

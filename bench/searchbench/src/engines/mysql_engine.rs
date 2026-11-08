//! MySQL / HeatWave adapter: InnoDB FULLTEXT WITH PARSER ngram.
//!
//! Connection settings come only from the environment (see run-db.sh):
//! MYSQL_HOST, MYSQL_PORT, MYSQL_ADMIN_USER, MYSQL_ADMIN_PASSWORD, and optionally
//! MYSQL_SSL_CA (path to a CA bundle for HeatWave's certificate).
//! MYSQL_SSL: `required` (default, TLS through native-tls/OpenSSL) or `disabled`
//! (no TLS; for the in-VCN private address only).
//! TLS uses native-tls rather than rustls: HeatWave serves an X.509 v1
//! certificate, which webpki rejects (UnsupportedCertVersion) but OpenSSL accepts.
//! The adapter drops and recreates the database `searchbench` and nothing else.
//!
//! Modes:
//! - boolean: every whitespace token is required (`+tok`), BOOLEAN MODE.
//! - natural: the raw query, NATURAL LANGUAGE MODE (relevance ranking; the 50%
//!   row threshold of natural mode applies).

use super::Engine;
use crate::corpus::Record;
use anyhow::{anyhow, Context, Result};
use mysql::{prelude::*, Conn, OptsBuilder, SslOpts};
use std::path::PathBuf;

pub struct MysqlEngine {
    conn: Option<Conn>,
    notes: Vec<String>,
}

pub fn env(name: &str) -> Result<String> {
    std::env::var(name).map_err(|_| anyhow!("missing environment variable {name}"))
}

impl MysqlEngine {
    pub fn new() -> Self {
        Self { conn: None, notes: Vec::new() }
    }

    fn opts(db: Option<&str>) -> Result<OptsBuilder> {
        let port: u16 = env("MYSQL_PORT")?.parse().context("MYSQL_PORT")?;
        let ssl = match std::env::var("MYSQL_SSL").unwrap_or_else(|_| "required".into()).as_str() {
            "required" => {
                let mut ssl = SslOpts::default();
                if let Ok(ca) = std::env::var("MYSQL_SSL_CA") {
                    ssl = ssl.with_root_cert_path(Some(PathBuf::from(ca)));
                }
                Some(ssl)
            }
            "disabled" => None,
            other => return Err(anyhow!("MYSQL_SSL must be required or disabled, got {other:?}")),
        };
        let mut b = OptsBuilder::new()
            .ip_or_hostname(Some(env("MYSQL_HOST")?))
            .tcp_port(port)
            .user(Some(env("MYSQL_ADMIN_USER")?))
            .pass(Some(env("MYSQL_ADMIN_PASSWORD")?))
            .ssl_opts(ssl);
        if let Some(db) = db {
            b = b.db_name(Some(db));
        }
        Ok(b)
    }
}

impl Engine for MysqlEngine {
    fn name(&self) -> &'static str {
        "mysql-heatwave-ngram"
    }

    fn modes(&self) -> Vec<&'static str> {
        vec!["boolean", "natural"]
    }

    fn build(&mut self, recs: &[Record]) -> Result<()> {
        let mut admin = Conn::new(Self::opts(None)?)?;
        admin.query_drop("DROP DATABASE IF EXISTS searchbench")?;
        admin.query_drop("CREATE DATABASE searchbench CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")?;
        drop(admin);

        let mut conn = Conn::new(Self::opts(Some("searchbench"))?)?;
        let version: Option<(String, String, String)> = conn.query_first(
            "SELECT CAST(@@version AS CHAR), CAST(@@version_comment AS CHAR), CAST(@@ngram_token_size AS CHAR)",
        )?;
        if let Some((v, c, n)) = version {
            self.notes.push(format!("server version {v} ({c}); ngram_token_size={n}"));
        }
        conn.query_drop(
            "CREATE TABLE sb_docs (id INT UNSIGNED NOT NULL PRIMARY KEY, rec_key VARCHAR(40) NOT NULL, body LONGTEXT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
        )?;
        conn.exec_batch(
            "INSERT INTO sb_docs (id, rec_key, body) VALUES (?, ?, ?)",
            recs.iter().enumerate().map(|(i, r)| ((i + 1) as u32, r.key.as_str(), r.text.as_str())),
        )?;
        // The FULLTEXT index is built after the bulk load; its time is part of ingest.
        conn.query_drop("ALTER TABLE sb_docs ADD FULLTEXT KEY ft_body (body) WITH PARSER ngram")?;
        self.conn = Some(conn);
        Ok(())
    }

    fn index_bytes(&mut self) -> Result<u64> {
        let conn = self.conn.as_mut().context("not built")?;
        let bytes: Option<u64> = conn.query_first(
            "SELECT CAST(SUM(data_length + index_length) AS UNSIGNED) FROM information_schema.tables WHERE table_schema = 'searchbench' AND table_name = 'sb_docs'",
        )?;
        Ok(bytes.unwrap_or(0))
    }

    fn search(&mut self, mode: &str, q: &str, k: usize) -> Result<Vec<String>> {
        let conn = self.conn.as_mut().context("not built")?;
        let (sql, arg) = match mode {
            "boolean" => {
                let bq = q.split_whitespace().map(|t| format!("+{t}")).collect::<Vec<_>>().join(" ");
                (
                    format!("SELECT rec_key FROM sb_docs WHERE MATCH(body) AGAINST (? IN BOOLEAN MODE) ORDER BY MATCH(body) AGAINST (? IN BOOLEAN MODE) DESC LIMIT {k}"),
                    bq,
                )
            }
            "natural" => (
                format!("SELECT rec_key FROM sb_docs WHERE MATCH(body) AGAINST (? IN NATURAL LANGUAGE MODE) ORDER BY MATCH(body) AGAINST (? IN NATURAL LANGUAGE MODE) DESC LIMIT {k}"),
                q.to_string(),
            ),
            other => return Err(anyhow!("unknown mode {other}")),
        };
        if arg.trim().is_empty() {
            return Ok(Vec::new());
        }
        let rows: Vec<String> = conn.exec(sql, (arg.clone(), arg))?;
        Ok(rows)
    }

    fn setup_notes(&self) -> Vec<String> {
        let mut n = self.notes.clone();
        n.push("FULLTEXT ... WITH PARSER ngram built after bulk insert (ingest time includes it)".into());
        n
    }
}

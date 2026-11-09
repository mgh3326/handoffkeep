//! Oracle Autonomous Database adapter: Oracle Text CONTEXT index, one table per
//! lexer (KOREAN_MORPH_LEXER, BASIC_LEXER, WORLD_LEXER). Access path: rust-oracle
//! (ODPI-C) over Oracle Instant Client, loaded at run time; the connection uses
//! the ADB wallet via TNS_ADMIN.
//!
//! Environment (see run-db.sh): RB_DB_HOST, RB_DB_PORT, RB_DB_SERVICE, RB_DB_USER,
//! RB_DB_PASSWORD, RB_WALLET_DIR. Tables are SB_KM, SB_BASIC, SB_WORLD (and their
//! CONTEXT indexes) in the RB_DB_USER schema; the adapter drops and recreates them.
//!
//! Query: each whitespace token becomes an {escaped} term, joined with OR,
//! ranked by SCORE(1).

use super::Engine;
use crate::corpus::Record;
use anyhow::{anyhow, bail, Context, Result};
use oracle::{sql_type::Clob, Connection};
use std::io::Write;

#[derive(Clone, Copy)]
pub enum Lexer {
    KoreanMorph,
    Basic,
    World,
}

impl Lexer {
    fn table(self) -> &'static str {
        match self {
            Lexer::KoreanMorph => "SB_KM",
            Lexer::Basic => "SB_BASIC",
            Lexer::World => "SB_WORLD",
        }
    }
    /// The lexer preference named in the index PARAMETERS. ADB ships no CTXSYS
    /// preference for KOREAN_MORPH_LEXER (DRG-10700), so the bench schema creates
    /// one with CTX_DDL.CREATE_PREFERENCE and drops it after the index exists.
    fn lexer_ref(self) -> &'static str {
        match self {
            Lexer::KoreanMorph => KM_PREFERENCE,
            Lexer::Basic => "CTXSYS.BASIC_LEXER",
            Lexer::World => "CTXSYS.WORLD_LEXER",
        }
    }
}

const KM_PREFERENCE: &str = "SB_KM_LEX";

pub struct OracleEngine {
    lexer: Lexer,
    conn: Option<Connection>,
    notes: Vec<String>,
}

fn env(name: &str) -> Result<String> {
    std::env::var(name).map_err(|_| anyhow!("missing environment variable {name}"))
}

/// Replaces connect values in an error message so they are never printed.
fn redact(msg: &str, values: &[&str]) -> String {
    values
        .iter()
        .filter(|v| !v.is_empty())
        .fold(msg.to_string(), |m, v| m.replace(v, "<redacted>"))
}

impl OracleEngine {
    pub fn new(lexer: Lexer) -> Self {
        Self { lexer, conn: None, notes: Vec::new() }
    }

    /// Connects once per engine. RB_DB_SERVICE is a service name, not a tnsnames.ora
    /// alias (the wallet's aliases differ), so the connect string is a full TCPS
    /// descriptor built from RB_DB_HOST, RB_DB_PORT and RB_DB_SERVICE. TLS trust comes
    /// from the wallet in TNS_ADMIN (set by run-db.sh); the server DN is matched.
    /// Errors are returned with the connect values redacted.
    fn connect() -> Result<Connection> {
        let user = env("RB_DB_USER")?;
        let pass = env("RB_DB_PASSWORD")?;
        let host = env("RB_DB_HOST")?;
        let port = env("RB_DB_PORT")?;
        let service = env("RB_DB_SERVICE")?;
        let descriptor = format!(
            "(DESCRIPTION=(ADDRESS=(PROTOCOL=tcps)(HOST={host})(PORT={port}))(CONNECT_DATA=(SERVICE_NAME={service}))(SECURITY=(SSL_SERVER_DN_MATCH=yes)))"
        );
        Connection::connect(user, pass, descriptor).map_err(|e| {
            let msg = redact(&format!("{e}"), &[&host, &port, &service]);
            anyhow!("oracle connect: {msg}")
        })
    }

    fn escape(tok: &str) -> String {
        // Braces quote any Oracle Text operator characters inside the term.
        format!("{{{}}}", tok.replace('}', "}}"))
    }
}

impl Engine for OracleEngine {
    fn name(&self) -> &'static str {
        match self.lexer {
            Lexer::KoreanMorph => "oracle-text-korean-morph",
            Lexer::Basic => "oracle-text-basic",
            Lexer::World => "oracle-text-world",
        }
    }

    fn modes(&self) -> Vec<&'static str> {
        vec!["or-score"]
    }

    fn build(&mut self, recs: &[Record]) -> Result<()> {
        let conn = Self::connect()?;
        let t = self.lexer.table();
        conn.execute(
            &format!("BEGIN EXECUTE IMMEDIATE 'DROP TABLE {t} PURGE'; EXCEPTION WHEN OTHERS THEN NULL; END;"),
            &[],
        )?;
        conn.execute(&format!("CREATE TABLE {t} (ID NUMBER PRIMARY KEY, REC_KEY VARCHAR2(40) NOT NULL, BODY CLOB NOT NULL)"), &[])?;
        for (i, r) in recs.iter().enumerate() {
            let mut clob = Clob::new(&conn)?;
            clob.write_all(r.text.as_bytes())?;
            conn.execute(&format!("INSERT INTO {t} (ID, REC_KEY, BODY) VALUES (:1, :2, :3)"), &[&(i as i64 + 1), &r.key, &clob])?;
        }
        conn.commit()?;
        // Read-back: a spread sample of stored bodies must equal the records exactly,
        // so a write-path fault shows up here, not as ranking.
        let step = (recs.len() / 25).max(1);
        for i in (0..recs.len()).step_by(step) {
            let (body,): (String,) = conn.query_row_as(&format!("SELECT BODY FROM {t} WHERE ID = :1"), &[&(i as i64 + 1)])?;
            if body != recs[i].text {
                bail!(
                    "stored body of {} differs from its record ({} vs {} characters)",
                    recs[i].key,
                    body.chars().count(),
                    recs[i].text.chars().count()
                );
            }
        }
        let mut lexer_setup = Vec::new();
        if let Lexer::KoreanMorph = self.lexer {
            // Leftover from an earlier run: drop it so CREATE_PREFERENCE cannot fail on it.
            conn.execute(
                &format!("BEGIN CTX_DDL.DROP_PREFERENCE('{KM_PREFERENCE}'); EXCEPTION WHEN OTHERS THEN NULL; END;"),
                &[],
            )?;
            conn.execute(
                &format!("BEGIN CTX_DDL.CREATE_PREFERENCE('{KM_PREFERENCE}', 'KOREAN_MORPH_LEXER'); END;"),
                &[],
            )?;
            lexer_setup.push(format!("lexer preference {KM_PREFERENCE} (KOREAN_MORPH_LEXER) created in the bench schema, dropped after the index"));
        }
        // The CONTEXT index is created after the bulk load; its time is part of ingest.
        let index_result = conn
            .execute(
                &format!(
                    "CREATE INDEX {t}_IDX ON {t} (BODY) INDEXTYPE IS CTXSYS.CONTEXT PARAMETERS ('LEXER {}')",
                    self.lexer.lexer_ref()
                ),
                &[],
            )
            .map(|_| ());
        if let Lexer::KoreanMorph = self.lexer {
            // The index keeps its lexer settings; the bench preference is removed either way.
            let drop_result = conn
                .execute(&format!("BEGIN CTX_DDL.DROP_PREFERENCE('{KM_PREFERENCE}'); END;"), &[])
                .map(|_| ());
            if index_result.is_ok() {
                drop_result?;
            }
        }
        index_result?;
        let (ver,): (String,) = conn.query_row_as("SELECT BANNER FROM V$VERSION WHERE ROWNUM = 1", &[])?;
        self.notes.push(format!("server: {ver}"));
        self.notes.push(format!("CONTEXT index PARAMETERS ('LEXER {}')", self.lexer.lexer_ref()));
        self.notes.extend(lexer_setup);
        self.notes.push(format!("read-back: {} sampled bodies equal their records", recs.len().div_ceil(step)));
        self.conn = Some(conn);
        Ok(())
    }

    fn index_bytes(&mut self) -> Result<u64> {
        let conn = self.conn.as_ref().context("not built")?;
        let t = self.lexer.table();
        let (bytes,): (i64,) = conn.query_row_as(
            &format!("SELECT NVL(SUM(bytes), 0) FROM user_segments WHERE segment_name = '{t}' OR segment_name LIKE 'DR${t}_IDX$%'"),
            &[],
        )?;
        Ok(bytes as u64)
    }

    fn search(&mut self, _mode: &str, q: &str, k: usize) -> Result<Vec<String>> {
        let conn = self.conn.as_ref().context("not built")?;
        let terms: Vec<String> = q.split_whitespace().map(Self::escape).collect();
        if terms.is_empty() {
            return Ok(Vec::new());
        }
        let text_query = terms.join(" OR ");
        let sql = format!(
            "SELECT REC_KEY FROM {} WHERE CONTAINS(BODY, :1, 1) > 0 ORDER BY SCORE(1) DESC FETCH FIRST {k} ROWS ONLY",
            self.lexer.table()
        );
        let rows = conn.query_as::<(String,)>(&sql, &[&text_query])?;
        let mut out = Vec::new();
        for row in rows {
            out.push(row?.0);
        }
        Ok(out)
    }

    fn setup_notes(&self) -> Vec<String> {
        let mut n = self.notes.clone();
        n.push("rust-oracle (ODPI-C) with Oracle Instant Client loaded at run time; ADB wallet via TNS_ADMIN".into());
        n
    }
}

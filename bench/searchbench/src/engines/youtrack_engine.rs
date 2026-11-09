//! YouTrack adapter (remote, read-only, GET only). The operator's YouTrack screen as a
//! search system: `/api/issues` and `/api/articles` text search, ranked by YouTrack.
//!
//! Keys. An issue maps to `task:<hk ID>` through the hk ID custom field (default
//! `hk ID`, the field internal/youtrack writes). An article maps to `doc:<id>` by the
//! first of these that applies:
//!   1. its hk ID field (`doc:<hk ID>`);
//!   2. its custom field named by YOUTRACK_DOC_KEY_FIELD (optional), when that value
//!      is a corpus document key;
//!   3. the first corpus document key named in its summary, then in its content
//!      (the body of the article, where a mirror would name its hk document).
//! An item with none of these keeps its rank as `unmapped:<idReadable>`, so a missing
//! mapping shows up as unmapped results rather than as a ranking miss.
//!
//! Mode `native`: issues in YouTrack's order, then articles, up to k.
//! Query: each whitespace token is a braced term (`{deploy-ok} {검증}`), so YouTrack's
//! query grammar does not read `-` or `:` as operators. YouTrack ANDs the terms; that
//! is the system's own behaviour and is measured as it is.
//!
//! Coverage (`key_exists`): a task exists when an issue's hk ID field has its number.
//! A doc exists when an article names its document key (or has its hk ID).
//!
//! Environment: YOUTRACK_URL (https), YOUTRACK_API_KEY (bearer token; never printed or
//! logged), optional YOUTRACK_HK_FIELD (default `hk ID`), optional YOUTRACK_DOC_KEY_FIELD.

use super::Engine;
use crate::corpus::Record;
use anyhow::{anyhow, bail, Context, Result};
use serde::Deserialize;
use serde_json::Value;
use std::collections::HashMap;
use std::time::Duration;

const ISSUE_FIELDS: &str = "idReadable,customFields(name,$type,value)";
const ARTICLE_FIELDS: &str = "idReadable,summary,content,customFields(name,$type,value)";
const TIMEOUT: Duration = Duration::from_secs(30);

#[derive(Deserialize)]
pub struct Item {
    #[serde(rename = "idReadable", default)]
    pub id_readable: String,
    #[serde(default)]
    pub summary: Option<String>,
    #[serde(default)]
    pub content: Option<String>,
    #[serde(rename = "customFields", default)]
    pub custom_fields: Vec<Field>,
}

#[derive(Deserialize)]
pub struct Field {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub value: Value,
}

pub struct YoutrackEngine {
    base: String,
    token: String,
    hk_field: String,
    doc_key_field: Option<String>,
    /// Corpus document key (first line of the record) to `doc:<id>`.
    doc_ids: HashMap<String, String>,
    /// `doc:<id>` to the corpus document key.
    doc_keys: HashMap<String, String>,
    agent: Option<ureq::Agent>,
}

impl YoutrackEngine {
    pub fn new() -> Self {
        Self {
            base: String::new(),
            token: String::new(),
            hk_field: String::new(),
            doc_key_field: None,
            doc_ids: HashMap::new(),
            doc_keys: HashMap::new(),
            agent: None,
        }
    }

    fn get_items(&self, path: &str, fields: &str, query: &str, top: usize) -> Result<Vec<Item>> {
        let agent = self.agent.as_ref().context("not built")?;
        let url = format!("{}{}", self.base, path);
        let resp = agent
            .get(&url)
            .header("Authorization", &format!("Bearer {}", self.token))
            .header("Accept", "application/json")
            .query("query", query)
            .query("fields", fields)
            .query("$top", &top.to_string())
            .call()
            .map_err(|e| anyhow!("youtrack GET {path}: {e}"))?;
        let mut resp = resp;
        let items: Vec<Item> = resp
            .body_mut()
            .read_json()
            .map_err(|e| anyhow!("youtrack GET {path}: unexpected response: {e}"))?;
        Ok(items)
    }

    /// The key of an item: `<prefix>:<id>`, or `unmapped:<idReadable>`.
    fn key_for(&self, item: &Item, prefix: &str) -> String {
        if let Some(n) = hk_id(&item.custom_fields, &self.hk_field) {
            return format!("{prefix}:{n}");
        }
        if prefix == "doc" {
            if let Some(id) = self.doc_id_for(item) {
                return id;
            }
        }
        format!("unmapped:{}", item.id_readable)
    }

    /// The `doc:<id>` of an article without an hk ID (see the module comment).
    fn doc_id_for(&self, item: &Item) -> Option<String> {
        if let Some(field) = &self.doc_key_field {
            let value = item
                .custom_fields
                .iter()
                .find(|f| f.name.trim().eq_ignore_ascii_case(field.trim()))
                .and_then(|f| f.value.as_str())
                .map(str::trim);
            if let Some(id) = value.and_then(|v| self.doc_ids.get(v)) {
                return Some(id.clone());
            }
        }
        for text in [item.summary.as_deref(), item.content.as_deref()].into_iter().flatten() {
            if let Some(id) = first_doc_key(text, &self.doc_ids).and_then(|k| self.doc_ids.get(k)) {
                return Some(id.clone());
            }
        }
        None
    }
}

/// The first corpus document key that appears in `text`. A key is a run of letters,
/// digits and `/._-`; trailing `._-/` is not part of it, so `see decision/x/y.` finds
/// `decision/x/y`.
pub fn first_doc_key<'a>(text: &'a str, keys: &HashMap<String, String>) -> Option<&'a str> {
    text.split(|c: char| !(c.is_alphanumeric() || matches!(c, '/' | '.' | '_' | '-')))
        .map(|t| t.trim_end_matches(|c: char| matches!(c, '.' | '/' | '_' | '-')))
        .find(|t| keys.contains_key(*t))
}

/// The HTTP agent. TLS is pinned to native-tls: ureq's default provider is rustls,
/// which is not compiled in here, and an https request then panics inside ureq.
fn new_agent() -> ureq::Agent {
    let tls = ureq::tls::TlsConfig::builder().provider(ureq::tls::TlsProvider::NativeTls).build();
    let config = ureq::Agent::config_builder().timeout_global(Some(TIMEOUT)).tls_config(tls).build();
    ureq::Agent::new_with_config(config)
}

/// Braced terms: YouTrack reads braces as an exact term, so operator characters in
/// the text are literal. A `}` inside a term is escaped as `\}`.
pub fn yt_text_query(q: &str) -> String {
    q.split_whitespace()
        .map(|t| format!("{{{}}}", t.replace('}', "\\}")))
        .collect::<Vec<_>>()
        .join(" ")
}

/// The hk ID of an item. YouTrack returns an integer field as a number, a text field
/// as a string, and an enum or value object as {text} or {name}.
pub fn hk_id(fields: &[Field], name: &str) -> Option<i64> {
    let f = fields.iter().find(|f| f.name.trim().eq_ignore_ascii_case(name.trim()))?;
    match &f.value {
        Value::Number(n) => n.as_i64(),
        Value::String(s) => s.trim().parse().ok(),
        Value::Object(o) => o
            .get("text")
            .or_else(|| o.get("name"))
            .and_then(Value::as_str)
            .and_then(|s| s.trim().parse().ok()),
        _ => None,
    }
}

impl Engine for YoutrackEngine {
    fn name(&self) -> &'static str {
        "youtrack"
    }

    fn modes(&self) -> Vec<&'static str> {
        vec!["native"]
    }

    fn build(&mut self, recs: &[Record]) -> Result<()> {
        // Remote system: nothing is ingested. Settings are validated here so a missing
        // setting fails this engine alone (recorded in summary.md).
        let base = std::env::var("YOUTRACK_URL").context("missing environment variable YOUTRACK_URL")?;
        let token = std::env::var("YOUTRACK_API_KEY").context("missing environment variable YOUTRACK_API_KEY")?;
        let base = base.trim().trim_end_matches('/').to_string();
        // https only; plain http is allowed for loopback test servers, as in internal/youtrack.
        let loopback = base.starts_with("http://127.0.0.1:") || base.starts_with("http://localhost:");
        if !base.starts_with("https://") && !loopback {
            bail!("YOUTRACK_URL must be an https:// URL");
        }
        if token.trim().is_empty() {
            bail!("YOUTRACK_API_KEY is empty");
        }
        self.base = base;
        self.token = token.trim().to_string();
        self.hk_field = std::env::var("YOUTRACK_HK_FIELD")
            .ok()
            .filter(|s| !s.trim().is_empty())
            .unwrap_or_else(|| "hk ID".to_string());
        self.doc_key_field = std::env::var("YOUTRACK_DOC_KEY_FIELD").ok().filter(|s| !s.trim().is_empty());
        // The corpus document keys (first line of each document record), for article mapping.
        for r in recs.iter().filter(|r| r.key.starts_with("doc:")) {
            if let Some(k) = r.text.lines().next().map(str::trim).filter(|k| !k.is_empty()) {
                self.doc_ids.insert(k.to_string(), r.key.clone());
                self.doc_keys.insert(r.key.clone(), k.to_string());
            }
        }
        self.agent = Some(new_agent());
        Ok(())
    }

    fn index_bytes(&mut self) -> Result<u64> {
        Ok(0)
    }

    fn search(&mut self, _mode: &str, q: &str, k: usize) -> Result<Vec<String>> {
        if q.split_whitespace().next().is_none() {
            return Ok(Vec::new());
        }
        let text = yt_text_query(q);
        let issues = self.get_items("/api/issues", ISSUE_FIELDS, &text, k)?;
        let mut out: Vec<String> = issues.iter().take(k).map(|i| self.key_for(i, "task")).collect();
        if out.len() < k {
            let articles = self.get_items("/api/articles", ARTICLE_FIELDS, &text, k - out.len())?;
            out.extend(articles.iter().take(k - out.len()).map(|a| self.key_for(a, "doc")));
        }
        Ok(out)
    }

    fn key_exists(&mut self, key: &str) -> Result<Option<bool>> {
        let (kind, id) = key.split_once(':').context("key has no kind")?;
        let n: i64 = id.parse().context("key id is not numeric")?;
        match kind {
            "task" => {
                let query = format!("{{{}}}: {n}", self.hk_field);
                let items = self.get_items("/api/issues", ISSUE_FIELDS, &query, 2)?;
                Ok(Some(items.iter().any(|i| hk_id(&i.custom_fields, &self.hk_field) == Some(n))))
            }
            "doc" => {
                // An article names its document key in its text; a bare hk ID field
                // value is checked too. A key missing from the corpus cannot be searched:
                // that is an error (unknown), not a missing record.
                let doc_key = self
                    .doc_keys
                    .get(key)
                    .with_context(|| format!("{key} is not in the corpus, so its document key cannot be searched"))?;
                let items = self.get_items("/api/articles", ARTICLE_FIELDS, &yt_text_query(doc_key), 10)?;
                if items.iter().any(|i| self.key_for(i, "doc") == key) {
                    return Ok(Some(true));
                }
                let query = format!("{{{}}}: {n}", self.hk_field);
                let items = self.get_items("/api/articles", ARTICLE_FIELDS, &query, 2)?;
                Ok(Some(items.iter().any(|i| hk_id(&i.custom_fields, &self.hk_field) == Some(n))))
            }
            _ => Ok(None),
        }
    }

    fn is_remote(&self) -> bool {
        true
    }

    fn setup_notes(&self) -> Vec<String> {
        let mut notes = vec![
            "YouTrack REST, GET only: /api/issues and /api/articles text search, native ranking".into(),
            format!("issues map to task:<id> through the '{}' field", self.hk_field),
            "articles map to doc:<id> through the hk ID field, else the optional doc key field, else the first corpus doc key in the summary or content".into(),
            "query terms are braced (literal); YouTrack ANDs them; unmapped items keep their rank".into(),
        ];
        if let Some(f) = &self.doc_key_field {
            notes.push(format!("doc key field '{f}' (YOUTRACK_DOC_KEY_FIELD)"));
        }
        notes
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn field(name: &str, value: Value) -> Field {
        Field { name: name.into(), value }
    }

    #[test]
    fn braced_terms_keep_operator_characters_literal() {
        assert_eq!(yt_text_query("deploy-ok 검증"), "{deploy-ok} {검증}");
        assert_eq!(yt_text_query("a}b"), r"{a\}b}");
        assert_eq!(yt_text_query("  "), "");
    }

    /// An https request must fail as an error (here: the listener closes the
    /// connection), never panic. Regression for the rustls-default panic.
    #[test]
    fn https_request_fails_as_error_not_panic() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let server = std::thread::spawn(move || {
            if let Ok((stream, _)) = listener.accept() {
                drop(stream);
            }
        });
        let engine = YoutrackEngine {
            base: format!("https://127.0.0.1:{port}"),
            token: "test-token".into(),
            hk_field: "hk ID".into(),
            agent: Some(new_agent()),
            ..YoutrackEngine::new()
        };
        let r = engine.get_items("/api/issues", ISSUE_FIELDS, "{x}", 1);
        assert!(r.is_err());
        server.join().unwrap();
    }

    #[test]
    fn hk_id_reads_number_string_and_object_values() {
        let fs = vec![
            field("hk ID", serde_json::json!(3536)),
            field("Lane", serde_json::json!({"name": "x"})),
        ];
        assert_eq!(hk_id(&fs, "hk ID"), Some(3536));
        assert_eq!(hk_id(&fs, "HK ID"), Some(3536));
        let s = vec![field("hk ID", serde_json::json!(" 42 "))];
        assert_eq!(hk_id(&s, "hk ID"), Some(42));
        let o = vec![field("hk ID", serde_json::json!({"text": "7"}))];
        assert_eq!(hk_id(&o, "hk ID"), Some(7));
        assert_eq!(hk_id(&fs, "Missing"), None);
        let bad = vec![field("hk ID", serde_json::json!("abc"))];
        assert_eq!(hk_id(&bad, "hk ID"), None);
    }
    fn doc_ids() -> HashMap<String, String> {
        let mut m = HashMap::new();
        m.insert("decision/2026-11-01/operator-credential-oci".to_string(), "doc:5".to_string());
        m.insert("consult/2026-11-01/routing-v2-astra".to_string(), "doc:9".to_string());
        m
    }

    fn article(summary: Option<&str>, content: Option<&str>, fields: Vec<Field>) -> Item {
        Item {
            id_readable: "HK-A-17".into(),
            summary: summary.map(str::to_string),
            content: content.map(str::to_string),
            custom_fields: fields,
        }
    }

    #[test]
    fn first_doc_key_strips_trailing_punctuation() {
        let ids = doc_ids();
        assert_eq!(
            first_doc_key("See decision/2026-11-01/operator-credential-oci.", &ids),
            Some("decision/2026-11-01/operator-credential-oci")
        );
        assert_eq!(first_doc_key("no key here", &ids), None);
        assert_eq!(first_doc_key("decision/2026-11-01/unknown", &ids), None);
    }

    #[test]
    fn article_maps_by_hk_id_then_summary_then_content() {
        let engine = YoutrackEngine { doc_ids: doc_ids(), hk_field: "hk ID".into(), ..YoutrackEngine::new() };
        let by_id = article(None, None, vec![field("hk ID", serde_json::json!(42))]);
        assert_eq!(engine.key_for(&by_id, "doc"), "doc:42");
        let by_summary = article(
            Some("consult/2026-11-01/routing-v2-astra"),
            Some("decision/2026-11-01/operator-credential-oci"),
            vec![],
        );
        assert_eq!(engine.key_for(&by_summary, "doc"), "doc:9");
        let by_content = article(Some("Notes"), Some("body names decision/2026-11-01/operator-credential-oci"), vec![]);
        assert_eq!(engine.key_for(&by_content, "doc"), "doc:5");
        let none = article(Some("Notes"), Some("nothing"), vec![]);
        assert_eq!(engine.key_for(&none, "doc"), "unmapped:HK-A-17");
    }

    #[test]
    fn article_maps_by_doc_key_field_when_configured() {
        let engine = YoutrackEngine {
            doc_ids: doc_ids(),
            hk_field: "hk ID".into(),
            doc_key_field: Some("hk 문서 키".into()),
            ..YoutrackEngine::new()
        };
        let a = article(Some("Notes"), None, vec![field("hk 문서 키", serde_json::json!("consult/2026-11-01/routing-v2-astra"))]);
        assert_eq!(engine.key_for(&a, "doc"), "doc:9");
    }
}

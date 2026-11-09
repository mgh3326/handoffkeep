//! YouTrack adapter (remote, read-only, GET only). The operator's YouTrack screen as a
//! search system: `/api/issues` and `/api/articles` text search, ranked by YouTrack.
//!
//! Keys: an issue maps to `task:<hk ID>` and an article to `doc:<hk ID>`, through the
//! hk ID custom field (default `hk ID`, the field internal/youtrack writes). An item
//! without that value keeps its rank as `unmapped:<idReadable>`, so a missing mirror
//! shows up as unmapped results rather than as a ranking miss.
//!
//! Mode `native`: issues in YouTrack's order, then articles, up to k.
//! Query: each whitespace token is a braced term (`{deploy-ok} {검증}`), so YouTrack's
//! query grammar does not read `-` or `:` as operators. YouTrack ANDs the terms; that
//! is the system's own behaviour and is measured as it is.
//!
//! Coverage (`key_exists`): a task or doc exists in YouTrack when a query on its hk ID
//! field returns an item with that value.
//!
//! Environment: YOUTRACK_URL (https), YOUTRACK_API_KEY (bearer token; never printed or
//! logged), optional YOUTRACK_HK_FIELD (default `hk ID`).

use super::Engine;
use crate::corpus::Record;
use anyhow::{anyhow, bail, Context, Result};
use serde::Deserialize;
use serde_json::Value;
use std::time::Duration;

const ITEM_FIELDS: &str = "idReadable,customFields(name,$type,value)";
const TIMEOUT: Duration = Duration::from_secs(30);

#[derive(Deserialize)]
pub struct Item {
    #[serde(rename = "idReadable", default)]
    pub id_readable: String,
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
    agent: Option<ureq::Agent>,
}

impl YoutrackEngine {
    pub fn new() -> Self {
        Self { base: String::new(), token: String::new(), hk_field: String::new(), agent: None }
    }

    fn get_items(&self, path: &str, query: &str, top: usize) -> Result<Vec<Item>> {
        let agent = self.agent.as_ref().context("not built")?;
        let url = format!("{}{}", self.base, path);
        let resp = agent
            .get(&url)
            .header("Authorization", &format!("Bearer {}", self.token))
            .header("Accept", "application/json")
            .query("query", query)
            .query("fields", ITEM_FIELDS)
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

    fn key_for(&self, item: &Item, prefix: &str) -> String {
        match hk_id(&item.custom_fields, &self.hk_field) {
            Some(n) => format!("{prefix}:{n}"),
            None => format!("unmapped:{}", item.id_readable),
        }
    }
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

    fn build(&mut self, _recs: &[Record]) -> Result<()> {
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
        let config = ureq::Agent::config_builder().timeout_global(Some(TIMEOUT)).build();
        self.agent = Some(ureq::Agent::new_with_config(config));
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
        let issues = self.get_items("/api/issues", &text, k)?;
        let mut out: Vec<String> = issues.iter().take(k).map(|i| self.key_for(i, "task")).collect();
        if out.len() < k {
            let articles = self.get_items("/api/articles", &text, k - out.len())?;
            out.extend(articles.iter().take(k - out.len()).map(|a| self.key_for(a, "doc")));
        }
        Ok(out)
    }

    fn key_exists(&mut self, key: &str) -> Result<Option<bool>> {
        let (kind, id) = key.split_once(':').context("key has no kind")?;
        let path = match kind {
            "task" => "/api/issues",
            "doc" => "/api/articles",
            _ => return Ok(None),
        };
        let n: i64 = id.parse().context("key id is not numeric")?;
        let query = format!("{{{}}}: {n}", self.hk_field);
        let items = self.get_items(path, &query, 2)?;
        Ok(Some(items.iter().any(|i| hk_id(&i.custom_fields, &self.hk_field) == Some(n))))
    }

    fn setup_notes(&self) -> Vec<String> {
        vec![
            "YouTrack REST, GET only: /api/issues and /api/articles text search, native ranking".into(),
            format!("hk ID custom field '{}' maps issues to task:<id> and articles to doc:<id>", self.hk_field),
            "query terms are braced (literal); YouTrack ANDs them; unmapped items keep their rank".into(),
        ]
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
}

//! tantivy adapter. Two variants share one schema and one query path; they
//! differ only in the tokenizer registered for the `text` field.
//! Queries are tokenized with the same analyzer as the index and combined with
//! OR over BM25 scores.

use super::Engine;
use crate::corpus::Record;
use anyhow::{anyhow, Context, Result};
use lindera::{dictionary::load_dictionary, mode::Mode, segmenter::Segmenter};
use lindera_tantivy::tokenizer::LinderaTokenizer;
use std::fs;
use std::path::{Path, PathBuf};
use tantivy::{
    collector::TopDocs,
    doc,
    query::{BooleanQuery, Occur, Query, TermQuery},
    schema::{Field, IndexRecordOption, Schema, TextFieldIndexing, TextOptions, Value, STORED, STRING},
    Index, IndexReader, ReloadPolicy, TantivyDocument, Term,
};

#[derive(Clone, Copy)]
pub enum Variant {
    /// tantivy's built-in "default" tokenizer (baseline).
    Default,
    /// lindera with the embedded ko-dic dictionary, Normal mode, no POS filter.
    LinderaKoDic,
}

pub struct TantivyEngine {
    variant: Variant,
    dir: PathBuf,
    index: Option<Index>,
    reader: Option<IndexReader>,
    id_f: Option<Field>,
    text_f: Option<Field>,
}

impl TantivyEngine {
    pub fn new(variant: Variant, work: &Path) -> Self {
        let sub = match variant {
            Variant::Default => "tantivy-default",
            Variant::LinderaKoDic => "tantivy-lindera-ko-dic",
        };
        Self { variant, dir: work.join(sub), index: None, reader: None, id_f: None, text_f: None }
    }

    fn tokenizer_name(&self) -> &'static str {
        match self.variant {
            Variant::Default => "default",
            Variant::LinderaKoDic => "lang_ko",
        }
    }
}

fn dir_bytes(p: &Path) -> Result<u64> {
    let mut total = 0;
    for e in fs::read_dir(p)? {
        let e = e?;
        let m = e.metadata()?;
        total += if m.is_dir() { dir_bytes(&e.path())? } else { m.len() };
    }
    Ok(total)
}

impl Engine for TantivyEngine {
    fn name(&self) -> &'static str {
        match self.variant {
            Variant::Default => "tantivy-default",
            Variant::LinderaKoDic => "tantivy-lindera-ko-dic",
        }
    }

    fn modes(&self) -> Vec<&'static str> {
        vec!["or-bm25"]
    }

    fn build(&mut self, recs: &[Record]) -> Result<()> {
        let _ = fs::remove_dir_all(&self.dir);
        fs::create_dir_all(&self.dir)?;

        let mut sb = Schema::builder();
        let id_f = sb.add_text_field("id", STRING | STORED);
        let text_opts = TextOptions::default().set_indexing_options(
            TextFieldIndexing::default()
                .set_tokenizer(self.tokenizer_name())
                .set_index_option(IndexRecordOption::WithFreqsAndPositions),
        );
        let text_f = sb.add_text_field("text", text_opts);
        let index = Index::create_in_dir(&self.dir, sb.build())?;

        if let Variant::LinderaKoDic = self.variant {
            let dict = load_dictionary("embedded://ko-dic").map_err(|e| anyhow!("{e:?}"))?;
            let seg = Segmenter::new(Mode::Normal, dict, None);
            index.tokenizers().register("lang_ko", LinderaTokenizer::from_segmenter(seg));
        }

        let mut w = index.writer(256_000_000)?;
        for r in recs {
            w.add_document(doc!(id_f => r.key.as_str(), text_f => r.text.as_str()))?;
        }
        w.commit()?;
        drop(w);

        let reader = index.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()?;
        reader.reload()?;
        self.index = Some(index);
        self.reader = Some(reader);
        self.id_f = Some(id_f);
        self.text_f = Some(text_f);
        Ok(())
    }

    fn index_bytes(&mut self) -> Result<u64> {
        dir_bytes(&self.dir)
    }

    fn search(&mut self, _mode: &str, q: &str, k: usize) -> Result<Vec<String>> {
        let index = self.index.as_ref().context("not built")?;
        let reader = self.reader.as_ref().context("not built")?;
        let (id_f, text_f) = (self.id_f.context("not built")?, self.text_f.context("not built")?);
        let searcher = reader.searcher();

        let mut analyzer = index.tokenizers().get(self.tokenizer_name()).context("tokenizer")?;
        let mut terms: Vec<String> = Vec::new();
        let mut stream = analyzer.token_stream(q);
        while stream.advance() {
            terms.push(stream.token().text.clone());
        }
        terms.sort();
        terms.dedup();
        if terms.is_empty() {
            return Ok(Vec::new());
        }

        let clauses: Vec<(Occur, Box<dyn Query>)> = terms
            .iter()
            .map(|t| {
                let tq = TermQuery::new(Term::from_field_text(text_f, t), IndexRecordOption::WithFreqs);
                (Occur::Should, Box::new(tq) as Box<dyn Query>)
            })
            .collect();
        let query = BooleanQuery::new(clauses);
        let top = searcher.search(&query, &TopDocs::with_limit(k).order_by_score())?;

        let mut out = Vec::with_capacity(top.len());
        for (_score, addr) in top {
            let d: TantivyDocument = searcher.doc(addr)?;
            out.push(d.get_first(id_f).and_then(|v| v.as_str()).unwrap_or_default().to_string());
        }
        Ok(out)
    }

    fn setup_notes(&self) -> Vec<String> {
        match self.variant {
            Variant::Default => vec!["tantivy 0.27 default tokenizer; Hangul runs split only on whitespace/punctuation (no morphology)".into()],
            Variant::LinderaKoDic => vec!["lindera 6.2 ko-dic (embedded), Mode::Normal, no POS stop filter; particles such as 를/가 are indexed as tokens".into()],
        }
    }
}

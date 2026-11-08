//! Scoring for known-item queries.
//!
//! For a query with a non-empty gold set and the top-10 returned keys:
//! - hit@10    = some gold key is in the top 10
//! - MRR@10    = 1 / rank of the first gold key in the top 10, else 0
//! - recall@10 = |gold ∩ top10| / min(|gold|, 10)  (capped so broad gold sets
//!               are not penalised for exceeding 10)
//! No-answer queries (empty gold) are not scored; the report counts how many of
//! them returned any hit at all.

pub struct Score {
    pub hit: bool,
    pub mrr: f64,
    pub recall: f64,
    pub first_rank: Option<usize>,
}

pub fn score(gold: &[String], top10: &[String]) -> Option<Score> {
    if gold.is_empty() {
        return None;
    }
    let first_rank = top10.iter().position(|k| gold.contains(k)).map(|i| i + 1);
    let found = top10.iter().filter(|k| gold.contains(k)).count();
    let denom = gold.len().min(10) as f64;
    Some(Score {
        hit: first_rank.is_some(),
        mrr: first_rank.map(|r| 1.0 / r as f64).unwrap_or(0.0),
        recall: found as f64 / denom,
        first_rank,
    })
}

/// Nearest-rank percentile over microsecond samples.
pub fn percentile(sorted_us: &[u64], p: f64) -> u64 {
    if sorted_us.is_empty() {
        return 0;
    }
    let rank = ((p * sorted_us.len() as f64).ceil() as usize).clamp(1, sorted_us.len());
    sorted_us[rank - 1]
}

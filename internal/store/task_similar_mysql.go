package store

import (
	"context"
	"sort"
	"strings"
	"unicode"
)

// The duplicate check on MySQL (design §11.7). PostgreSQL ranks open titles
// with pg_trgm's similarity(); MySQL has no trigram operator, so the same
// measure runs in Go over the open tasks' titles. It is pg_trgm's measure on
// purpose: SimilarTaskThreshold keeps its meaning, a title is compared by the
// trigrams of its words, and creating a task depends on nothing but hk's own
// rows. The result is the same SimilarTask list, most similar first, ties by id.

// openTaskTitle is one open task the duplicate check compares against.
type openTaskTitle struct {
	ID                 int64
	Title, State, Lane string
}

// similarOpenTasksFor is similarOpenTasks for the store's dialect: PostgreSQL
// asks its trigram index, MySQL scores the open titles in Go. q is the caller's
// transaction or the store's querier.
func (s *Store) similarOpenTasksFor(ctx context.Context, q rowsQuerier, titles []string) ([]SimilarTask, error) {
	if s.dialect != mysqlDialect {
		return similarOpenTasks(ctx, q, titles)
	}
	return similarOpenTasksGo(ctx, q, titles)
}

// similarOpenTasksGo lists started, unstarted and backlog tasks whose title is
// similar to any of titles, most similar first, from the open tasks' titles.
func similarOpenTasksGo(ctx context.Context, q rowsQuerier, titles []string) ([]SimilarTask, error) {
	r, err := q.Query(ctx, `SELECT id, title, state, lane FROM tasks WHERE state IN ('backlog','unstarted',`+startedStatesSQL+`) ORDER BY id`)
	if err != nil {
		return nil, err
	}
	open, err := collectRows(r, func(r row) (openTaskTitle, error) {
		var t openTaskTitle
		err := r.Scan(&t.ID, &t.Title, &t.State, &t.Lane)
		return t, err
	})
	if err != nil {
		return nil, err
	}
	return rankSimilarTitles(open, titles, SimilarTaskThreshold, SimilarTaskLimit), nil
}

// rankSimilarTitles keeps the open tasks whose best similarity to any of titles
// is at least threshold, ordered by that score (highest first) and then id, at
// most limit of them. It is pure and deterministic.
func rankSimilarTitles(open []openTaskTitle, titles []string, threshold float64, limit int) []SimilarTask {
	grams := make([]map[string]struct{}, 0, len(titles))
	for _, title := range titles {
		grams = append(grams, titleTrigrams(title))
	}
	out := []SimilarTask{}
	for _, t := range open {
		tg := titleTrigrams(t.Title)
		best := 0.0
		for _, g := range grams {
			if score := trigramSimilarity(tg, g); score > best {
				best = score
			}
		}
		if best >= threshold {
			out = append(out, SimilarTask{ID: t.ID, Title: t.Title, State: t.State, Lane: t.Lane, Similarity: best})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		return out[i].ID < out[j].ID
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// titleTrigrams is pg_trgm's trigram set of s: lower-cased, split into words of
// letters and digits, each word padded with two spaces in front and one behind.
func titleTrigrams(s string) map[string]struct{} {
	set := map[string]struct{}{}
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, w := range words {
		padded := []rune("  " + w + " ")
		for i := 0; i+3 <= len(padded); i++ {
			set[string(padded[i:i+3])] = struct{}{}
		}
	}
	return set
}

// trigramSimilarity is the shared trigrams over the distinct trigrams of both,
// computed in single precision like pg_trgm's float4 so a score at the
// threshold compares the same way.
func trigramSimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	common := 0
	for g := range a {
		if _, ok := b[g]; ok {
			common++
		}
	}
	return float64(float32(common) / float32(len(a)+len(b)-common))
}

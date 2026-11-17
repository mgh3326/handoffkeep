package store

import (
	"context"
	"sort"
	"strings"
	"unicode"
)

// Intake on MySQL (hk 5074): the duplicate check's title similarity and the
// YouTrack issue enqueue step, which intake shares with no other file.

// intakeEnqueueYTIssueTx is intake's and ticket sets' step after a task insert.
// PostgreSQL always enqueues (the gate is inside enqueueYTIssueTx). On MySQL the
// enqueue is a documented no-op while the YouTrack issue gate is off, exactly
// the gate ensureYTIssueTx tests first; with the gate on it needs the yt_*
// tables of B4 and refuses.
func (s *Store) intakeEnqueueYTIssueTx(ctx context.Context, tx transaction, t Task) error {
	if s.dialect == mysqlDialect {
		if mode, _ := s.ytIssuesNow(); mode != YTIssuesOn {
			return nil
		}
		// B4 dependency: enqueueYTIssueTx writes yt_write_ops and takes pgx.Tx.
		return ErrDialectUnsupported
	}
	return s.enqueueYTIssueTx(ctx, pgxTx(tx), t)
}

// intakeTrigrams is pg_trgm's trigram set of text: lower-cased, split into words
// of letters and digits, each word padded with two spaces before and one after.
// The set holds each trigram once, as show_trgm does.
func intakeTrigrams(text string) map[string]struct{} {
	set := map[string]struct{}{}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
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

// intakeSimilarity is pg_trgm similarity(): shared trigrams over all trigrams,
// computed in float32 like the extension's float4 result.
func intakeSimilarity(a, b map[string]struct{}) float32 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(small) > len(large) {
		small, large = large, small
	}
	common := 0
	for k := range small {
		if _, ok := large[k]; ok {
			common++
		}
	}
	return float32(common) / float32(len(a)+len(b)-common)
}

// intakeSimilarOpenTasks is similarOpenTasks for MySQL: the open (backlog,
// unstarted, started) titles are read and scored in Go against each title.
// Order and limit are the PostgreSQL query's: best score first, then id.
func intakeSimilarOpenTasks(ctx context.Context, q rowsQuerier, titles []string) ([]SimilarTask, error) {
	wanted := make([]map[string]struct{}, 0, len(titles))
	for _, t := range titles {
		wanted = append(wanted, intakeTrigrams(t))
	}
	rs, err := q.Query(ctx, `SELECT id,title,state,lane FROM tasks WHERE state IN ('backlog','unstarted',`+startedStatesSQL+`) ORDER BY id`)
	if err != nil {
		return nil, err
	}
	open, err := collectRows(rs, func(r row) (SimilarTask, error) {
		var x SimilarTask
		return x, r.Scan(&x.ID, &x.Title, &x.State, &x.Lane)
	})
	if err != nil {
		return nil, err
	}
	out := []SimilarTask{}
	for _, x := range open {
		have := intakeTrigrams(x.Title)
		var best float32
		for _, w := range wanted {
			if sim := intakeSimilarity(have, w); sim > best {
				best = sim
			}
		}
		if float64(best) >= SimilarTaskThreshold {
			x.Similarity = float64(best)
			out = append(out, x)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > SimilarTaskLimit {
		out = out[:SimilarTaskLimit]
	}
	return out, nil
}

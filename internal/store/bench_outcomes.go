package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// BenchOutcome holds a rep's experiment and outcome columns (schema 40).
// Every field is nullable: nil is unknown, never zero or false.
type BenchOutcome struct {
	ExperimentID     *string `json:"experiment_id"`
	Arm              *string `json:"arm"`
	FirstCIGreen     *bool   `json:"first_ci_green"`
	CriticalFindings *int    `json:"critical_findings"`
	HighFindings     *int    `json:"high_findings"`
	Merged           *bool   `json:"merged"`
	RevertedWithin7d *bool   `json:"reverted_within_7d"`
	ReviewerProfile  *string `json:"reviewer_profile"`
	ReviewerEffort   *string `json:"reviewer_effort"`
	ReasoningTokens  *int64  `json:"reasoning_tokens"`
	CacheReadTokens  *int64  `json:"cache_read_tokens"`
	CacheWriteTokens *int64  `json:"cache_write_tokens"`
	SpawnToReadyS    *int    `json:"spawn_to_ready_s"`
	SpawnToMergeS    *int    `json:"spawn_to_merge_s"`
}

// benchOutcomeColumns lists BenchOutcome's columns in field order; fields
// returns pointers to the struct fields in the same order.
var benchOutcomeColumns = []string{"experiment_id", "arm", "first_ci_green", "critical_findings", "high_findings", "merged", "reverted_within_7d", "reviewer_profile", "reviewer_effort", "reasoning_tokens", "cache_read_tokens", "cache_write_tokens", "spawn_to_ready_s", "spawn_to_merge_s"}

func (o *BenchOutcome) fields() []any {
	return []any{&o.ExperimentID, &o.Arm, &o.FirstCIGreen, &o.CriticalFindings, &o.HighFindings, &o.Merged, &o.RevertedWithin7d, &o.ReviewerProfile, &o.ReviewerEffort, &o.ReasoningTokens, &o.CacheReadTokens, &o.CacheWriteTokens, &o.SpawnToReadyS, &o.SpawnToMergeS}
}

// values returns the field values, as query arguments, in column order.
func (o BenchOutcome) values() []any {
	return []any{o.ExperimentID, o.Arm, o.FirstCIGreen, o.CriticalFindings, o.HighFindings, o.Merged, o.RevertedWithin7d, o.ReviewerProfile, o.ReviewerEffort, o.ReasoningTokens, o.CacheReadTokens, o.CacheWriteTokens, o.SpawnToReadyS, o.SpawnToMergeS}
}

func (o BenchOutcome) same(p BenchOutcome) bool {
	return benchRepPtrEq(o.ExperimentID, p.ExperimentID) && benchRepPtrEq(o.Arm, p.Arm) &&
		benchRepPtrEq(o.FirstCIGreen, p.FirstCIGreen) && benchRepPtrEq(o.CriticalFindings, p.CriticalFindings) &&
		benchRepPtrEq(o.HighFindings, p.HighFindings) && benchRepPtrEq(o.Merged, p.Merged) &&
		benchRepPtrEq(o.RevertedWithin7d, p.RevertedWithin7d) && benchRepPtrEq(o.ReviewerProfile, p.ReviewerProfile) &&
		benchRepPtrEq(o.ReviewerEffort, p.ReviewerEffort) && benchRepPtrEq(o.ReasoningTokens, p.ReasoningTokens) &&
		benchRepPtrEq(o.CacheReadTokens, p.CacheReadTokens) && benchRepPtrEq(o.CacheWriteTokens, p.CacheWriteTokens) &&
		benchRepPtrEq(o.SpawnToReadyS, p.SpawnToReadyS) && benchRepPtrEq(o.SpawnToMergeS, p.SpawnToMergeS)
}

func (o BenchOutcome) valid() error {
	if o.Arm != nil && *o.Arm != "baseline" && *o.Arm != "arm" {
		return errors.New("invalid bench rep: arm must be baseline or arm")
	}
	for _, n := range []*int{o.CriticalFindings, o.HighFindings, o.SpawnToReadyS, o.SpawnToMergeS} {
		if n != nil && *n < 0 {
			return errors.New("invalid bench rep: counts must be >= 0")
		}
	}
	for _, n := range []*int64{o.ReasoningTokens, o.CacheReadTokens, o.CacheWriteTokens} {
		if n != nil && *n < 0 {
			return errors.New("invalid bench rep: token counts must be >= 0")
		}
	}
	for _, s := range []*string{o.ExperimentID, o.ReviewerProfile, o.ReviewerEffort} {
		if s == nil {
			continue
		}
		if !validBenchText(*s) {
			return errors.New("invalid bench rep text")
		}
		if err := guard.Reject(*s); err != nil {
			return err
		}
	}
	return nil
}

// benchRepColumns is every bench_reps column a read returns; scanBenchRep
// reads them in this order.
var benchRepColumns = `id,origin_id,profile,model_id,task_ref,tier,role,rounds,blockers_found,completed,input_tokens,output_tokens,notes,recorded_at,effort,grade,table_grade,created_by,created_at,` + strings.Join(benchOutcomeColumns, ",")

func scanBenchRep(row pgx.Row) (BenchRep, error) {
	var x BenchRep
	dst := append([]any{&x.ID, &x.OriginID, &x.Profile, &x.ModelID, &x.TaskRef, &x.Tier, &x.Role, &x.Rounds, &x.BlockersFound, &x.Completed, &x.InputTokens, &x.OutputTokens, &x.Notes, &x.RecordedAt, &x.Effort, &x.Grade, &x.TableGrade, &x.CreatedBy, &x.CreatedAt}, x.BenchOutcome.fields()...)
	if err := row.Scan(dst...); err != nil {
		return x, err
	}
	x.RecordedAt = x.RecordedAt.UTC()
	x.CreatedAt = x.CreatedAt.UTC()
	return x, nil
}

// BenchRepOutcomePatch is a set-once patch of a stored rep: the outcome
// columns plus input and output tokens, which a rep recorded before usage
// was known can get later.
type BenchRepOutcomePatch struct {
	BenchOutcome
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

// BenchRepOutcomeConflictError rejects a patch whose value differs from a
// column that already holds one; Field names the column.
type BenchRepOutcomeConflictError struct{ Field string }

func (e *BenchRepOutcomeConflictError) Error() string {
	return "bench_rep_outcome_conflict: " + e.Field + " already holds a different value"
}

// ErrBenchRepNotFound is a patch of an id with no rep.
var ErrBenchRepNotFound = errors.New("bench_rep_not_found")

// PatchBenchRepOutcome fills the patch's non-nil columns of rep id that are
// still NULL. A column that already holds the same value is a no-op; one that
// holds a different value rejects the whole patch with
// *BenchRepOutcomeConflictError, writing nothing. An empty patch is invalid.
func (s *Store) PatchBenchRepOutcome(ctx context.Context, id int64, p BenchRepOutcomePatch) (BenchRep, error) {
	var x BenchRep
	if id < 1 {
		return x, errors.New("invalid bench rep id")
	}
	if err := p.BenchOutcome.valid(); err != nil {
		return x, err
	}
	for _, n := range []*int64{p.InputTokens, p.OutputTokens} {
		if n != nil && *n < 0 {
			return x, errors.New("invalid bench rep: token counts must be >= 0")
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return x, err
	}
	defer tx.Rollback(ctx)
	x, err = scanBenchRep(tx.QueryRow(ctx, `SELECT `+benchRepColumns+` FROM bench_reps WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return x, ErrBenchRepNotFound
	}
	if err != nil {
		return x, err
	}
	stored := append(x.BenchOutcome.fields(), &x.InputTokens, &x.OutputTokens)
	patch := append(p.BenchOutcome.fields(), &p.InputTokens, &p.OutputTokens)
	names := append(append([]string{}, benchOutcomeColumns...), "input_tokens", "output_tokens")
	var sets []string
	var args []any
	given := 0
	for i := range names {
		changed, present, err := setOnce(stored[i], patch[i])
		if err != nil {
			return x, &BenchRepOutcomeConflictError{Field: names[i]}
		}
		if present {
			given++
		}
		if changed {
			args = append(args, patchValue(patch[i]))
			sets = append(sets, fmt.Sprintf("%s=$%d", names[i], len(args)+1))
		}
	}
	if given == 0 {
		return x, errors.New("invalid bench rep patch: no field given")
	}
	if len(sets) == 0 {
		return x, nil
	}
	x, err = scanBenchRep(tx.QueryRow(ctx, `UPDATE bench_reps SET `+strings.Join(sets, ",")+` WHERE id=$1 RETURNING `+benchRepColumns, append([]any{id}, args...)...))
	if err != nil {
		return x, err
	}
	return x, tx.Commit(ctx)
}

var errSetOnce = errors.New("set-once conflict")

// setOnce compares one stored column with its patch value. Both arguments
// are pointers to the same nullable field type.
func setOnce(stored, patch any) (changed, present bool, err error) {
	switch s := stored.(type) {
	case **string:
		return setOnceField(s, patch.(**string))
	case **bool:
		return setOnceField(s, patch.(**bool))
	case **int:
		return setOnceField(s, patch.(**int))
	case **int64:
		return setOnceField(s, patch.(**int64))
	}
	panic(fmt.Sprintf("setOnce: unsupported field %T", stored))
}

func setOnceField[T comparable](stored, patch **T) (changed, present bool, err error) {
	if *patch == nil {
		return false, false, nil
	}
	if *stored == nil {
		return true, true, nil
	}
	if **stored != **patch {
		return false, true, errSetOnce
	}
	return false, true, nil
}

func patchValue(p any) any {
	switch v := p.(type) {
	case **string:
		return *v
	case **bool:
		return *v
	case **int:
		return *v
	case **int64:
		return *v
	}
	panic(fmt.Sprintf("patchValue: unsupported field %T", p))
}

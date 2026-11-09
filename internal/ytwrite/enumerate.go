package ytwrite

import (
	"context"
	"errors"
	"fmt"
)

// ErrInconsistentEnumeration means offset pages did not produce the same set
// twice within the retry bound; callers must not adopt evidence from them.
var ErrInconsistentEnumeration = errors.New("inconsistent_comment_enumeration")

// ErrEnumerationTooLarge means one read of the target's comments passed
// MaxEnumeratedComments; the evidence search gives up rather than page without
// end (the op stays remote_unknown).
var ErrEnumerationTooLarge = errors.New("comment_enumeration_too_large")

// MaxEnumeratedComments bounds one read of one issue's comments. An hk issue
// with more comments than this is not expected; it is an error, not a cut.
const MaxEnumeratedComments = 10000

// EnumerateStableComments reads all offset pages twice per attempt. It accepts
// only identical sets, including text, update time and deletion state. The
// page function must return an empty page at the end and honor ctx.
func EnumerateStableComments(ctx context.Context, pageSize int, page func(context.Context, int, int) ([]RemoteComment, error)) ([]RemoteComment, error) {
	if pageSize <= 0 {
		return nil, fmt.Errorf("%w: page size must be positive", ErrInvalid)
	}
	read := func() (map[string]RemoteComment, error) {
		out := make(map[string]RemoteComment)
		for skip := 0; ; {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			items, err := page(ctx, skip, pageSize)
			if err != nil {
				return nil, err
			}
			if len(items) == 0 {
				return out, nil
			}
			if len(out)+len(items) > MaxEnumeratedComments {
				return nil, ErrEnumerationTooLarge
			}
			for _, c := range items {
				if c.ID == "" {
					return nil, ErrInconsistentEnumeration
				}
				if _, exists := out[c.ID]; exists {
					return nil, ErrInconsistentEnumeration
				}
				out[c.ID] = c
			}
			skip += len(items)
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		a, err := read()
		if err != nil && !errors.Is(err, ErrInconsistentEnumeration) {
			return nil, err
		}
		if err != nil {
			continue
		}
		b, err := read()
		if err != nil && !errors.Is(err, ErrInconsistentEnumeration) {
			return nil, err
		}
		if err != nil || len(a) != len(b) {
			continue
		}
		equal := true
		for id, x := range a {
			y, ok := b[id]
			if !ok || x.Text != y.Text || x.Deleted != y.Deleted || !x.UpdatedAt.Equal(y.UpdatedAt) {
				equal = false
				break
			}
		}
		if equal {
			result := make([]RemoteComment, 0, len(a))
			for _, c := range a {
				result = append(result, c)
			}
			return result, nil
		}
	}
	return nil, ErrInconsistentEnumeration
}

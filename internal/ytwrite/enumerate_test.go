package ytwrite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

func TestEnumerateStableComments(t *testing.T) {
	ctx := context.Background()
	t.Run("retries_changed_update", func(t *testing.T) {
		calls := 0
		page := func(_ context.Context, skip, _ int) ([]ytwrite.RemoteComment, error) {
			if skip > 0 {
				return nil, nil
			}
			calls++
			at := time.Unix(1, 0)
			if calls > 1 {
				at = time.Unix(2, 0)
			}
			return []ytwrite.RemoteComment{{ID: "1", UpdatedAt: at}}, nil
		}
		got, err := ytwrite.EnumerateStableComments(ctx, 1, page)
		if err != nil || len(got) != 1 || !got[0].UpdatedAt.Equal(time.Unix(2, 0)) || calls != 4 {
			t.Fatalf("got %+v, calls %d, err %v", got, calls, err)
		}
	})
	t.Run("inconsistent_exhausts_retries", func(t *testing.T) {
		calls := 0
		page := func(_ context.Context, skip, _ int) ([]ytwrite.RemoteComment, error) {
			if skip > 0 {
				return nil, nil
			}
			calls++
			return []ytwrite.RemoteComment{{ID: "1", Deleted: calls%2 == 0}}, nil
		}
		_, err := ytwrite.EnumerateStableComments(ctx, 1, page)
		if !errors.Is(err, ytwrite.ErrInconsistentEnumeration) || calls != 6 {
			t.Fatalf("calls %d, err %v", calls, err)
		}
	})
}

package api

import (
	"bytes"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// GET /metrics exposes the YouTrack write outbox under fixed names: the alert
// rules in deploy/observability read these lines, so the names, the label and
// the order are the contract. Every open state is reported, even at zero.
func TestWriteYTWriteOutboxMetrics(t *testing.T) {
	var b bytes.Buffer
	writeYTWriteOutboxMetrics(&b, store.YTWriteOutbox{
		Open: map[ytwrite.State]int64{
			ytwrite.StatePrepared:      2,
			ytwrite.StateDispatched:    1,
			ytwrite.StateRemoteUnknown: 0,
		},
		OldestAge: map[ytwrite.State]time.Duration{
			ytwrite.StatePrepared:   90 * time.Second,
			ytwrite.StateDispatched: 18 * time.Minute,
		},
		Retrying:    1,
		Reconciling: 0,
	})
	want := `handoffkeep_yt_write_ops_open{state="prepared"} 2
handoffkeep_yt_write_oldest_open_age_seconds{state="prepared"} 90
handoffkeep_yt_write_ops_open{state="dispatched"} 1
handoffkeep_yt_write_oldest_open_age_seconds{state="dispatched"} 1080
handoffkeep_yt_write_ops_open{state="remote_unknown"} 0
handoffkeep_yt_write_oldest_open_age_seconds{state="remote_unknown"} 0
handoffkeep_yt_write_ops_retrying 1
handoffkeep_yt_write_ops_reconciling 0
`
	if got := b.String(); got != want {
		t.Fatalf("outbox metrics:\n%s\nwant:\n%s", got, want)
	}
}

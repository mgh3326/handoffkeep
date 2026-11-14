package api

import (
	"fmt"
	"io"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// writeYTWriteOutboxMetrics appends the YouTrack write outbox gauges, Prometheus
// text format (deploy/observability/yt-write-outbox.rules.yml reads them):
//
//	handoffkeep_yt_write_ops_open{state="prepared"} <n>
//	handoffkeep_yt_write_oldest_open_age_seconds{state="prepared"} <seconds>
//	handoffkeep_yt_write_ops_retrying <n>
//	handoffkeep_yt_write_ops_reconciling <n>
//
// Every open state is written, zero included, so a state never drops out of a
// series.
func writeYTWriteOutboxMetrics(w io.Writer, o store.YTWriteOutbox) {
	for _, st := range store.YTWriteOutboxStates {
		label := promLabel(string(st))
		fmt.Fprintf(w, "handoffkeep_yt_write_ops_open{state=%s} %d\n", label, o.Open[st])
		fmt.Fprintf(w, "handoffkeep_yt_write_oldest_open_age_seconds{state=%s} %d\n", label, int64(o.OldestAge[st].Seconds()))
	}
	fmt.Fprintf(w, "handoffkeep_yt_write_ops_retrying %d\n", o.Retrying)
	fmt.Fprintf(w, "handoffkeep_yt_write_ops_reconciling %d\n", o.Reconciling)
}

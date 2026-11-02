package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func syncCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "delta" {
		return errors.New("usage: handoffkeep sync delta --after N [--limit N]")
	}
	fs := flag.NewFlagSet("sync delta", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	after := fs.Int64("after", 0, "last applied sync ID")
	limit := fs.Int("limit", 100, "page size (1-1000)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *after < 0 || *limit < 1 || *limit > store.SyncLimit {
		return errors.New("invalid sync arguments")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	changes, meta, err := c.SyncDelta(ctx, *after, *limit)
	if err != nil {
		return err
	}
	return printJSON(out, struct {
		Changes  []store.SyncChange `json:"changes"`
		Metadata store.SyncMetadata `json:"_metadata_"`
	}{changes, meta})
}

func syncRetentionFromEnv() (time.Duration, error) {
	raw := os.Getenv("HANDOFFKEEP_SYNC_RETENTION")
	if raw == "" {
		return 30 * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return 0, errors.New("HANDOFFKEEP_SYNC_RETENTION must be a positive duration (for example 720h)")
	}
	return duration, nil
}

type syncRetentionWorker struct {
	store     *store.Store
	retention time.Duration
	interval  time.Duration
}

func (w syncRetentionWorker) Run(ctx context.Context) {
	interval := w.interval
	if interval <= 0 {
		interval = time.Hour
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		// Drain old rows in bounded transactions without leaving a large backlog
		// for the next hourly tick.
		for ctx.Err() == nil {
			n, err := w.store.PruneSync(ctx, w.retention)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("sync.prune_error err=%q", err)
				}
				break
			}
			if n < 10000 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

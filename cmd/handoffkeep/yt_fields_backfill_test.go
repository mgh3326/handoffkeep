package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// fields-backfill refuses, before any database or YouTrack use, unless the
// fields mode is on (hk 4022), and rejects bad arguments with its usage.
func TestYouTrackFieldsBackfillRefusesModeOff(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DB_URL", "")
	for _, mode := range []string{"", "off"} {
		t.Setenv("HANDOFFKEEP_YT_FIELDS_MODE", mode)
		err := run([]string{"youtrack", "fields-backfill", "--dry-run"}, io.Discard, io.Discard)
		var exit exitCodeError
		if !errors.As(err, &exit) || exit.code != youtrackExitFieldsOff || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_FIELDS_MODE") {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
	t.Setenv("HANDOFFKEEP_YT_FIELDS_MODE", "bogus")
	var exit exitCodeError
	if err := run([]string{"yt", "fields-backfill"}, io.Discard, io.Discard); !errors.As(err, &exit) || exit.code != youtrackExitConfig {
		t.Fatalf("unknown mode: %v", err)
	}
	t.Setenv("HANDOFFKEEP_YT_FIELDS_MODE", "on")
	for _, args := range [][]string{{"extra"}, {"--bogus"}, {"--limit=-1"}, {"--task=-2"}} {
		if err := run(append([]string{"youtrack", "fields-backfill"}, args...), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "fields-backfill") {
			t.Errorf("%v: %v", args, err)
		}
	}
	err := run([]string{"yt", "fields-backfill", "--project", "p", "--limit", "2", "--task", "3", "--dry-run"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_DB_URL") {
		t.Fatalf("no database: %v", err)
	}
}

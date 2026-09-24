package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/fleetmetrics"
)

func TestFleetMetricsFromSnapshot(t *testing.T) {
	since := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	snap := fleetmetrics.Snapshot{Schema: fleetmetrics.SnapshotSchema, Since: since, Until: since.Add(24 * time.Hour), CollectedAt: since.Add(24 * time.Hour)}
	path := filepath.Join(t.TempDir(), "snap.json")
	b, _ := json.Marshal(snap)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	noRun := func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("a snapshot run must not execute any command")
		return nil, nil
	}
	var out bytes.Buffer
	if err := fleetMetricsRun([]string{"--from-snapshot", path}, &out, time.Now(), noRun); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"## 1. order → usable result", "## 5. finished on the normal path", "not observed (n=0)", "COVERAGE"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "p50 0.00h") {
		t.Fatalf("an empty snapshot printed a zero value:\n%s", text)
	}
	out.Reset()
	if err := fleetMetricsRun([]string{"--from-snapshot", path, "--format", "json"}, &out, time.Now(), noRun); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report["lead_time"] == nil {
		t.Fatalf("json output = %s (%v)", out.String(), err)
	}
}

func TestFleetMetricsArgs(t *testing.T) {
	until := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	if got, err := parseSince("7d", until); err != nil || !got.Equal(until.Add(-168*time.Hour)) {
		t.Fatalf("7d = %v %v", got, err)
	}
	if got, err := parseSince("36h", until); err != nil || !got.Equal(until.Add(-36*time.Hour)) {
		t.Fatalf("36h = %v %v", got, err)
	}
	if _, err := parseSince("0d", until); err == nil {
		t.Fatal("0d accepted")
	}
	for _, args := range [][]string{
		{"--format", "csv", "--machine", "m"},
		{"--slots", "m=x", "--machine", "m"},
		{"--slots", "m=3@yesterday", "--machine", "m"},
		{},
	} {
		if err := fleetMetricsRun(args, &bytes.Buffer{}, until, nil); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}

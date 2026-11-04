package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestRevision(t *testing.T) {
	t.Cleanup(func() { read = debug.ReadBuildInfo })
	read = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.time", Value: "x"}, {Key: "vcs.revision", Value: "0123abc"}}}, true
	}
	if got := Revision(); got != "0123abc" {
		t.Fatalf("Revision=%q", got)
	}
	read = func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{}, true }
	if got := Revision(); got != "unknown" {
		t.Fatalf("unstamped Revision=%q", got)
	}
	read = func() (*debug.BuildInfo, bool) { return nil, false }
	if got := Revision(); got != "unknown" {
		t.Fatalf("no build info Revision=%q", got)
	}
}

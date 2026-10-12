package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
)

func stubBuildInfo(t *testing.T, info *debug.BuildInfo, ok bool) {
	t.Helper()
	previous := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) { return info, ok }
	t.Cleanup(func() { readBuildInfo = previous })
}

const (
	testVCSRevision = "0123456789abcdef0123456789abcdef01234567"
	testVCSTime     = "2026-09-28T00:59:04Z"
	testGoVersion   = "go1.27.0"
	testReleaseTag  = "v1.2.3"
)

func stampedBuildInfo() *debug.BuildInfo {
	return &debug.BuildInfo{
		GoVersion: testGoVersion,
		Main:      debug.Module{Path: "github.com/mgh3326/handoffkeep", Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: testVCSRevision},
			{Key: "vcs.time", Value: testVCSTime},
			{Key: "vcs.modified", Value: "false"},
		},
	}
}

func stubReleaseTag(t *testing.T, tag string) {
	t.Helper()
	previous := releaseTag
	releaseTag = tag
	t.Cleanup(func() { releaseTag = previous })
}

var wantVersionFields = map[string]string{
	"rev":      testVCSRevision,
	"time":     testVCSTime,
	"modified": "false",
	"module":   "(devel)",
	"go":       testGoVersion,
	"release":  testReleaseTag,
}

// TestVersionTextOutput pins the exact single-line text format of
// handoffkeep version for a fully VCS-stamped build.
func TestVersionTextOutput(t *testing.T) {
	stubBuildInfo(t, stampedBuildInfo(), true)
	stubReleaseTag(t, testReleaseTag)
	var out bytes.Buffer
	if err := run([]string{"version"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "handoffkeep rev=" + testVCSRevision + " time=" + testVCSTime + " modified=false module=(devel) go=" + testGoVersion + " release=" + testReleaseTag + "\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
}

// TestVersionJSONOutput requires exactly the six keys, each equal to the
// stamped build-info value.
func TestVersionJSONOutput(t *testing.T) {
	stubBuildInfo(t, stampedBuildInfo(), true)
	stubReleaseTag(t, testReleaseTag)
	var out bytes.Buffer
	if err := run([]string{"version", "--json"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fields, wantVersionFields) {
		t.Fatalf("got %v want %v", fields, wantVersionFields)
	}
}

// TestVersionNoBuildInfo covers ReadBuildInfo reporting ok=false: Go build
// info fields are "unknown" while the separate release stamp remains present.
func TestVersionNoBuildInfo(t *testing.T) {
	stubBuildInfo(t, nil, false)
	stubReleaseTag(t, testReleaseTag)
	var out bytes.Buffer
	if err := run([]string{"version"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "handoffkeep rev=unknown time=unknown modified=unknown module=unknown go=unknown release=" + testReleaseTag + "\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
	out.Reset()
	if err := run([]string{"version", "--json"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"rev", "time", "modified", "module", "go"} {
		if fields[key] != "unknown" {
			t.Fatalf("%s=%q want unknown (%v)", key, fields[key], fields)
		}
	}
	if fields["release"] != testReleaseTag {
		t.Fatalf("release=%q want %q (%v)", fields["release"], testReleaseTag, fields)
	}
	if len(fields) != 6 {
		t.Fatalf("got %d keys: %v", len(fields), fields)
	}
}

// TestVersionWithoutReleaseStamp keeps a build without the linker stamp
// explicit: release is "unknown" even when Go build info is available.
func TestVersionWithoutReleaseStamp(t *testing.T) {
	stubBuildInfo(t, stampedBuildInfo(), true)
	stubReleaseTag(t, "")
	var out bytes.Buffer
	if err := run([]string{"version"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "handoffkeep rev=" + testVCSRevision + " time=" + testVCSTime + " modified=false module=(devel) go=" + testGoVersion + " release=unknown\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
	out.Reset()
	if err := run([]string{"version", "--json"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["release"] != "unknown" || len(fields) != 6 {
		t.Fatalf("release fields=%v want release=unknown and six keys", fields)
	}
}

// TestVersionWithoutVCSStamp covers a build that carries a module version
// but no vcs.* settings (a go install module@ref build): rev, time and
// modified stay "unknown" and module is the pseudo-version verbatim.
func TestVersionWithoutVCSStamp(t *testing.T) {
	pseudo := "v0.0.0-20260928005904-c9dafb3d343e"
	stubBuildInfo(t, &debug.BuildInfo{
		GoVersion: testGoVersion,
		Main:      debug.Module{Path: "github.com/mgh3326/handoffkeep", Version: pseudo},
	}, true)
	stubReleaseTag(t, testReleaseTag)
	var out bytes.Buffer
	if err := run([]string{"version"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "handoffkeep rev=unknown time=unknown modified=unknown module=" + pseudo + " go=" + testGoVersion + " release=" + testReleaseTag + "\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
	out.Reset()
	if err := run([]string{"version", "--json"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := map[string]string{
		"rev":      "unknown",
		"time":     "unknown",
		"modified": "unknown",
		"module":   pseudo,
		"go":       testGoVersion,
		"release":  testReleaseTag,
	}
	if !reflect.DeepEqual(fields, wantFields) {
		t.Fatalf("got %v want %v", fields, wantFields)
	}
}

// TestVersionEmptyVCSValues pins that a vcs.* setting present in the
// build stamp with an empty Value still reports the literal "unknown" —
// a field is never printed empty — in both formats.
func TestVersionEmptyVCSValues(t *testing.T) {
	stubBuildInfo(t, &debug.BuildInfo{
		GoVersion: testGoVersion,
		Main:      debug.Module{Path: "github.com/mgh3326/handoffkeep", Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: ""},
			{Key: "vcs.time", Value: ""},
			{Key: "vcs.modified", Value: ""},
		},
	}, true)
	stubReleaseTag(t, testReleaseTag)
	var out bytes.Buffer
	if err := run([]string{"version"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "handoffkeep rev=unknown time=unknown modified=unknown module=(devel) go=" + testGoVersion + " release=" + testReleaseTag + "\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
	out.Reset()
	if err := run([]string{"version", "--json"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := map[string]string{
		"rev":      "unknown",
		"time":     "unknown",
		"modified": "unknown",
		"module":   "(devel)",
		"go":       testGoVersion,
		"release":  testReleaseTag,
	}
	if !reflect.DeepEqual(fields, wantFields) {
		t.Fatalf("got %v want %v", fields, wantFields)
	}
}

// TestVersionFlagAlias proves handoffkeep --version is byte-for-byte the
// same output as handoffkeep version, arguments included.
func TestVersionFlagAlias(t *testing.T) {
	stubBuildInfo(t, stampedBuildInfo(), true)
	stubReleaseTag(t, testReleaseTag)
	for _, args := range [][]string{{}, {"--json"}} {
		var sub, flagOut bytes.Buffer
		if err := run(append([]string{"version"}, args...), &sub, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if err := run(append([]string{"--version"}, args...), &flagOut, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sub.Bytes(), flagOut.Bytes()) {
			t.Fatalf("args=%v: --version %q differs from version %q", args, flagOut.String(), sub.String())
		}
	}
}

// TestVersionIgnoresEnvironment clears every HK_/HANDOFFKEEP_ variable the
// CLI reads and requires the same output as a stamped build: the version
// command reads no environment, config file, or remote server.
func TestVersionIgnoresEnvironment(t *testing.T) {
	for _, name := range []string{
		"HK_LINEAR_API_URL", "HK_LINEAR_TEAM_ID", "HK_LINEAR_API_KEY", "HK_ALERT_DISCORD_WEBHOOK",
		"HANDOFFKEEP_URL", "HANDOFFKEEP_TOKEN", "HANDOFFKEEP_DB_URL", "HANDOFFKEEP_AUTH_FILE",
		"HANDOFFKEEP_UI_CF_TEAM_DOMAIN", "HANDOFFKEEP_UI_CF_AUD", "HANDOFFKEEP_UI_ALLOWED_EMAILS",
		"HANDOFFKEEP_UI_ALLOWED_SERVICE_NAMES", "HANDOFFKEEP_HUB_URL", "HANDOFFKEEP_HUB_TOKEN",
	} {
		t.Setenv(name, "")
	}
	stubBuildInfo(t, stampedBuildInfo(), true)
	stubReleaseTag(t, testReleaseTag)
	var out bytes.Buffer
	if err := run([]string{"version"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "handoffkeep rev=" + testVCSRevision + " time=" + testVCSTime + " modified=false module=(devel) go=" + testGoVersion + " release=" + testReleaseTag + "\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
}

// TestVersionRejectsArguments keeps the command surface to the single
// --json flag.
func TestVersionRejectsArguments(t *testing.T) {
	stubBuildInfo(t, stampedBuildInfo(), true)
	stubReleaseTag(t, testReleaseTag)
	if err := run([]string{"version", "extra"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for positional argument")
	}
	if err := run([]string{"version", "--bogus"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

// TestVersionListedInUsage requires both usage errors to name the version
// subcommand.
func TestVersionListedInUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}} {
		err := run(args, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.HasPrefix(err.Error(), "usage: handoffkeep") || !strings.Contains(err.Error(), "version") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

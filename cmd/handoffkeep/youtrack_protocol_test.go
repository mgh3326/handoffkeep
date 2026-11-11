package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// protocolTestDB points at a port nothing listens on. The guard must refuse
// before any connection is attempted, so a refusal that reached the store
// would fail with a connection error, not the protocol exit code.
const protocolTestDB = "postgres://protocol-test.invalid:1/none?sslmode=disable"

// protocolEnv clears every variable the legacy push reads, then sets the
// modes under test and the credentials of a configured legacy push.
func protocolEnv(t *testing.T, comments, issues string) {
	t.Helper()
	for _, name := range []string{"HANDOFFKEEP_YT_COMMENTS_MODE", "HANDOFFKEEP_YT_ISSUES_MODE", "HANDOFFKEEP_YT_COMMENTS_CATCHUP", "HANDOFFKEEP_YT_READTHROUGH_MODE", "HANDOFFKEEP_DB_URL", "YOUTRACK_URL", "YOUTRACK_BOT_API_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", comments)
	t.Setenv("HANDOFFKEEP_YT_ISSUES_MODE", issues)
	t.Setenv("HANDOFFKEEP_DB_URL", protocolTestDB)
	t.Setenv("YOUTRACK_URL", "https://protocol-test.example.test")
	t.Setenv("YOUTRACK_BOT_API_KEY", "fake-protocol-test-key")
}

// exitCodeOf returns the exit code of err, or -1 when err carries none.
func exitCodeOf(err error) int {
	var exit exitCodeError
	if errors.As(err, &exit) {
		return exit.code
	}
	return -1
}

func TestYouTrackLegacyPushRefusedWhileProtocolOwnsWrites(t *testing.T) {
	cases := []struct {
		name     string
		comments string
		issues   string
		want     string
	}{
		{"comments canary", "canary", "", "HANDOFFKEEP_YT_COMMENTS_MODE=canary"},
		{"comments on", "on", "", "HANDOFFKEEP_YT_COMMENTS_MODE=on"},
		{"issues on", "", "on", "HANDOFFKEEP_YT_ISSUES_MODE=on"},
		{"both on", "on", "on", "HANDOFFKEEP_YT_COMMENTS_MODE=on"},
	}
	argvs := map[string][]string{
		"sync":          {"sync"},
		"sync dry-run":  {"sync", "--dry-run"},
		"adopt":         {"adopt", "--state", "/nonexistent/yt_state.json"},
		"adopt dry-run": {"adopt", "--state", "/nonexistent/yt_state.json", "--dry-run"},
	}
	for _, tc := range cases {
		for argName, argv := range argvs {
			t.Run(tc.name+"/"+argName, func(t *testing.T) {
				protocolEnv(t, tc.comments, tc.issues)
				err := youtrackCmd(argv, io.Discard)
				if got := exitCodeOf(err); got != youtrackExitProtocolOwned {
					t.Fatalf("exit %d, want %d (err %v)", got, youtrackExitProtocolOwned, err)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("refusal %q does not name %s", err, tc.want)
				}
				if strings.Contains(err.Error(), "protocol-test") {
					t.Fatalf("refusal carries the database or YouTrack address: %q", err)
				}
			})
		}
	}
}

func TestYouTrackLegacyPushOffReachesTodaysPath(t *testing.T) {
	// Off/off gets past the guard and fails on the config check, which runs
	// before any store call, so no exit code 5 and no connection.
	protocolEnv(t, "", "")
	t.Setenv("HANDOFFKEEP_DB_URL", "")
	if got := exitCodeOf(youtrackCmd([]string{"sync"}, io.Discard)); got != youtrackExitConfig {
		t.Fatalf("sync off/off: exit %d, want config %d", got, youtrackExitConfig)
	}
	if got := exitCodeOf(youtrackCmd([]string{"adopt", "--state", "/nonexistent/yt_state.json"}, io.Discard)); got != youtrackExitConfig {
		t.Fatalf("adopt off/off: exit %d, want config %d (read state)", got, youtrackExitConfig)
	}
	for _, mode := range []string{"off", " off "} {
		protocolEnv(t, mode, mode)
		t.Setenv("HANDOFFKEEP_DB_URL", "")
		if got := exitCodeOf(youtrackCmd([]string{"sync"}, io.Discard)); got == youtrackExitProtocolOwned {
			t.Fatalf("sync with %q modes refused as protocol-owned", mode)
		}
	}
}

func TestYouTrackLegacyPushUnparsableModeFailsClosed(t *testing.T) {
	protocolEnv(t, "On", "")
	err := youtrackCmd([]string{"sync"}, io.Discard)
	if got := exitCodeOf(err); got != youtrackExitConfig || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_COMMENTS_MODE") {
		t.Fatalf("comments mode \"On\": exit %d err %v, want config naming the comments mode", got, err)
	}
	protocolEnv(t, "", "Yes")
	err = youtrackCmd([]string{"adopt", "--state", "/nonexistent/yt_state.json"}, io.Discard)
	if got := exitCodeOf(err); got != youtrackExitConfig || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_ISSUES_MODE") {
		t.Fatalf("issues mode \"Yes\": exit %d err %v, want config naming the issues mode", got, err)
	}
}

func TestYTIssuesModeFromEnv(t *testing.T) {
	for raw, want := range map[string]store.YTIssuesMode{"": store.YTIssuesOff, "off": store.YTIssuesOff, " off ": store.YTIssuesOff, "on": store.YTIssuesOn, " on ": store.YTIssuesOn} {
		t.Setenv("HANDOFFKEEP_YT_ISSUES_MODE", raw)
		if got, err := ytIssuesModeFromEnv(); err != nil || got != want {
			t.Errorf("%q: got %v err %v, want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"ON", "1", "true", "canary"} {
		t.Setenv("HANDOFFKEEP_YT_ISSUES_MODE", raw)
		if _, err := ytIssuesModeFromEnv(); err == nil {
			t.Errorf("%q: want an error", raw)
		}
	}
}

// Read-only subcommands and the write-protocol subcommands keep running while
// the protocol owns writes; the guard is not on them.
func TestYouTrackUnguardedSubcommandsRunWhileProtocolOwnsWrites(t *testing.T) {
	argvs := [][]string{
		{"ops"},
		{"schema", "plan"},
		{"comments-preflight"},
		{"comments-backfill"},
		{"audit"},
		{"audit", "--enqueue"},
		{"canary", "list"},
		{"projects", "list"},
	}
	for _, argv := range argvs {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			protocolEnv(t, "on", "on")
			t.Setenv("HANDOFFKEEP_DB_URL", "")
			err := youtrackCmd(argv, io.Discard)
			if got := exitCodeOf(err); got == youtrackExitProtocolOwned {
				t.Fatalf("%v refused as protocol-owned: %v", argv, err)
			}
		})
	}
}

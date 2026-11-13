package main

import (
	"errors"
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
	for _, name := range []string{"HANDOFFKEEP_YT_COMMENTS_MODE", "HANDOFFKEEP_YT_ISSUES_MODE", "HANDOFFKEEP_YT_FIELDS_MODE", "HANDOFFKEEP_YT_DOC_COMMENTS_MODE", "HANDOFFKEEP_YT_COMMENTS_CATCHUP", "HANDOFFKEEP_YT_READTHROUGH_MODE", "HANDOFFKEEP_DB_URL", "YOUTRACK_URL", "YOUTRACK_BOT_API_KEY"} {
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

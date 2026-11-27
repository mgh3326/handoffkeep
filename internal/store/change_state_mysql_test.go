//go:build mysql

package store

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestChangeChecksChunkedPollFreshnessMySQL(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	f := stateFixture{s: s, ref: ChangeRef{Repo: "example/chunkfreshmysql", PR: 1}, base: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)}
	checkChunkedPollFreshness(t, f)
}

func TestChangeChecksCompleteSetFloorMySQL(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	f := stateFixture{s: s, ref: ChangeRef{Repo: "example/completefloormysql", PR: 1}, base: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)}
	checkCompleteSetFloor(t, f)
}

package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// hk 4122: with every protocol mode off, the legacy push still refuses while
// a kind is canonical in YouTrack, and runs its usual path again once the
// kind is set back to hk.
func TestYouTrackLegacyPushRefusedWhileYouTrackIsCanonical(t *testing.T) {
	_, dbURL := testdb.Schema(t, "yta_push")
	protocolEnv(t, "off", "off")
	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	st, err := store.Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	conn, err := pgx.Connect(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(t.Context()) })
	state := filepath.Join(t.TempDir(), "yt_state.json")
	if err := os.WriteFile(state, []byte(`{"issues":{"1":"HK-1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE yt_authority SET authority='youtrack' WHERE kind='documents'`); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"sync"}, {"sync", "--dry-run"}, {"adopt", "--state", state, "--dry-run"}} {
		err := youtrackCmd(args, io.Discard)
		if got := exitCodeOf(err); got != youtrackExitProtocolOwned || !strings.Contains(err.Error(), "canonical for documents") {
			t.Fatalf("%v: exit %d err %v, want %d naming documents", args, got, err, youtrackExitProtocolOwned)
		}
	}
	if _, err := conn.Exec(t.Context(), `UPDATE yt_authority SET authority='hk' WHERE kind='documents'`); err != nil {
		t.Fatal(err)
	}
	// Back under hk the guard lets the command through.
	st, err = store.Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := youtrackRefuseAuthority(t.Context(), st, "sync"); err != nil {
		t.Fatalf("sync still refused after the rollback to hk: %v", err)
	}
}

func TestYTAuthorityKindsArg(t *testing.T) {
	all, err := ytAuthorityKindsArg("all")
	if err != nil || !slices.Equal(all, store.YTAuthorityKinds) {
		t.Fatalf("all = %v %v", all, err)
	}
	one, err := ytAuthorityKindsArg("comments")
	if err != nil || !slices.Equal(one, []store.YTAuthorityKind{store.YTAuthorityComments}) {
		t.Fatalf("comments = %v %v", one, err)
	}
	if _, err := ytAuthorityKindsArg("files"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	for _, args := range [][]string{
		{"set", "comments", "youtrack", "--note", "n"},
		{"set", "comments", "youtrack", "--by", "operator"},
		{"set", "comments", "both", "--by", "operator", "--note", "n"},
		{"set", "files", "hk", "--by", "operator", "--note", "n"},
		{"show", "extra"},
		{"flip"},
	} {
		if err := youtrackAuthorityCmd(args, io.Discard); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}

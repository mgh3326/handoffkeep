//go:build mysql

package store

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestMySQLCtxSearchFindsCheckpointAndMemory runs scope "ctx" on MySQL: one
// checkpoint and one memory row carry a term no other row holds, and the
// search returns exactly those two, keyed as the PostgreSQL search keys them.
func TestMySQLCtxSearchFindsCheckpointAndMemory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n := strconv.FormatInt(time.Now().UnixNano(), 10)
	term := "ctxneedle" + n
	session := "ctx-mysql-" + n
	agent := "ctx-agent-" + n
	when := time.Now().UTC()
	if _, err := s.db().Exec(ctx, `INSERT INTO checkpoints(session,kind,title,body,refs,created_by,created_at) VALUES(?,'checkpoint',?,'body text',JSON_OBJECT(),'test',?)`, session, "title "+term, when); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db().Exec(ctx, `INSERT INTO memory(agent,name,description,memory_type,content,updated_by,updated_at) VALUES(?,?,'desc','user','content','test',?)`, agent, "name-"+term, when); err != nil {
		t.Fatal(err)
	}

	hits, err := s.SearchCtx(ctx, term, "", 20)
	if err != nil {
		t.Fatalf("ctx search on mysql: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits=%+v, want the checkpoint and the memory row", hits)
	}
	var sawCheckpoint, sawMemory bool
	for _, h := range hits {
		if h.Scope != "ctx" {
			t.Fatalf("scope=%q, want ctx: %+v", h.Scope, h)
		}
		switch h.Title {
		case "title " + term:
			sawCheckpoint = h.Session == session && h.Kind == "checkpoint"
		case "name-" + term:
			sawMemory = h.Key == agent+"/name-"+term && h.Session == agent && h.Kind == "user"
		}
	}
	if !sawCheckpoint || !sawMemory {
		t.Fatalf("hits=%+v: checkpoint ok=%t memory ok=%t", hits, sawCheckpoint, sawMemory)
	}
}

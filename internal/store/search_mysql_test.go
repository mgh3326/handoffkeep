//go:build mysql

package store

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// mysqlSearchHasKey reports whether hits contain scope/key.
func mysqlSearchHasKey(hits []SearchResult, scope, key string) bool {
	for _, h := range hits {
		if h.Scope == scope && h.Key == key {
			return true
		}
	}
	return false
}

// TestMySQLSearchDocsFindsBodyAndExactID runs scopes "docs" and "all" on MySQL.
// The term is written in mixed case and searched in lower case, which only
// matches because the MySQL statement compares case-insensitively, as
// PostgreSQL's ILIKE does. A bare "#<id>" query leads with the exact document,
// and the session filter hides it from another session.
func TestMySQLSearchDocsFindsBodyAndExactID(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n := strconv.FormatInt(time.Now().UnixNano(), 10)
	session := "srch-" + n
	term := "Needle" + n
	doc, _, err := s.PutDocument(ctx, Document{Key: "hk5912/doc-" + n, Kind: "note", Session: session, Body: "a body that holds " + term + " once", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"docs", "all"} {
		hits, err := s.Search(ctx, strings.ToLower(term), scope, session, 20)
		if err != nil {
			t.Fatalf("scope %s on mysql: %v", scope, err)
		}
		if !mysqlSearchHasKey(hits, "docs", doc.Key) {
			t.Fatalf("scope %s: hits=%+v, want %s", scope, hits, doc.Key)
		}
		for _, h := range hits {
			if h.Scope == "docs" && h.Key == doc.Key && !strings.Contains(strings.ToLower(h.Snippet), strings.ToLower(term)) {
				t.Fatalf("scope %s: snippet %q lacks the term", scope, h.Snippet)
			}
		}
	}
	exact, err := s.Search(ctx, "#"+strconv.FormatInt(doc.ID, 10), "all", session, 20)
	if err != nil {
		t.Fatalf("exact id on mysql: %v", err)
	}
	if len(exact) == 0 || exact[0].Key != doc.Key || exact[0].Scope != "docs" {
		t.Fatalf("exact id: hits=%+v, want %s first", exact, doc.Key)
	}
	other, err := s.Search(ctx, "#"+strconv.FormatInt(doc.ID, 10), "all", session+"-other", 20)
	if err != nil {
		t.Fatal(err)
	}
	if mysqlSearchHasKey(other, "docs", doc.Key) {
		t.Fatalf("session filter: hits=%+v, the other session must not see %s", other, doc.Key)
	}
}

// TestMySQLSearchTasksFindsTitleAndCommentsOnce runs scope "tasks" on MySQL. A
// title hit ranks above a comment hit. A task with two matching comments
// returns one row, and an exact id leads. The lane filter applies.
func TestMySQLSearchTasksFindsTitleAndCommentsOnce(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n := strconv.FormatInt(time.Now().UnixNano(), 10)
	lane := "srch-lane-" + n
	term := "Needle" + n
	titled, err := s.CreateTask(ctx, Task{Lane: lane, Title: "title holds " + strings.ToUpper(term), Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	commented, err := s.CreateTask(ctx, Task{Lane: lane, Title: "plain work " + n, Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := s.CreateTaskComment(ctx, commented.ID, "test", "comment "+term+" "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := s.Search(ctx, strings.ToLower(term), "tasks", lane, 20)
	if err != nil {
		t.Fatalf("tasks scope on mysql: %v", err)
	}
	titledKey := strconv.FormatInt(titled.ID, 10)
	commentedKey := strconv.FormatInt(commented.ID, 10)
	if len(hits) != 2 || hits[0].Key != titledKey || hits[1].Key != commentedKey {
		t.Fatalf("hits=%+v, want the title hit then the comment hit, one row each", hits)
	}

	exact, err := s.Search(ctx, "#"+commentedKey, "tasks", lane, 20)
	if err != nil {
		t.Fatalf("exact task id on mysql: %v", err)
	}
	if len(exact) == 0 || exact[0].Key != commentedKey || exact[0].Scope != "tasks" {
		t.Fatalf("exact id: hits=%+v, want %s first", exact, commentedKey)
	}

	laneless, err := s.Search(ctx, strings.ToLower(term), "tasks", lane+"-other", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(laneless) != 0 {
		t.Fatalf("lane filter: hits=%+v, want none in another lane", laneless)
	}
}

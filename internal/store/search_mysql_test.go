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

// openMySQLStoreWithSQLMode opens a second pool on the migrated database at url
// with sql_mode set on every connection, and checks the session really has it.
// Search reads the same tables through either store.
func openMySQLStoreWithSQLMode(t *testing.T, url, mode string) *Store {
	t.Helper()
	ctx := t.Context()
	db, err := openMySQLPool(ctx, url, map[string]string{"sql_mode": "'" + mode + "'"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var got string
	if err := db.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, mode) {
		t.Fatalf("session sql_mode=%q, want %s", got, mode)
	}
	return &Store{mysqlPool: db, dialect: mysqlDialect, queries: sqlQuerier{db}}
}

// assertTaskHits checks that scope "tasks" returned exactly the wanted task ids.
func assertTaskHits(t *testing.T, label string, hits []SearchResult, want ...int64) {
	t.Helper()
	got := map[string]bool{}
	for _, h := range hits {
		if h.Scope == "tasks" {
			got[h.Key] = true
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s: hits=%+v, want %d task(s) %v", label, hits, len(want), want)
	}
	for _, id := range want {
		if key := strconv.FormatInt(id, 10); !got[key] {
			t.Fatalf("%s: hits=%+v, missing task %s", label, hits, key)
		}
	}
}

// TestMySQLSearchTasksWildcardsAreLiteral ports TestSearchTasksWildcardLiteral
// and covers the escape of the MySQL statements. Each query is matched in task
// titles, in their Korean translations and in comments and comment translations,
// once under the default sql_mode and once with NO_BACKSLASH_ESCAPES on every
// connection. The decoy rows differ from the query only where a wildcard or an
// escape would match them, so a leaked wildcard or a doubled backslash shows.
func TestMySQLSearchTasksWildcardsAreLiteral(t *testing.T) {
	ctx := t.Context()
	url := testdb.MySQLDatabase(t)
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	noBackslash := openMySQLStoreWithSQLMode(t, url, "NO_BACKSLASH_ESCAPES")

	n := strconv.FormatInt(time.Now().UnixNano(), 10)
	lane := "srch-lit-" + n
	newTask := func(title string) int64 {
		t.Helper()
		task, err := s.CreateTask(ctx, Task{Lane: lane, Title: title, Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName)})
		if err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	translate := func(entity, entityID, field, original, text string) {
		t.Helper()
		if err := s.PutTranslation(ctx, Translation{Entity: entity, EntityID: entityID, Field: field, Lang: "ko", SourceSHA256: SourceSHA256(original), Text: text, Translator: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	comment := func(taskID int64, body string) TaskComment {
		t.Helper()
		c, err := s.CreateTaskComment(ctx, taskID, "test", body)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// Titles: an underscore, a percent sign and a backslash, each beside a decoy
	// that only a wildcard or a doubled backslash would match.
	underTitle := "under_score task " + n
	newTask("underXscore task " + n)
	under := newTask(underTitle)
	pct := newTask("100% rollout plan " + n)
	newTask("1000 rollout plan " + n)
	slash := newTask(`a\b path ` + n)
	newTask("axb path " + n)

	// Korean translations of titles: the query matches the translated text.
	pctTranslated := newTask("translated pct one " + n)
	translate("task", strconv.FormatInt(pctTranslated, 10), "title", "translated pct one "+n, "번역 100% 계획 "+n)
	translate("task", strconv.FormatInt(newTask("translated pct two "+n), 10), "title", "translated pct two "+n, "번역 1000 계획 "+n)

	// Comments: the task matches through its comment, and through the comment's
	// Korean translation.
	host := newTask("comment host " + n)
	comment(host, "comment under_score note "+n)
	comment(newTask("comment decoy "+n), "comment underXscore note "+n)
	slashHost := newTask("comment slash host " + n)
	c := comment(slashHost, "placeholder comment "+n)
	translate("task_comment", strconv.FormatInt(c.ID, 10), "body", "placeholder comment "+n, `번역 a\b 주석 `+n)

	for _, mode := range []struct {
		name string
		s    *Store
	}{{"default sql_mode", s}, {"NO_BACKSLASH_ESCAPES", noBackslash}} {
		t.Run(mode.name, func(t *testing.T) {
			search := func(q string) []SearchResult {
				t.Helper()
				hits, err := mode.s.Search(ctx, q, "tasks", lane, 20)
				if err != nil {
					t.Fatalf("search %q: %v", q, err)
				}
				return hits
			}
			// "_" is a literal underscore: the title and the comment that hold it,
			// not the decoys with an X where it stands.
			assertTaskHits(t, "_", search("_"), under, host)
			assertTaskHits(t, "under_score", search("under_score"), under, host)
			// "%" is a literal percent sign: the title and its translation.
			assertTaskHits(t, "%", search("%"), pct, pctTranslated)
			assertTaskHits(t, "100%", search("100%"), pct, pctTranslated)
			// A backslash is a literal character, in titles and in translations.
			assertTaskHits(t, `a\b`, search(`a\b`), slash, slashHost)
		})
	}
}

package store

import (
	"strconv"
	"strings"
	"testing"
)

func TestTranslationValidation(t *testing.T) {
	valid := Translation{Entity: "task", EntityID: "1", Field: "title", Lang: "ko", SourceSHA256: SourceSHA256("hello"), Text: "안녕하세요", Translator: "test"}
	for _, tc := range []struct {
		name   string
		reason string
		change func(*Translation)
	}{
		{"entity", "entity", func(x *Translation) { x.Entity = "wrong" }},
		{"field", "field", func(x *Translation) { x.Field = "question" }},
		{"language", "lang", func(x *Translation) { x.Lang = "en" }},
		{"hash", "source_sha256", func(x *Translation) { x.SourceSHA256 = "bad" }},
		{"secret", "secret_like_content", func(x *Translation) { x.Text = "sk-" + strings.Repeat("a", 48) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := valid
			tc.change(&x)
			err := checkTranslation(x)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("error=%v, want reason %q", err, tc.reason)
			}
		})
	}
	if err := checkTranslation(valid); err != nil {
		t.Fatal(err)
	}
}

func TestTranslationEntityIDs(t *testing.T) {
	for _, tc := range []struct {
		entity, id string
		valid      bool
	}{
		{"task", "1", true}, {"task_comment", "27", true},
		{"task", "0", false}, {"task", "-1", false}, {"task", "01", false}, {"task", "dr-1-1", false},
		{"decision_request", "dr-42-1", true}, {"decision_request", "dr-42-2", true},
		{"decision_request", "42", false}, {"decision_request", "dr-0-1", false}, {"decision_request", "dr-1-0", false}, {"decision_request", "dr-1-x", false},
	} {
		if got := validTranslationEntityID(tc.entity, tc.id); got != tc.valid {
			t.Errorf("entity=%q id=%q got=%v", tc.entity, tc.id, got)
		}
	}
}

func TestTranslationsMigrationAndRoundTrip(t *testing.T) {
	s := schemaStore(t, "translations")
	ctx := t.Context()
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE id=$1`, translationsMigrationID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration rows=%d err=%v", count, err)
	}
	if err := s.migrateIfBehind(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE id=$1`, translationsMigrationID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration rows after rerun=%d err=%v", count, err)
	}
	id := seedTaskRow(t, s.pool, "translation-lane", "hello", "backlog")
	x := Translation{Entity: "task", EntityID: strconv.FormatInt(id, 10), Field: "title", Lang: "ko", SourceSHA256: SourceSHA256("hello"), Text: "안녕하세요", Translator: "test"}
	if err := s.PutTranslation(ctx, x); err != nil {
		t.Fatal(err)
	}
	for _, original := range []string{"hello", "changed"} {
		got, err := s.ListTranslations(ctx, "task", []TranslationSource{{EntityID: strconv.FormatInt(id, 10), Field: "title", Original: original}})
		if err != nil || len(got) != 1 || got[0].Text != x.Text || got[0].Stale != (original != "hello") {
			t.Fatalf("original=%q got=%+v err=%v", original, got, err)
		}
	}
	for _, revision := range []string{"dr-1-1", "dr-1-2"} {
		decision := Translation{Entity: "decision_request", EntityID: revision, Field: "question", Lang: "ko", SourceSHA256: SourceSHA256(revision), Text: "질문 " + revision, Translator: "test"}
		if err := s.PutTranslation(ctx, decision); err != nil {
			t.Fatal(err)
		}
	}
	decisions, err := s.ListTranslations(ctx, "decision_request", []TranslationSource{{EntityID: "dr-1-1", Field: "question", Original: "dr-1-1"}, {EntityID: "dr-1-2", Field: "question", Original: "dr-1-2"}})
	if err != nil || len(decisions) != 2 || decisions[0].Text == decisions[1].Text || decisions[0].Stale || decisions[1].Stale {
		t.Fatalf("decision revisions=%+v err=%v", decisions, err)
	}
	got, err := s.Search(ctx, "안녕", "tasks", "", 10)
	if err != nil || len(got) != 1 || !strings.Contains(got[0].Title, "hello") {
		t.Fatalf("search=%+v err=%v", got, err)
	}
}

func TestTranslationTaskBodyLimit(t *testing.T) {
	x := Translation{Entity: "task", EntityID: "1", Field: "body", Lang: "ko", SourceSHA256: SourceSHA256("source"), Text: strings.Repeat("x", MaxBytes+1), Translator: "test"}
	if err := checkTranslation(x); err != nil {
		t.Fatalf("valid long task body translation refused: %v", err)
	}
	x.Field = "title"
	if err := checkTranslation(x); err == nil {
		t.Fatal("oversized title translation accepted")
	}
	x.Field = "body"
	x.Text = strings.Repeat("한", DocumentMaxBytes/3+1)
	if err := checkTranslation(x); err != nil {
		t.Fatalf("expanded Korean body refused: %v", err)
	}
	x.Text = strings.Repeat("x", 3*DocumentMaxBytes+1)
	if err := checkTranslation(x); err == nil {
		t.Fatal("translation larger than expansion limit accepted")
	}
}

func TestMissingTaskTranslationsLongBodiesProgress(t *testing.T) {
	s := schemaStore(t, "translation_long_bodies")
	ctx := t.Context()
	original := strings.Repeat("x", DocumentMaxBytes)
	ids := make([]string, 3)
	for i := range ids {
		id := seedTaskRow(t, s.pool, "long-body-lane", "short title", "backlog")
		ids[i] = strconv.FormatInt(id, 10)
		key := "translation/long-" + ids[i]
		if _, err := s.pool.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at) VALUES($1,'note',$2,$3,'test',now(),now())`, key, original, SourceSHA256(original)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE tasks SET body_doc=$1 WHERE id=$2`, key, id); err != nil {
			t.Fatal(err)
		}
		if err := s.PutTranslation(ctx, Translation{Entity: "task", EntityID: ids[i], Field: "title", Lang: "ko", SourceSHA256: SourceSHA256("short title"), Text: "제목", Translator: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.MissingTaskTranslations(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].EntityID != ids[0] || first[1].EntityID != ids[1] {
		t.Fatalf("first page count=%d, want first two body translations", len(first))
	}
	for _, row := range first {
		if err := s.PutTranslation(ctx, Translation{Entity: "task", EntityID: row.EntityID, Field: "body", Lang: "ko", SourceSHA256: row.SourceSHA256, Text: original, Translator: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	next, err := s.MissingTaskTranslations(ctx, 1000)
	if err != nil || len(next) != 1 || next[0].EntityID != ids[2] {
		t.Fatalf("next page count=%d err=%v", len(next), err)
	}
}

func TestCommentTranslationMayExpandBeyondSourceLimit(t *testing.T) {
	row := Translation{Entity: "task_comment", EntityID: "1", Field: "body", Lang: "ko", SourceSHA256: SourceSHA256("source"), Text: strings.Repeat("한", MaxBytes/3+1), Translator: "test"}
	if err := checkTranslation(row); err != nil {
		t.Fatal(err)
	}
	row.Text = strings.Repeat("x", DocumentMaxBytes+1)
	if err := checkTranslation(row); err == nil {
		t.Fatal("oversized translation accepted")
	}
}

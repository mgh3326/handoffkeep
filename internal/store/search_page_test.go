package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSearchPageOrdersScopesAndMarksTruncatedTies(t *testing.T) {
	s := schemaStore(t, "search_page")
	ctx := context.Background()
	createdAt := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, title := range []string{"alpha ctxorderprobe", "beta ctxorderprobe"} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO checkpoints(session,kind,title,body,refs,created_by,created_at) VALUES('lane-a','checkpoint',$1,'ctxorderprobe body','{}','test',$2)`, title, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"a-doc-orderprobe", "z-doc-orderprobe"} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at) VALUES($1,'note','lane-a','', 'docorderprobe body','sha','test',$2,$2)`, key, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"alpha memoryorderprobe", "beta memoryorderprobe", "gamma memoryorderprobe"} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO memory(agent,name,description,memory_type,content,updated_by,updated_at) VALUES('lane-a',$1,'','reference','memoryorderprobe body','test',$2)`, name, createdAt); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Search(ctx, "orderprobe", "all", "", 6)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ctx:alpha ctxorderprobe", "ctx:beta ctxorderprobe",
		"docs:a-doc-orderprobe", "docs:z-doc-orderprobe",
		"memory:alpha memoryorderprobe", "memory:beta memoryorderprobe",
	}
	keys := make([]string, 0, len(got))
	for _, row := range got {
		keys = append(keys, row.Scope+":"+row.Title)
		if !row.CreatedAt.Equal(createdAt) {
			t.Errorf("%s created_at=%s, want %s", row.Title, row.CreatedAt, createdAt)
		}
		if !row.Truncated {
			t.Errorf("%s: truncated page row was not marked", row.Title)
		}
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("ordered page=%v, want %v", keys, want)
	}
}

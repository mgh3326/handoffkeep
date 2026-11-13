package youtrack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// StateChanges asks the activity log for custom field changes since the
// bound, pages through it, and keeps only entries into a value of the state
// field: other fields, a cleared field and untargeted rows are left out, and
// a value that maps to no hk state keeps its raw name.
func TestStateChangesReadsOnlyStateFieldChanges(t *testing.T) {
	at := time.Date(2026, 11, 12, 1, 2, 3, 0, time.UTC)
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
		if skip == 0 {
			// A full first page, mostly other fields, so the client asks again.
			var rows []string
			for i := 0; i < stateChangePageSize-3; i++ {
				rows = append(rows, `{"timestamp":1,"field":{"name":"hk PR"},"added":[{"name":"x"}],"target":{"id":"2-9"}}`)
			}
			rows = append(rows,
				`{"timestamp":2,"field":{"name":"hk PR"},"added":["pull/42"],"target":{"id":"2-8"}}`,
				`{"timestamp":3,"field":{"name":"hk 우선순위"},"added":[3],"target":{"id":"2-7"}}`,
				fmt.Sprintf(`{"timestamp":%d,"field":{"name":"hk 상태"},"added":[{"name":"개발 중"}],"removed":[{"name":"리뷰 중"}],"target":{"id":"2-1"}}`, at.UnixMilli()),
				`{"timestamp":5,"field":{"name":"hk 상태"},"added":[],"target":{"id":"2-2"}}`,
				`{"timestamp":6,"field":{"name":"hk 상태"},"added":[{"name":"개발 중"}]}`)
			_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`[{"timestamp":%d,"field":{"name":"hk 상태"},"added":[{"name":"Custom"}],"target":{"id":"2-3"}}]`, at.Add(time.Hour).UnixMilli())))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "token", Sleep: func(_ context.Context, _ time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	since := at.Add(-24 * time.Hour)
	got, err := c.StateChanges(context.Background(), DefaultSchema(), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("changes=%+v", got)
	}
	if got[0].IssueID != "2-1" || got[0].State != "developing" || !got[0].StateKnown || !got[0].At.Equal(at) {
		t.Fatalf("first=%+v", got[0])
	}
	if got[1].IssueID != "2-3" || got[1].State != "Custom" || got[1].StateKnown {
		t.Fatalf("second=%+v", got[1])
	}
	if len(queries) != 2 {
		t.Fatalf("pages=%d", len(queries))
	}
	for _, want := range []string{"categories=CustomFieldCategory", "start=" + strconv.FormatInt(since.UnixMilli(), 10), "%24skip=0", "%24top=500"} {
		if !strings.Contains(queries[0], want) {
			t.Fatalf("query %q is missing %q", queries[0], want)
		}
	}
	if !strings.Contains(queries[1], "%24skip=500") {
		t.Fatalf("second page query %q", queries[1])
	}
}

func TestStateChangesFailsOnAServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "token", MaxRetries: -1, Sleep: func(_ context.Context, _ time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.StateChanges(context.Background(), DefaultSchema(), time.Now()); err == nil {
		t.Fatal("a refused read must fail, not return an empty history")
	}
}

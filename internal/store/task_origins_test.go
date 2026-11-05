package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

const (
	originUUID  = "3f2b8c1e-9a4d-4e6f-8b7a-1c2d3e4f5a6b"
	originUUID2 = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
)

func validOrigin() TaskOriginInput {
	return TaskOriginInput{Provider: "linear", Namespace: "example-ws", ExternalID: originUUID, DisplayRef: "MGH-151", URL: "https://linear.app/example-ws/issue/MGH-151", Title: "an issue"}
}

// The validator reads no database.
func TestTaskOriginValidate(t *testing.T) {
	good := validOrigin()
	if err := checkTaskOriginInput(&good); err != nil {
		t.Fatalf("valid origin refused: %v", err)
	}
	bare := TaskOriginInput{Provider: "github", Namespace: "mgh3326/handoffkeep", ExternalID: "PR_kwDOabc123"}
	if err := checkTaskOriginInput(&bare); err != nil {
		t.Fatalf("github origin without display_ref, url and title refused: %v", err)
	}
	refused := map[string]func(*TaskOriginInput){
		"non-UUID linear external_id": func(o *TaskOriginInput) { o.ExternalID = "MGH-151" },
		"uppercase UUID":              func(o *TaskOriginInput) { o.ExternalID = strings.ToUpper(originUUID) },
		"braced UUID":                 func(o *TaskOriginInput) { o.ExternalID = "{" + originUUID + "}" },
		"unknown provider":            func(o *TaskOriginInput) { o.Provider = "jira" },
		"empty provider":              func(o *TaskOriginInput) { o.Provider = "" },
		"bad namespace":               func(o *TaskOriginInput) { o.Namespace = "has space" },
		"empty namespace":             func(o *TaskOriginInput) { o.Namespace = "" },
		"long namespace":              func(o *TaskOriginInput) { o.Namespace = strings.Repeat("a", 129) },
		"http url":                    func(o *TaskOriginInput) { o.URL = "http://linear.app/x" },
		"url without host":            func(o *TaskOriginInput) { o.URL = "https:///x" },
		"long url":                    func(o *TaskOriginInput) { o.URL = "https://linear.app/" + strings.Repeat("a", 512) },
		"65-byte display_ref":         func(o *TaskOriginInput) { o.DisplayRef = strings.Repeat("a", 65) },
		"secret in title":             func(o *TaskOriginInput) { o.Title = "key sk-ant-api03-" + strings.Repeat("a", 40) },
		"NUL in title":                func(o *TaskOriginInput) { o.Title = "a\x00b" },
		"github id with space":        func(o *TaskOriginInput) { o.Provider, o.ExternalID = "github", "a b" },
		"github id with newline":      func(o *TaskOriginInput) { o.Provider, o.ExternalID = "github", "a\nb" },
		"github empty id":             func(o *TaskOriginInput) { o.Provider, o.ExternalID = "github", "" },
		"github 257-byte id":          func(o *TaskOriginInput) { o.Provider, o.ExternalID = "sentry", strings.Repeat("a", 257) },
	}
	for name, mutate := range refused {
		o := validOrigin()
		mutate(&o)
		if err := checkTaskOriginInput(&o); err == nil {
			t.Errorf("%s: accepted %+v", name, o)
		}
	}
	secret := validOrigin()
	secret.Title = "key sk-ant-api03-" + strings.Repeat("a", 40)
	if err := checkTaskOriginInput(&secret); err == nil {
		t.Fatal("secret-like title accepted")
	} else if _, ok := guard.SecretPattern(err); !ok {
		t.Fatalf("secret refusal is not a guard.SecretError: %v", err)
	}
	edge := validOrigin()
	edge.DisplayRef = strings.Repeat("a", 64)
	if err := checkTaskOriginInput(&edge); err != nil {
		t.Fatalf("64-byte display_ref refused: %v", err)
	}
	long := validOrigin()
	long.Title = strings.Repeat("가", 300)
	if err := checkTaskOriginInput(&long); err != nil {
		t.Fatalf("300-rune title refused: %v", err)
	}
	if got := len([]rune(long.Title)); got != 200 {
		t.Fatalf("title cut to %d runes, want 200", got)
	}
}

func TestTaskOriginUUIDIsTheKey(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	a := seedTaskRow(t, pool, "origin-test", "task a", "backlog")
	b := seedTaskRow(t, pool, "origin-test", "task b", "backlog")

	first, created, err := s.AddTaskOrigin(ctx, a, "tester", validOrigin())
	if err != nil || !created || first.DisplayRef != "MGH-151" || first.TaskID != a {
		t.Fatalf("first add: %+v created=%t err=%v", first, created, err)
	}
	moved := validOrigin()
	moved.DisplayRef = "ROB-1514"
	moved.URL = "https://linear.app/example-ws/issue/ROB-1514"
	second, created, err := s.AddTaskOrigin(ctx, a, "tester", moved)
	if err != nil || created {
		t.Fatalf("re-add under a new number: created=%t err=%v", created, err)
	}
	if second.ID != first.ID || second.DisplayRef != "ROB-1514" || second.URL != moved.URL || !second.SnapshotAt.After(first.SnapshotAt) {
		t.Fatalf("re-add: first=%+v second=%+v", first, second)
	}
	// The same input again changes nothing, and a field left out keeps its value.
	same, created, err := s.AddTaskOrigin(ctx, a, "tester", moved)
	if err != nil || created || !same.SnapshotAt.Equal(second.SnapshotAt) {
		t.Fatalf("identical re-add: %+v created=%t err=%v", same, created, err)
	}
	sparse, created, err := s.AddTaskOrigin(ctx, a, "tester", TaskOriginInput{Provider: "linear", Namespace: "example-ws", ExternalID: originUUID})
	if err != nil || created || sparse.DisplayRef != "ROB-1514" || sparse.URL != moved.URL {
		t.Fatalf("sparse re-add: %+v created=%t err=%v", sparse, created, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_origins WHERE task_id=$1`, a).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v, want one", rows, err)
	}

	found, err := s.FindTaskOrigins(ctx, "linear", originUUID, 10)
	if err != nil || len(found) != 1 || found[0].TaskID != a || found[0].DisplayRef != "ROB-1514" {
		t.Fatalf("find: %+v err=%v", found, err)
	}

	// The same UUID on a second task is a second row, found with the first.
	if _, created, err = s.AddTaskOrigin(ctx, b, "tester", validOrigin()); err != nil || !created {
		t.Fatalf("second task: created=%t err=%v", created, err)
	}
	found, err = s.FindTaskOrigins(ctx, "linear", originUUID, 10)
	if err != nil || len(found) != 2 || found[0].TaskID != a || found[1].TaskID != b {
		t.Fatalf("find across tasks: %+v err=%v", found, err)
	}
	if other, err := s.FindTaskOrigins(ctx, "linear", originUUID2, 10); err != nil || len(other) != 0 {
		t.Fatalf("find of an unknown UUID: %+v err=%v", other, err)
	}

	// Another UUID on the same task: two origins, listed in id order.
	if _, _, err = s.AddTaskOrigin(ctx, a, "tester", TaskOriginInput{Provider: "github", Namespace: "mgh3326/handoffkeep", ExternalID: "I_kwDOabc"}); err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListTaskOrigins(ctx, a)
	if err != nil || len(listed) != 2 || listed[0].ID != first.ID || listed[1].Provider != "github" {
		t.Fatalf("list: %+v err=%v", listed, err)
	}

	if _, _, err = s.AddTaskOrigin(ctx, 999999999, "tester", validOrigin()); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: err=%v, want ErrTaskNotFound", err)
	}
	if _, err = s.ListTaskOrigins(ctx, 999999999); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("list of a missing task: err=%v, want ErrTaskNotFound", err)
	}
	bad := validOrigin()
	bad.ExternalID = "MGH-151"
	if _, _, err = s.AddTaskOrigin(ctx, a, "tester", bad); err == nil || errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("display number as external_id: err=%v", err)
	}
}

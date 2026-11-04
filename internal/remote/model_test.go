package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestModelMethods(t *testing.T) {
	var method, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if path == "/v1/labels" && method == "GET" {
			fmt.Fprint(w, `{"labels":[]}`)
		} else {
			fmt.Fprint(w, `{}`)
		}
	}))
	defer server.Close()
	c := Client{URL: server.URL, Token: "test", HTTP: server.Client()}
	ctx := context.Background()
	mid := int64(2)
	cases := []struct {
		wantMethod, wantPath string
		call                 func() error
	}{
		{"GET", "/v1/tasks/projects/p/metadata", func() error { _, e := c.GetProjectMetadata(ctx, "p"); return e }},
		{"PUT", "/v1/tasks/projects/p/metadata", func() error { _, e := c.SetProjectMetadata(ctx, store.ProjectMetadata{Name: "p"}); return e }},
		{"PATCH", "/v1/tasks/projects/p/metadata", func() error { _, e := c.PatchProjectMetadata(ctx, "p", store.ProjectMetadataPatch{}); return e }},
		{"POST", "/v1/milestones", func() error { _, e := c.SaveMilestone(ctx, store.ProjectMilestone{}); return e }},
		{"GET", "/v1/milestones/2", func() error { _, e := c.GetMilestone(ctx, 2); return e }},
		{"PATCH", "/v1/milestones/2", func() error { _, e := c.PatchMilestone(ctx, 2, store.MilestonePatch{}); return e }},
		{"PUT", "/v1/milestones/2", func() error { _, e := c.SaveMilestone(ctx, store.ProjectMilestone{ID: 2}); return e }},
		{"GET", "/v1/labels", func() error { _, e := c.ListLabels(ctx); return e }},
		{"POST", "/v1/labels", func() error { _, e := c.SaveLabel(ctx, store.Label{}); return e }},
		{"PUT", "/v1/labels/2", func() error { _, e := c.SaveLabel(ctx, store.Label{ID: 2}); return e }},
		{"POST", "/v1/labels/2/retire", func() error { return c.RetireLabel(ctx, 2) }},
		{"POST", "/v1/tasks/1/milestone", func() error { _, e := c.SetTaskMilestone(ctx, 1, &mid); return e }},
		{"POST", "/v1/tasks/1/labels", func() error { _, e := c.SetTaskLabel(ctx, 1, 2, true); return e }},
		{"POST", "/v1/tasks/1/deadlines", func() error { _, e := c.SetTaskDeadlines(ctx, 1, store.TaskDeadlines{}); return e }},
		{"POST", "/v1/tasks/1/due", func() error { _, e := c.SetTaskDue(ctx, 1, nil); return e }},
		{"POST", "/v1/tasks/1/project", func() error {
			_, e := c.SetTaskProjectWithMetadata(ctx, 1, "p", "", store.ProjectMetadataPatch{})
			return e
		}},
		{"POST", "/v1/tasks/1/subscribers", func() error { _, e := c.SetTaskSubscriber(ctx, 1, "a", "cli", true); return e }},
	}
	for _, tc := range cases {
		t.Run(tc.wantMethod+tc.wantPath, func(t *testing.T) {
			method, path = "", ""
			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
			if method != tc.wantMethod || path != tc.wantPath {
				t.Fatalf("got %s %s", method, path)
			}
		})
	}
}

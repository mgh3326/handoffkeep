package ui

import "testing"

func TestRoutePatternsHidePathValues(t *testing.T) {
	for path, want := range map[string]string{
		"/ui/queue":                               "/ui/queue",
		"/ui/work":                                "/ui/work",
		"/ui/tasks/123":                           "/ui/tasks/{id}",
		"/ui/tasks/123/comments":                  "/ui/tasks/{id}/comments",
		"/ui/doc/private-key/note":                "/ui/doc/{key...}",
		"/ui/api/board/tasks/123":                 "/ui/api/board/tasks/{id}",
		"/ui/api/board/tasks/123/comments":        "/ui/api/board/tasks/{id}/comments",
		"/ui/api/nodes/private-machine/accepting": "/ui/api/nodes/{machine}/accepting",
		"/ui/static/console/queue.js":             "/ui/static/{file...}",
		"/ui/unknown-private-path":                "/ui/{path...}",
	} {
		if got := routePattern(path); got != want {
			t.Errorf("routePattern(%q)=%q want %q", path, got, want)
		}
	}
}

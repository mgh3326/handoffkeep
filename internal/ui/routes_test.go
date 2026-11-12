package ui

import "testing"

func TestRoutePatternsHidePathValues(t *testing.T) {
	for path, want := range map[string]string{
		"/ui/queue":                               "/ui/queue",
		"/ui/api/board/projects":                  "/ui/api/board/projects",
		"/ui/work":                                "/ui/work",
		"/ui/work-status":                         "/ui/work-status",
		"/ui/api/work-status":                     "/ui/api/work-status",
		"/ui/tasks/123":                           "/ui/tasks/{id}",
		"/ui/tasks/123/comments":                  "/ui/tasks/{id}/comments",
		"/ui/doc/private-key/note":                "/ui/doc/{key...}",
		"/ui/api/board/tasks/123":                 "/ui/api/board/tasks/{id}",
		"/ui/api/board/tasks/123/comments":        "/ui/api/board/tasks/{id}/comments",
		"/ui/api/nodes/private-machine/accepting": "/ui/api/nodes/{machine}/accepting",
		"/ui/api/decision-requests/resolve":       "/ui/api/decision-requests/resolve",
		"/ui/static/console/queue.js":             "/ui/static/{file...}",
		"/ui/unknown-private-path":                "/ui/{path...}",
	} {
		if got := routePattern(path); got != want {
			t.Errorf("routePattern(%q)=%q want %q", path, got, want)
		}
	}
}

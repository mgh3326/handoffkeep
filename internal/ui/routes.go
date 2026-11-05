package ui

import "strings"

// routePattern supplies static telemetry names for the console's manual
// dispatcher, including auth failures. Never fall back to an unknown raw path.
func routePattern(path string) string {
	switch path {
	case "/ui", "/ui/timeline", "/ui/queue", "/ui/decisions", "/ui/compose",
		"/ui/work", "/ui/fleet", "/ui/grades", "/ui/deploys", "/ui/docs", "/ui/home", "/ui/events",
		"/ui/dispositions/answer", "/ui/dispositions/accept-batch", "/ui/dispositions/renotify",
		"/ui/decisions/answer", "/ui/decisions/answer-batch",
		"/ui/fragments/timeline", "/ui/fragments/decisions", "/ui/fragments/fleet", "/ui/fragments/compose",
		"/ui/api/fleet", "/ui/api/live", "/ui/api/reap", "/ui/api/glance", "/ui/api/board/tasks",
		"/ui/api/search", "/ui/api/policy/active", "/ui/api/board/doc", "/ui/api/docs",
		"/ui/api/docs/prefixes", "/ui/api/dashboard", "/ui/api/bench/catalog", "/ui/api/deploy-pending":
		return path // only literal, exact matches can reach here
	}
	switch {
	case taskCommentsPathRE.MatchString(path):
		return "/ui/tasks/{id}/comments"
	case boardCommentsPathRE.MatchString(path):
		return "/ui/api/board/tasks/{id}/comments"
	case strings.HasPrefix(path, "/ui/api/board/tasks/"):
		return "/ui/api/board/tasks/{id}"
	case strings.HasPrefix(path, "/ui/api/nodes/"):
		return "/ui/api/nodes/{machine}/accepting"
	case strings.HasPrefix(path, "/ui/doc/"):
		return "/ui/doc/{key...}"
	case strings.HasPrefix(path, "/ui/tasks/"):
		return "/ui/tasks/{id}"
	case strings.HasPrefix(path, "/ui/static/"):
		return "/ui/static/{file...}"
	default:
		return "/ui/{path...}"
	}
}

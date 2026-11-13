package ui

import "strings"

// routePattern supplies static telemetry names for the console's manual
// dispatcher, including auth failures. Never fall back to an unknown raw path.
func routePattern(path string) string {
	switch path {
	case "/ui", "/ui/queue", "/ui/decisions", "/ui/work", "/ui/deploys", "/ui/home",
		"/ui/work-status",
		"/ui/api/fleet", "/ui/api/glance", "/ui/api/board/tasks",
		"/ui/api/attention", "/ui/api/decisions", "/ui/api/board/projects", "/ui/api/board/lanes",
		"/ui/api/board/doc", "/ui/api/deploy-pending",
		"/ui/api/decision-requests/resolve", "/ui/api/decision-requests/deny",
		"/ui/api/inbox/state", "/ui/api/inbox/undo", "/ui/api/work-status":
		return path // only literal, exact matches can reach here
	}
	switch {
	case taskPriorityPathRE.MatchString(path):
		return "/ui/api/tasks/{id}/priority"
	case taskLifecyclePathRE.MatchString(path):
		return lifecycleRouteName(path)
	case taskCommentsPathRE.MatchString(path):
		return "/ui/tasks/{id}/comments"
	case boardCommentsPathRE.MatchString(path):
		return "/ui/api/board/tasks/{id}/comments"
	case strings.HasPrefix(path, "/ui/api/decisions/"):
		return "/ui/api/decisions/{id}"
	case strings.HasPrefix(path, "/ui/api/board/tasks/"):
		return "/ui/api/board/tasks/{id}"
	case strings.HasPrefix(path, "/ui/api/nodes/"):
		return "/ui/api/nodes/{machine}/accepting"
	case strings.HasPrefix(path, "/ui/doc/"):
		return "/ui/doc/{key...}"
	case strings.HasPrefix(path, "/ui/tasks/"):
		return "/ui/tasks/{id}"
	case strings.HasPrefix(path, "/ui/decisions/"):
		return "/ui/decisions/{id}"
	case strings.HasPrefix(path, "/ui/static/"):
		return "/ui/static/{file...}"
	default:
		return "/ui/{path...}"
	}
}

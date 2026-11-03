package telemetry

import (
	"strconv"

	"github.com/getsentry/sentry-go"
)

// Scrub list for both errors and transactions: request (all headers, cookies,
// URL/query, body), user, breadcrumbs, hostname, arbitrary messages, extras,
// tags, contexts, exception values and frame locals/source. Only our generated
// request id, the API error code, HTTP metadata, trace ids/status and
// normalized DB spans survive; 5xx events get a route+status fingerprint.
func scrubEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	event.Request = nil
	event.User = sentry.User{}
	event.Breadcrumbs = nil
	event.ServerName = ""
	event.Message = ""
	event.Fingerprint, event.Threads, event.Attachments = nil, nil, nil
	event.Logger, event.DebugMeta = "", nil
	event.Extra = onlyKeys(event.Extra, "http.request.method", "http.response.status_code")
	tags := map[string]string{"request_id": event.Tags["request_id"]}
	if code := event.Tags["error_code"]; validErrorCode(code) {
		tags["error_code"] = code
	}
	event.Tags = tags
	trace := event.Contexts["trace"]
	event.Contexts = map[string]sentry.Context{
		"trace": onlyKeys(trace, "trace_id", "span_id", "parent_span_id", "op", "status", "origin"),
	}
	for i := range event.Exception {
		exception := &event.Exception[i]
		switch exception.Type {
		case "HTTPError":
			exception.Value = "HTTP handler returned 5xx"
		case "panic":
			exception.Value = "HTTP handler panicked"
		default:
			exception.Type, exception.Value = "error", "[filtered]"
		}
		if stack := exception.Stacktrace; stack != nil {
			for i := range stack.Frames {
				frame := &stack.Frames[i]
				frame.Vars = nil
				frame.PreContext, frame.PostContext, frame.ContextLine = nil, nil, ""
				frame.AbsPath = ""
			}
		}
	}
	// A 5xx has no stack and fixed exception text, so Sentry's default
	// grouping would merge every route into one issue. Group by the route
	// template and status instead; both are content-free. Panics keep their
	// stack-based grouping.
	if len(event.Exception) == 1 && event.Exception[0].Type == "HTTPError" {
		if status, ok := event.Extra["http.response.status_code"].(int); ok {
			event.Fingerprint = []string{"http-5xx", event.Transaction, strconv.Itoa(status)}
		}
	}
	for _, span := range event.Spans {
		span.Tags = nil
		if span.Op != "db.sql.query" {
			span.Description, span.Data = "", nil
			continue
		}
		span.Description = normalizeSQL(span.Description)
		span.Data = onlyKeys(span.Data, "db.system", "db.rows_affected")
	}
	return event
}

// validErrorCode accepts the API's closed error vocabulary shape: a short
// lowercase snake_case code such as internal_error. It never matches free
// text, so a code tag cannot carry request content.
func validErrorCode(code string) bool {
	if code == "" || len(code) > 64 || code[0] < 'a' || code[0] > 'z' {
		return false
	}
	for _, c := range code {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func onlyKeys(values map[string]any, keys ...string) map[string]any {
	result := make(map[string]any)
	for _, key := range keys {
		if value, ok := values[key]; ok {
			result[key] = value
		}
	}
	return result
}

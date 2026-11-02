package telemetry

import "github.com/getsentry/sentry-go"

// Scrub list for both errors and transactions: request (all headers, cookies,
// URL/query, body), user, breadcrumbs, hostname, arbitrary messages, extras,
// tags, contexts, exception values and frame locals/source. Only our generated
// request id, HTTP metadata, trace ids/status and normalized DB spans survive.
func scrubEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	event.Request = nil
	event.User = sentry.User{}
	event.Breadcrumbs = nil
	event.ServerName = ""
	event.Message = ""
	event.Fingerprint, event.Threads, event.Attachments = nil, nil, nil
	event.Logger, event.DebugMeta = "", nil
	event.Extra = onlyKeys(event.Extra, "http.request.method", "http.response.status_code")
	event.Tags = map[string]string{"request_id": event.Tags["request_id"]}
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

func onlyKeys(values map[string]any, keys ...string) map[string]any {
	result := make(map[string]any)
	for _, key := range keys {
		if value, ok := values[key]; ok {
			result[key] = value
		}
	}
	return result
}

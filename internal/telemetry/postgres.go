package telemetry

import (
	"context"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/getsentry/sentry-go"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// QueryTracer traces Query, QueryRow and Exec, including calls on transactions.
// A query becomes a Sentry span under a sampled Sentry transaction and an OTLP
// span under a recording OTLP server span. It never reads Args, connection
// configuration, returned rows, or error text. Every statement that runs under
// a context from WithQueryCount is counted, sampled or not.
//
// Each span carries a query label (Label, or the verb and first table of the
// statement). The label is what names a span when its text is filtered out.
type QueryTracer struct{}

// querySpans carries the spans started by TraceQueryStart to TraceQueryEnd.
// Either field may be nil.
type querySpans struct {
	sentry *sentry.Span
	otel   trace.Span
}

type querySpanKey struct{}

type queryCountKey struct{}

type queryLabelKey struct{}

var _ pgx.QueryTracer = QueryTracer{}

// QueryCount counts the statements that ran under one request context.
type QueryCount struct{ n atomic.Int64 }

// WithQueryCount returns ctx with a fresh QueryCount. Statements run with the
// returned context, or with any context derived from it, add to the count.
func WithQueryCount(ctx context.Context) (context.Context, *QueryCount) {
	count := &QueryCount{}
	return context.WithValue(ctx, queryCountKey{}, count), count
}

// Count reports how many statements have run so far.
func (c *QueryCount) Count() int { return int(c.n.Load()) }

// labelPattern is the only shape a Label name may take: lower-case words,
// digits, underscores and dots, so a name cannot carry request content.
var labelPattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// Label returns ctx naming the next statement run with it, for example
// tx.Query(telemetry.Label(ctx, "task_drift.deploy_records"), sql). A name that
// does not match labelPattern is ignored, so an unlabelled statement keeps its
// shape label.
func Label(ctx context.Context, name string) context.Context {
	if !labelPattern.MatchString(name) {
		return ctx
	}
	return context.WithValue(ctx, queryLabelKey{}, name)
}

func (QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if count, ok := ctx.Value(queryCountKey{}).(*QueryCount); ok {
		count.n.Add(1)
	}
	sentryParent := sentry.SpanFromContext(ctx)
	otelParent := trace.SpanFromContext(ctx)
	sentryOn := sentryParent != nil && sentryParent.Sampled == sentry.SampledTrue
	otelOn := otelParent.IsRecording()
	if !sentryOn && !otelOn {
		return ctx
	}
	statement := normalizeSQL(data.SQL)
	label := queryLabel(ctx, data.SQL)
	var spans querySpans
	if sentryOn {
		span := sentry.StartSpan(ctx, "db.sql.query", sentry.WithDescription(label))
		span.SetData("db.system", "postgresql")
		spans.sentry = span
		ctx = span.Context()
	}
	if otelOn {
		var span trace.Span
		ctx, span = otelParent.TracerProvider().Tracer(otlpTracerName).Start(ctx, "db.sql.query",
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("db.system", "postgresql"),
				attribute.String("db.query.label", label),
				attribute.String("db.query.text", statement),
			),
		)
		spans.otel = span
	}
	return context.WithValue(ctx, querySpanKey{}, spans)
}

// queryLabel is the explicit Label when one is set, else the shape label.
func queryLabel(ctx context.Context, sql string) string {
	if name, ok := ctx.Value(queryLabelKey{}).(string); ok {
		return name
	}
	return shapeLabel(sql)
}

var (
	leadingVerb   = regexp.MustCompile(`^\s*([A-Za-z]+)`)
	firstRelation = regexp.MustCompile(`(?i)\b(?:from|into|update|join)\s+"?([a-z_][a-z0-9_]*)"?(?:[\s(),;]|$)`)
)

// shapeLabel names a statement by its verb and first table, such as "select
// tasks". Only a plain identifier that ends at a delimiter after FROM, INTO,
// UPDATE or JOIN is kept: a quoted identifier that holds more than that (a
// hyphen, a space) yields no table, so no part of it reaches a label. A
// statement without a table yields its verb alone, or "statement".
func shapeLabel(sql string) string {
	verb := "statement"
	if m := leadingVerb.FindStringSubmatch(sql); m != nil {
		verb = strings.ToLower(m[1])
	}
	if m := firstRelation.FindStringSubmatch(sql); m != nil {
		return verb + " " + strings.ToLower(m[1])
	}
	return verb
}

func (QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	spans, ok := ctx.Value(querySpanKey{}).(querySpans)
	if !ok {
		return
	}
	if span := spans.sentry; span != nil {
		span.SetData("db.rows_affected", data.CommandTag.RowsAffected())
		span.Status = sentry.SpanStatusOK
		if data.Err != nil {
			span.Status = sentry.SpanStatusInternalError
		}
		span.Finish()
	}
	if span := spans.otel; span != nil {
		span.SetAttributes(attribute.Int64("db.rows_affected", data.CommandTag.RowsAffected()))
		if data.Err != nil {
			// The error text can carry request content, so only the status is kept.
			span.SetStatus(codes.Error, "")
		}
		span.End()
	}
}

var numberLiteral = regexp.MustCompile(`(?i)^(?:0x[0-9a-f_]+|0o[0-7_]+|0b[01_]+|(?:[0-9][0-9_]*(?:\.[0-9_]*)?|\.[0-9][0-9_]*)(?:e[+-]?[0-9_]+)?)`)

// normalizeSQL removes values even from SQL that didn't use bind parameters.
// Quoted identifiers are also removed: dynamic identifiers can contain content.
// Malformed or oversized input is discarded instead of risking partial scrubs.
func normalizeSQL(sql string) string {
	if len(sql) > 16<<10 {
		return "[filtered statement]"
	}
	var out strings.Builder
	for i := 0; i < len(sql); {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexAny(sql[i:], "\r\n")
			if end < 0 {
				i = len(sql)
			} else {
				i += end
			}
			out.WriteByte(' ')
		case strings.HasPrefix(sql[i:], "/*"):
			i += 2
			depth := 1
			for i < len(sql) && depth > 0 {
				switch {
				case strings.HasPrefix(sql[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(sql[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			if depth != 0 {
				return "[filtered statement]"
			}
			out.WriteByte(' ')
		case sql[i] == '\'' || sql[i] == '"':
			quote := sql[i]
			// Backslash escapes a quote only inside E'...' strings. In any other
			// string its meaning depends on the session's
			// standard_conforming_strings and on U&'...' escapes, so a backslash
			// there filters the whole statement rather than guess a boundary.
			escapes := quote == '\'' && i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && (i < 2 || !identByte(sql[i-2]))
			i++
			closed := false
			for i < len(sql) {
				if sql[i] == '\\' && !escapes {
					return "[filtered statement]"
				}
				if sql[i] == '\\' && escapes {
					i += 2
				} else if sql[i] == quote {
					i++
					if i < len(sql) && sql[i] == quote {
						i++
						continue
					}
					closed = true
					break
				} else {
					i++
				}
			}
			if !closed {
				return "[filtered statement]"
			}
			out.WriteByte('?')
		case sql[i] == '$':
			end := i + 1
			if end < len(sql) && sql[end] >= '0' && sql[end] <= '9' {
				for end < len(sql) && sql[end] >= '0' && sql[end] <= '9' {
					end++
				}
			} else {
				for end < len(sql) && (sql[end] == '_' || sql[end] >= 'a' && sql[end] <= 'z' || sql[end] >= 'A' && sql[end] <= 'Z' || end > i+1 && sql[end] >= '0' && sql[end] <= '9') {
					end++
				}
				if end >= len(sql) || sql[end] != '$' {
					return "[filtered statement]"
				}
				delimiter := sql[i : end+1]
				close := strings.Index(sql[end+1:], delimiter)
				if close < 0 {
					return "[filtered statement]"
				}
				end += 1 + close + len(delimiter)
			}
			i = end
			out.WriteByte('?')
		case sql[i] >= '0' && sql[i] <= '9' || sql[i] == '.' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9':
			i += len(numberLiteral.FindString(sql[i:]))
			out.WriteByte('?')
		case sql[i] == '_' || sql[i] >= 'a' && sql[i] <= 'z' || sql[i] >= 'A' && sql[i] <= 'Z' || sql[i] >= 128:
			start := i
			for i < len(sql) && (sql[i] == '_' || sql[i] == '$' || sql[i] >= 'a' && sql[i] <= 'z' || sql[i] >= 'A' && sql[i] <= 'Z' || sql[i] >= '0' && sql[i] <= '9' || sql[i] >= 128) {
				i++
			}
			out.WriteString(sql[start:i])
		default:
			out.WriteByte(sql[i])
			i++
		}
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

func identByte(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 128
}

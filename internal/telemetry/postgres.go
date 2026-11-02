package telemetry

import (
	"context"
	"regexp"
	"strings"

	"github.com/getsentry/sentry-go"
	"github.com/jackc/pgx/v5"
)

// QueryTracer traces Query, QueryRow and Exec, including calls on transactions.
// It never reads Args, connection configuration, returned rows, or error text.
type QueryTracer struct{}

type querySpanKey struct{}

var _ pgx.QueryTracer = QueryTracer{}

func (QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	parent := sentry.SpanFromContext(ctx)
	if parent == nil || parent.Sampled != sentry.SampledTrue {
		return ctx
	}
	span := sentry.StartSpan(ctx, "db.sql.query", sentry.WithDescription(normalizeSQL(data.SQL)))
	span.SetData("db.system", "postgresql")
	return context.WithValue(span.Context(), querySpanKey{}, span)
}

func (QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if span, ok := ctx.Value(querySpanKey{}).(*sentry.Span); ok {
		span.SetData("db.rows_affected", data.CommandTag.RowsAffected())
		span.Status = sentry.SpanStatusOK
		if data.Err != nil {
			span.Status = sentry.SpanStatusInternalError
		}
		span.Finish()
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
			// Backslash escapes only inside E'...' strings; with
			// standard_conforming_strings on, 'a\' ends at its second quote.
			escapes := quote == '\'' && i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && (i < 2 || !identByte(sql[i-2]))
			i++
			closed := false
			for i < len(sql) {
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

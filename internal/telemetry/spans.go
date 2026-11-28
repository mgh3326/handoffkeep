package telemetry

import (
	"context"

	"github.com/getsentry/sentry-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// SizeAttribute is the only datum an internal span carries: the byte count of
// the work it measured. The span name is a fixed string in the code, so a span
// holds no names, refs or content.
const SizeAttribute = "attachment.size_bytes"

// Span opens a child span named name under the request span carried by ctx. It
// is a Sentry span when the Sentry transaction is sampled and an OTLP span when
// the OTLP server span is recording, the same rule as the query tracer. With
// neither, it creates nothing and end does nothing.
//
// The returned context carries the new span for work inside it. end ends the
// span with the byte count it measured and whether the work failed; error text
// is never recorded.
func Span(ctx context.Context, name string) (context.Context, func(size int64, failed bool)) {
	sentryParent := sentry.SpanFromContext(ctx)
	otelParent := trace.SpanFromContext(ctx)
	sentryOn := sentryParent != nil && sentryParent.Sampled == sentry.SampledTrue
	otelOn := otelParent.IsRecording()
	if !sentryOn && !otelOn {
		return ctx, func(int64, bool) {}
	}
	var sentrySpan *sentry.Span
	if sentryOn {
		sentrySpan = sentry.StartSpan(ctx, name, sentry.WithDescription(name))
		ctx = sentrySpan.Context()
	}
	var otelSpan trace.Span
	if otelOn {
		ctx, otelSpan = otelParent.TracerProvider().Tracer(otlpTracerName).Start(ctx, name,
			trace.WithSpanKind(trace.SpanKindInternal))
	}
	return ctx, func(size int64, failed bool) {
		if sentrySpan != nil {
			sentrySpan.SetData(SizeAttribute, size)
			sentrySpan.Status = sentry.SpanStatusOK
			if failed {
				sentrySpan.Status = sentry.SpanStatusInternalError
			}
			sentrySpan.Finish()
		}
		if otelSpan != nil {
			otelSpan.SetAttributes(attribute.Int64(SizeAttribute, size))
			if failed {
				otelSpan.SetStatus(codes.Error, "")
			}
			otelSpan.End()
		}
	}
}

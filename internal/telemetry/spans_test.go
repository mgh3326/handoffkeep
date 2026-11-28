package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/getsentry/sentry-go"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestSpanCreatesNothingWithoutRecordingParent(t *testing.T) {
	ctx := context.Background()
	got, end := Span(ctx, "attachment.hash")
	end(10, true)
	if got != ctx {
		t.Fatal("Span changed the context without a recording parent")
	}
}

func TestSpanIsChildOfRecordingParentWithSizeOnly(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx, parent := provider.Tracer("test").Start(context.Background(), "server")
	_, end := Span(ctx, "attachment.object_put")
	end(4096, true)
	parent.End()

	var child *tracetest.SpanStub
	spans := exporter.GetSpans()
	for i := range spans {
		if spans[i].Name == "attachment.object_put" {
			child = &spans[i]
		}
	}
	if child == nil {
		t.Fatalf("no attachment.object_put span among %v", names(spans))
	}
	if child.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Fatal("span is not a child of the request span")
	}
	if child.Status.Code != codes.Error || child.Status.Description != "" {
		t.Fatalf("failed span status = %+v, want error with no text", child.Status)
	}
	attrs := child.Attributes
	if len(attrs) != 1 || string(attrs[0].Key) != SizeAttribute || attrs[0].Value.AsInt64() != 4096 {
		t.Fatalf("attributes = %v, want only %s=4096", attrs, SizeAttribute)
	}
}

func TestAttachmentSpansReachSentryWithSizeOnly(t *testing.T) {
	monitor, transport := testMonitor(t)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/attachments", func(w http.ResponseWriter, r *http.Request) {
		_, end := Span(r.Context(), "attachment.hash")
		end(7, false)
		w.WriteHeader(http.StatusCreated)
	})
	monitor.Handler(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/v1/attachments", nil))
	if len(transport.events) != 1 {
		t.Fatalf("got %d events", len(transport.events))
	}
	spans := transport.events[0].Spans
	if len(spans) != 1 || spans[0].Op != "attachment.hash" {
		t.Fatalf("spans = %+v, want one attachment.hash span", spans)
	}
	if !reflect.DeepEqual(spans[0].Data, map[string]any{SizeAttribute: int64(7)}) {
		t.Fatalf("span data = %v, want only the size", spans[0].Data)
	}
}

func TestScrubKeepsOnlyTheSizeOfAttachmentSpans(t *testing.T) {
	event := &sentry.Event{Spans: []*sentry.Span{{
		Op:          "attachment.db_write",
		Description: "attachment.db_write",
		Data:        map[string]any{SizeAttribute: int64(3), "name": "private.txt"},
	}}}
	got := scrubEvent(event, nil).Spans[0]
	if !reflect.DeepEqual(got.Data, map[string]any{SizeAttribute: int64(3)}) {
		t.Fatalf("scrubbed data = %v", got.Data)
	}
}

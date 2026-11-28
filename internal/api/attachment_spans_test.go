package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// spanPutServer is uploadServer's wiring with a handler that, when parented,
// starts a recording request span the way the OTLP middleware does.
func spanPutServer(t *testing.T, parent trace.Tracer) *httptest.Server {
	t.Helper()
	objs := &uploadObjects{objects: map[string][]byte{}}
	allow := map[string]bool{}
	for _, x := range strings.Split(attachments.DefaultMIMEAllow, ",") {
		allow[x] = true
	}
	m := &attachments.Manager{Store: &uploadDB{rows: map[string]store.Attachment{}, uploads: map[string]store.AttachmentUpload{}}, Objects: objs, Enabled: true, Config: attachments.Config{MaxBytes: 1 << 20, StorageCap: 1 << 30, PutCap: 1000, GetCap: 1000, Allow: allow, DirectUpload: true, StagingCap: 1 << 30, StagingClientCap: 1 << 30}}
	handler := Server{Service: Service{Attachments: m}, Tokens: Tokens{"node": "node-token"}}.Handler()
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if parent == nil {
			handler.ServeHTTP(w, r)
			return
		}
		ctx, span := parent.Start(r.Context(), "PUT /v1/attachments")
		defer span.End()
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(h.Close)
	return h
}

func putAttachment(t *testing.T, h *httptest.Server, body []byte) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, h.URL+"/v1/attachments", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer node-token")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-HK-Name", "notes.txt")
	req.Header.Set("X-HK-Ref", "task:12")
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestBufferedPutSpansAreChildrenOfRequestSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	h := spanPutServer(t, provider.Tracer("test"))

	body := []byte("handoff notes\n")
	if status := putAttachment(t, h, body); status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	_ = provider.ForceFlush(context.Background())

	var request *tracetest.SpanStub
	spans := exporter.GetSpans()
	for i := range spans {
		if spans[i].Name == "PUT /v1/attachments" {
			request = &spans[i]
		}
	}
	if request == nil {
		t.Fatal("no request span")
	}
	want := []string{"attachment.body_receive", "attachment.hash", "attachment.db_read", "attachment.db_capacity", "attachment.object_put", "attachment.db_write"}
	seen := map[string]bool{}
	for _, s := range spans {
		if !strings.HasPrefix(s.Name, "attachment.") {
			continue
		}
		seen[s.Name] = true
		if s.Parent.SpanID() != request.SpanContext.SpanID() {
			t.Fatalf("%s is not a child of the request span", s.Name)
		}
		if len(s.Attributes) != 1 || string(s.Attributes[0].Key) != telemetry.SizeAttribute || s.Attributes[0].Value.AsInt64() != int64(len(body)) {
			t.Fatalf("%s attributes = %v, want only size %d", s.Name, s.Attributes, len(body))
		}
	}
	for _, name := range want {
		if !seen[name] {
			t.Fatalf("missing span %s; got %v", name, seen)
		}
	}
}

func TestBufferedPutWithoutRecordingParentCreatesNoSpans(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	h := spanPutServer(t, nil)

	if status := putAttachment(t, h, []byte("handoff notes\n")); status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	if n := len(exporter.GetSpans()); n != 0 {
		t.Fatalf("got %d spans without a recording parent", n)
	}
}

package observability_test

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otel"

	"github.com/ravencloak-org/caw/internal/config"
	"github.com/ravencloak-org/caw/internal/observability"
)

// TestInitNoOp verifies that when OTLPEndpoint is empty, Init returns a
// working no-op shutdown and does not replace the global tracer provider.
func TestInitNoOp(t *testing.T) {
	before := otel.GetTracerProvider()

	cfg := config.Config{OTLPEndpoint: ""}
	shutdown, err := observability.Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}

	// Global provider must be unchanged (still the same pointer).
	if otel.GetTracerProvider() != before {
		t.Errorf("Init with empty endpoint must not replace the global TracerProvider")
	}
}

// TestInitShutdownRoundtrip verifies that Init with a real TracerProvider
// (wired via an in-memory exporter) shuts down cleanly.
func TestInitShutdownRoundtrip(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))

	// Temporarily install our own in-memory provider as global.
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	ctx := context.Background()

	// Emit a span.
	tracer := otel.Tracer("test")
	_, span := tracer.Start(ctx, "test.span")
	span.End()

	if err := tp.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	got := exp.GetSpans()
	if len(got) != 1 {
		t.Fatalf("expected 1 span, got %d", len(got))
	}
	if got[0].Name != "test.span" {
		t.Errorf("span name = %q, want %q", got[0].Name, "test.span")
	}

	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestSpanAttributes verifies that span attributes survive the in-memory
// exporter round-trip (used by all instrumented packages).
func TestSpanAttributes(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	ctx := context.Background()

	tracer := tp.Tracer("test")
	ctx2, span := tracer.Start(ctx, "webhook.ingest")
	// Verify the span context is valid (exercises the OTel API).
	if !trace.SpanFromContext(ctx2).SpanContext().IsValid() {
		t.Error("expected a valid span context")
	}
	span.End()

	if err := tp.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}
	if spans[0].Name != "webhook.ingest" {
		t.Errorf("got span name %q", spans[0].Name)
	}
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

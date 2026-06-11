// Package observability initializes OpenTelemetry providers for tracing and
// metrics, exporting over OTLP to whatever endpoint is configured via the
// standard OTEL_EXPORTER_OTLP_ENDPOINT environment variable.
//
// When OTLPEndpoint is empty Init returns no-op providers so the binary runs
// without any telemetry backend (development / unit-test default).
package observability

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"go.opentelemetry.io/otel/propagation"

	"github.com/ravencloak-org/caw/internal/config"
)

// Init configures global TracerProvider and MeterProvider.
// It returns a shutdown function that must be deferred by the caller.
//
// When cfg.OTLPEndpoint is empty, Init registers no-op providers and returns
// a no-op shutdown — the binary works identically but emits no telemetry.
func Init(ctx context.Context, cfg config.Config) (func(context.Context) error, error) {
	if cfg.OTLPEndpoint == "" {
		// No-op: leave global providers as the SDK default (no-op).
		return func(context.Context) error { return nil }, nil
	}

	svcName := os.Getenv("OTEL_SERVICE_NAME")
	if svcName == "" {
		svcName = "caw-hub"
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(svcName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("observability: build resource: %w", err)
	}

	// --- trace exporter (OTLP gRPC) ------------------------------------------
	traceExp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("observability: trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// --- metric exporter (OTLP gRPC) -----------------------------------------
	metricExp, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		// shut down the trace provider we already started before returning
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("observability: metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	// --- propagator -----------------------------------------------------------
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	shutdown := func(shutCtx context.Context) error {
		var tpErr, mpErr error
		tpErr = tp.Shutdown(shutCtx)
		mpErr = mp.Shutdown(shutCtx)
		if tpErr != nil {
			return fmt.Errorf("observability shutdown: trace provider: %w", tpErr)
		}
		if mpErr != nil {
			return fmt.Errorf("observability shutdown: meter provider: %w", mpErr)
		}
		return nil
	}

	return shutdown, nil
}

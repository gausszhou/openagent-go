// Package otel provides OpenTelemetry integration for openagent.
//
// setup.go initializes the TracerProvider and OTLP exporter from config.
// hook.go provides the RunHooks implementation that creates spans.
//
// Usage:
//
//	tp, err := otel.SetupTracer(ctx, otel.Config{
//	    Endpoint:   "http://localhost:4318",
//	    ServiceName: "openagent",
//	})
//	if err != nil { ... }
//	defer tp.Shutdown(ctx)   // flush pending spans
//
//	tracer := tp.Tracer(version.Name)
//	deps := kernel.Deps{Hooks: otel.New(tracer), ...}
package otel

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/yusheng-g/openagent-go/version"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Config holds the settings for OTel tracer initialization. It mirrors
// config.TelemetryConfig but lives in the otel package so callers outside
// cmd/cli/config can use it without a circular import.
type Config struct {
	Endpoint    string // OTLP endpoint URL; empty = no-op provider
	Protocol    string // "http" (default) or "grpc"
	ServiceName string // OTel resource service.name; default "openagent"
	Insecure    bool   // disable TLS (default true for local collectors)
}

// SetupResult bundles the initialized TracerProvider with a shutdown func.
// The caller must defer Shutdown to flush pending spans before exit.
type SetupResult struct {
	Provider *sdktrace.TracerProvider
	Tracer   trace.Tracer
}

// SetupTracer initializes a TracerProvider with an OTLP exporter and returns
// it along with a tracer. When Endpoint is empty, returns a no-op provider
// (spans are never sent) so the caller can wire Hooks unconditionally without
// checking whether telemetry is configured.
//
// Telemetry NEVER blocks server startup: a misconfigured endpoint or
// unsupported protocol degrades to a no-op tracer (spans sampled out) with
// a warning log, so the agent runs normally without traces.
//
// The caller MUST defer Shutdown to flush the batch span processor before the
// process exits, otherwise pending spans are lost.
func SetupTracer(ctx context.Context, cfg Config) (*SetupResult, error) {
	if cfg.Endpoint == "" {
		// No endpoint configured: return nil tracer so the caller skips
		// mounting the OTel hook entirely — zero overhead, no spans
		// created, no goroutines started.
		return &SetupResult{Provider: nil, Tracer: nil}, nil
	}

	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = version.Name
	}
	protocol := strings.ToLower(strings.TrimSpace(cfg.Protocol))
	if protocol == "" {
		protocol = "http"
	}

	// Build the OTLP trace exporter. A creation failure or unsupported
	// protocol degrades to no-op rather than blocking startup.
	var exporter sdktrace.SpanExporter
	var err error
	switch protocol {
	case "grpc":
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exporter, err = otlptracegrpc.New(ctx, opts...)
	case "http":
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exporter, err = otlptracehttp.New(ctx, opts...)
	default:
		slog.Warn("unsupported telemetry protocol, degrading to no-op",
			"protocol", protocol, "valid", "http, grpc")
		return &SetupResult{Provider: nil, Tracer: nil}, nil
	}
	if err != nil {
		slog.Warn("OTLP exporter creation failed, degrading to no-op",
			"protocol", protocol, "endpoint", cfg.Endpoint, "error", err)
		return &SetupResult{Provider: nil, Tracer: nil}, nil
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version.Version),
		),
	)
	if err != nil {
		slog.Warn("OTel resource creation failed, degrading to no-op", "error", err)
		return &SetupResult{Provider: nil, Tracer: nil}, nil
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(newLoggingExporter(exporter)),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return &SetupResult{Provider: tp, Tracer: tp.Tracer(version.Name)}, nil
}

// loggingExporter wraps a SpanExporter and logs the first export failure
// so the user knows traces are not reaching the backend (the OTel batch
// processor silently drops spans on export errors). Subsequent failures are
// suppressed to avoid log spam — one warning is enough.
type loggingExporter struct {
	inner      sdktrace.SpanExporter
	failedOnce sync.Once
}

func newLoggingExporter(inner sdktrace.SpanExporter) sdktrace.SpanExporter {
	return &loggingExporter{inner: inner}
}

func (e *loggingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.inner.ExportSpans(ctx, spans)
	if err != nil {
		e.failedOnce.Do(func() {
			slog.Warn("telemetry export failed — traces are not reaching the backend",
				"error", err,
				"hint", "check the telemetry.endpoint in settings.json and that the OTLP collector is running")
		})
	}
	return err
}

func (e *loggingExporter) Shutdown(ctx context.Context) error {
	return e.inner.Shutdown(ctx)
}

// TracerHolder holds a mutable trace.Tracer so hooks can swap the tracer
// at runtime (e.g. when telemetry endpoint changes via settings reload).
// The hooks call Tracer() on every span creation; Set replaces the tracer
// under a read-write lock.
type TracerHolder struct {
	mu sync.RWMutex
	tr trace.Tracer
}

// NewTracerHolder wraps a tracer (nil = no-op, spans are never exported).
func NewTracerHolder(tr trace.Tracer) *TracerHolder {
	return &TracerHolder{tr: tr}
}

// Tracer returns the current tracer (nil-safe).
func (h *TracerHolder) Tracer() trace.Tracer {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tr
}

// Set replaces the tracer. Safe to call while hooks are actively creating
// spans — the read lock ensures in-flight Start calls finish on the old
// tracer before the swap takes effect.
func (h *TracerHolder) Set(tr trace.Tracer) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.tr = tr
	h.mu.Unlock()
}

// Shutdown flushes pending spans and closes the exporter. Must be called
// on process exit. Errors are logged but not fatal — losing pending spans
// on shutdown is preferable to hanging.
func (r *SetupResult) Shutdown(ctx context.Context) error {
	if r == nil || r.Provider == nil {
		return nil
	}
	return r.Provider.Shutdown(ctx)
}

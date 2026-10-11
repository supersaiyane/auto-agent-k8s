// Package tracing exports OpenTelemetry spans for findings and gate
// decisions when it is turned on (ISS-018). Off, it installs nothing and the
// otel API stays a no-op.
package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/klog/v2"
)

const instrumentation = "github.com/supersaiyane/auto-agent-k8s"

// Config turns tracing on and says where spans go.
type Config struct {
	Enabled  bool
	Endpoint string // OTLP/HTTP base URL, e.g. http://otel-collector.monitoring:4318
	Version  string
	Pod      string
}

// Start installs an OTLP exporter when tracing is on and returns the
// function that flushes and stops it; off, it returns a no-op.
func Start(ctx context.Context, c Config) (func(context.Context) error, error) {
	if !c.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	if c.Endpoint == "" {
		return nil, fmt.Errorf("tracing: enabled without an endpoint")
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(c.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("tracing: exporter: %w", err)
	}
	return install(exp, c), nil
}

// install sets the global tracer provider over exp.
func install(exp sdktrace.SpanExporter, c Config) func(context.Context) error {
	res := resource.NewSchemaless(
		attribute.String("service.name", "auto-agent"),
		attribute.String("service.version", c.Version),
		attribute.String("k8s.pod.name", c.Pod),
	)
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	klog.Infof("tracing: exporting spans to %s", c.Endpoint)
	return tp.Shutdown
}

// Span starts a span named name with attrs. Callers end it. Attributes are
// names only (reason, namespace, workload, action), never message text, so
// nothing a redactor would remove leaves the cluster this way.
func Span(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer(instrumentation).Start(ctx, name, trace.WithAttributes(attrs...))
}

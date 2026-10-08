package tracing

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestStart_OffInstallsNothing(t *testing.T) {
	before := otel.GetTracerProvider()
	stop, err := Start(context.Background(), Config{})
	if err != nil || stop(context.Background()) != nil || otel.GetTracerProvider() != before {
		t.Fatal("tracing off changes nothing")
	}
	if _, err := Start(context.Background(), Config{Enabled: true}); err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("on without an endpoint: %v", err)
	}
	stop, err = Start(context.Background(), Config{Enabled: true, Endpoint: "http://127.0.0.1:4318"})
	if err != nil {
		t.Fatal(err)
	}
	defer otel.SetTracerProvider(before)
	if otel.GetTracerProvider() == before {
		t.Fatal("tracing on installs a provider")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // nothing to flush; a closed context returns at once
	_ = stop(ctx)
}

// Spans carry their name and attributes to the exporter.
func TestSpan_Exported(t *testing.T) {
	before := otel.GetTracerProvider()
	defer otel.SetTracerProvider(before)
	exp := tracetest.NewInMemoryExporter()
	stop := install(exp, Config{Endpoint: "memory", Version: "test", Pod: "c-0"})
	_, span := Span(context.Background(), "gate delete_pod", attribute.String("namespace", "default"))
	span.End()
	if err := otel.GetTracerProvider().(*sdktrace.TracerProvider).ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := exp.GetSpans() // read before Shutdown, which clears the in-memory exporter
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "gate delete_pod" || got[0].Attributes[0].Value.AsString() != "default" {
		t.Fatalf("spans %+v", got)
	}
}

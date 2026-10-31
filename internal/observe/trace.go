package observe

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Tracer is the gateway's tracer. Spans follow the OpenTelemetry GenAI semantic
// conventions for upstream calls [OBS-03].
var Tracer trace.Tracer = otel.Tracer("llm-gateway")

// Propagator continues incoming W3C trace context (traceparent/tracestate).
var Propagator propagation.TextMapPropagator = propagation.TraceContext{}

// TracingConfig configures OTLP export. Langfuse and LangSmith both accept OTLP, so
// they are configured as OTLP endpoints with their auth headers [OBS-04].
type TracingConfig struct {
	Endpoint string            // e.g. https://cloud.langfuse.com/api/public/otel
	Headers  map[string]string // resolved secret values
	Sample   float64
}

// SetupTracing installs a tracer provider. With no endpoint and no extra processors
// tracing still runs in-process (spans are created and propagated, nothing exported).
func SetupTracing(ctx context.Context, cfg TracingConfig, extra ...sdktrace.SpanProcessor) (func(context.Context) error, error) {
	sampler := sdktrace.ParentBased(sdktrace.AlwaysSample())
	if cfg.Sample > 0 && cfg.Sample < 1 {
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Sample))
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithSampler(sampler)}
	if cfg.Endpoint != "" {
		eopts := []otlptracehttp.Option{otlptracehttp.WithHeaders(cfg.Headers)}
		ep := cfg.Endpoint
		if strings.HasPrefix(ep, "http://") {
			eopts = append(eopts, otlptracehttp.WithInsecure())
		}
		eopts = append(eopts, otlptracehttp.WithEndpointURL(strings.TrimRight(ep, "/")+"/v1/traces"))
		exp, err := otlptracehttp.New(ctx, eopts...)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	for _, p := range extra {
		opts = append(opts, sdktrace.WithSpanProcessor(p))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(Propagator)
	Tracer = tp.Tracer("llm-gateway")
	return tp.Shutdown, nil
}

// Start begins a span.
func Start(ctx context.Context, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
}

// GenAI attribute helpers (semantic conventions).
func GenAIRequest(provider, model, operation string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", operation),
		attribute.String("gen_ai.provider.name", provider),
		attribute.String("gen_ai.request.model", model),
	}
}

func GenAIUsage(in, out int, responseModel string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.Int("gen_ai.usage.input_tokens", in),
		attribute.Int("gen_ai.usage.output_tokens", out),
		attribute.String("gen_ai.response.model", responseModel),
	}
}

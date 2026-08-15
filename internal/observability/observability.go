// Package observability wires up structured logging, metrics, and tracing
// so every service emits them from day one rather than bolting them on later.
package observability

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

const serviceName = "kolo-bank-server"

// NewLogger returns a structured JSON logger writing to stdout.
func NewLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler)
}

// InitTracing configures the global OTEL tracer provider. With an
// otlpEndpoint it exports there; otherwise it prints spans to stdout, so
// tracing is genuinely observable in every environment rather than a
// silently-wired no-op until a collector exists.
func InitTracing(ctx context.Context, otlpEndpoint string) (shutdown func(context.Context) error, err error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("observability: build resource: %w", err)
	}

	var opts []sdktrace.TracerProviderOption
	opts = append(opts, sdktrace.WithResource(res))

	if otlpEndpoint != "" {
		exporter, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(otlpEndpoint),
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return nil, fmt.Errorf("observability: build otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	} else {
		exporter, err := stdouttrace.New()
		if err != nil {
			return nil, fmt.Errorf("observability: build stdout trace exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

// InitMetrics configures the global OTEL meter provider, mirroring InitTracing:
// metrics are always recorded, exported only when otlpEndpoint is set.
func InitMetrics(ctx context.Context, otlpEndpoint string) (shutdown func(context.Context) error, err error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("observability: build resource: %w", err)
	}

	var opts []metric.Option
	opts = append(opts, metric.WithResource(res))

	if otlpEndpoint != "" {
		exporter, err := otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithEndpoint(otlpEndpoint),
			otlpmetricgrpc.WithInsecure(),
		)
		if err != nil {
			return nil, fmt.Errorf("observability: build otlp metric exporter: %w", err)
		}
		opts = append(opts, metric.WithReader(metric.NewPeriodicReader(exporter)))
	} else {
		exporter, err := stdoutmetric.New()
		if err != nil {
			return nil, fmt.Errorf("observability: build stdout metric exporter: %w", err)
		}
		opts = append(opts, metric.WithReader(metric.NewPeriodicReader(exporter)))
	}

	mp := metric.NewMeterProvider(opts...)
	otel.SetMeterProvider(mp)

	return mp.Shutdown, nil
}

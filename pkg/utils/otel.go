// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package utils provides OpenTelemetry SDK setup utilities.
package utils

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/propagators/jaeger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

const (
	OTelProtocolGRPC = "grpc"
	OTelProtocolHTTP = "http"
	OTelExporterOTLP = "otlp"
	OTelExporterNone = "none"
)

// OTelConfig holds OpenTelemetry configuration.
// All fields can be set via the standard OTEL_* environment variables.
type OTelConfig struct {
	ServiceName       string
	ServiceVersion    string
	Protocol          string
	Endpoint          string
	Insecure          bool
	TracesExporter    string
	TracesSampleRatio float64
	MetricsExporter   string
	LogsExporter      string
	Propagators       string
}

// OTelConfigFromEnv reads OTel configuration from environment variables.
func OTelConfigFromEnv() OTelConfig {
	cfg := OTelConfig{
		ServiceName:       envOrDefault("OTEL_SERVICE_NAME", "lfx-v2-campaign-service"),
		ServiceVersion:    os.Getenv("OTEL_SERVICE_VERSION"),
		Protocol:          envOrDefault("OTEL_EXPORTER_OTLP_PROTOCOL", OTelProtocolGRPC),
		Endpoint:          os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Insecure:          os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
		TracesExporter:    envOrDefault("OTEL_TRACES_EXPORTER", OTelExporterNone),
		MetricsExporter:   envOrDefault("OTEL_METRICS_EXPORTER", OTelExporterNone),
		LogsExporter:      envOrDefault("OTEL_LOGS_EXPORTER", OTelExporterNone),
		Propagators:       envOrDefault("OTEL_PROPAGATORS", "tracecontext,baggage"),
		TracesSampleRatio: 1.0,
	}

	if ratio := os.Getenv("OTEL_TRACES_SAMPLE_RATIO"); ratio != "" {
		if parsed, err := strconv.ParseFloat(ratio, 64); err == nil && parsed >= 0.0 && parsed <= 1.0 {
			cfg.TracesSampleRatio = parsed
		} else {
			slog.Warn("invalid OTEL_TRACES_SAMPLE_RATIO, using 1.0", "value", ratio)
		}
	}

	return cfg
}

// SetupOTelSDK bootstraps the OTel pipeline using environment-based configuration.
func SetupOTelSDK(ctx context.Context) (func(context.Context) error, error) {
	return SetupOTelSDKWithConfig(ctx, OTelConfigFromEnv())
}

// SetupOTelSDKWithConfig bootstraps the OTel pipeline with an explicit config.
// Call the returned shutdown function for cleanup.
func SetupOTelSDKWithConfig(ctx context.Context, cfg OTelConfig) (shutdown func(context.Context) error, err error) {
	var shutdownFuncs []func(context.Context) error

	shutdown = func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}

	handleErr := func(inErr error) {
		err = errors.Join(inErr, shutdown(ctx))
	}

	if cfg.Endpoint != "" {
		cfg.Endpoint = normalizeEndpointURL(cfg.Endpoint, cfg.Insecure)
	}

	res, err := newResource(cfg)
	if err != nil {
		handleErr(err)
		return
	}

	otel.SetTextMapPropagator(newPropagator(cfg))

	if cfg.TracesExporter != OTelExporterNone {
		tp, tpErr := newTraceProvider(ctx, cfg, res)
		if tpErr != nil {
			handleErr(tpErr)
			return
		}
		shutdownFuncs = append(shutdownFuncs, tp.Shutdown)
		otel.SetTracerProvider(tp)
	}

	if cfg.MetricsExporter != OTelExporterNone {
		mp, mpErr := newMetricsProvider(ctx, cfg, res)
		if mpErr != nil {
			handleErr(mpErr)
			return
		}
		shutdownFuncs = append(shutdownFuncs, mp.Shutdown)
		otel.SetMeterProvider(mp)
	}

	if cfg.LogsExporter != OTelExporterNone {
		lp, lpErr := newLoggerProvider(ctx, cfg, res)
		if lpErr != nil {
			handleErr(lpErr)
			return
		}
		shutdownFuncs = append(shutdownFuncs, lp.Shutdown)
		global.SetLoggerProvider(lp)
	}

	return
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newResource(cfg OTelConfig) (*resource.Resource, error) {
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
}

func newPropagator(cfg OTelConfig) propagation.TextMapPropagator {
	var props []propagation.TextMapPropagator
	for _, p := range strings.Split(cfg.Propagators, ",") {
		switch strings.TrimSpace(p) {
		case "tracecontext":
			props = append(props, propagation.TraceContext{})
		case "baggage":
			props = append(props, propagation.Baggage{})
		case "jaeger":
			props = append(props, jaeger.Jaeger{})
		default:
			slog.Warn("unknown OTel propagator, skipping", "propagator", p)
		}
	}
	if len(props) == 0 {
		props = []propagation.TextMapPropagator{propagation.TraceContext{}, propagation.Baggage{}}
	}
	return propagation.NewCompositeTextMapPropagator(props...)
}

// normalizeEndpointURL ensures the endpoint has a URL scheme for the OTel SDK url.Parse call.
func normalizeEndpointURL(raw string, insecure bool) string {
	if strings.Contains(raw, "://") {
		return raw
	}
	if insecure {
		return "http://" + raw
	}
	return "https://" + raw
}

func newTraceProvider(ctx context.Context, cfg OTelConfig, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	var (
		exporter sdktrace.SpanExporter
		err      error
	)
	if cfg.Protocol == OTelProtocolHTTP {
		opts := []otlptracehttp.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exporter, err = otlptracehttp.New(ctx, opts...)
	} else {
		opts := []otlptracegrpc.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exporter, err = otlptracegrpc.New(ctx, opts...)
	}
	if err != nil {
		return nil, err
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.TracesSampleRatio)),
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(time.Second)),
	), nil
}

func newMetricsProvider(ctx context.Context, cfg OTelConfig, res *resource.Resource) (*metric.MeterProvider, error) {
	var (
		exporter metric.Exporter
		err      error
	)
	if cfg.Protocol == OTelProtocolHTTP {
		opts := []otlpmetrichttp.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		exporter, err = otlpmetrichttp.New(ctx, opts...)
	} else {
		opts := []otlpmetricgrpc.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		exporter, err = otlpmetricgrpc.New(ctx, opts...)
	}
	if err != nil {
		return nil, err
	}
	return metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter, metric.WithInterval(30*time.Second))),
	), nil
}

func newLoggerProvider(ctx context.Context, cfg OTelConfig, res *resource.Resource) (*log.LoggerProvider, error) {
	var (
		exporter log.Exporter
		err      error
	)
	if cfg.Protocol == OTelProtocolHTTP {
		opts := []otlploghttp.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlploghttp.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlploghttp.WithInsecure())
		}
		exporter, err = otlploghttp.New(ctx, opts...)
	} else {
		opts := []otlploggrpc.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlploggrpc.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlploggrpc.WithInsecure())
		}
		exporter, err = otlploggrpc.New(ctx, opts...)
	}
	if err != nil {
		return nil, err
	}
	return log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(exporter)),
	), nil
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utils

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// TestOTelConfigFromEnv_Sampler verifies that the sampler env vars are read
// (and normalized) into the config.
func TestOTelConfigFromEnv_Sampler(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "  ParentBased_TraceIDRatio  ")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "  0.25  ")

	cfg := OTelConfigFromEnv()

	if cfg.TracesSampler != "parentbased_traceidratio" {
		t.Errorf("TracesSampler = %q, want %q", cfg.TracesSampler, "parentbased_traceidratio")
	}
	if cfg.TracesSamplerArg != "0.25" {
		t.Errorf("TracesSamplerArg = %q, want %q", cfg.TracesSamplerArg, "0.25")
	}
}

// TestNewResource_NoSchemaConflict guards against the OTel resource schema-URL
// conflict that crashes the service at startup. resource.Merge errors when the
// SDK's default resource and our attribute resource carry differing non-empty
// schema URLs, so this fails if the semconv import ever drifts from the schema
// URL bundled by the installed go.opentelemetry.io/otel/sdk version.
func TestNewResource_NoSchemaConflict(t *testing.T) {
	res, err := newResource(OTelConfig{ServiceName: "test-svc", ServiceVersion: "1.2.3"})
	if err != nil {
		t.Fatalf("newResource returned error (semconv import likely out of sync with the OTel SDK default resource schema URL): %v", err)
	}
	if res == nil {
		t.Fatal("newResource returned nil resource")
	}
	if got := res.SchemaURL(); got != semconv.SchemaURL {
		t.Errorf("resource SchemaURL = %q, want %q", got, semconv.SchemaURL)
	}
}

// TestNewSampler verifies that newSampler returns a non-nil, described sampler
// for every supported OTEL_TRACES_SAMPLER value, including the default (empty)
// and unknown cases.
func TestNewSampler(t *testing.T) {
	tests := []struct {
		name    string
		sampler string
		arg     string
	}{
		{"default (empty)", "", ""},
		{"always_on", "always_on", ""},
		{"always_off", "always_off", ""},
		{"traceidratio", "traceidratio", "0.5"},
		{"parentbased_always_on", "parentbased_always_on", ""},
		{"parentbased_always_off", "parentbased_always_off", ""},
		{"parentbased_traceidratio", "parentbased_traceidratio", "0.5"},
		{"unknown", "unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSampler(OTelConfig{TracesSampler: tt.sampler, TracesSamplerArg: tt.arg})
			if s == nil {
				t.Fatalf("newSampler(%q) returned nil", tt.sampler)
			}
			if s.Description() == "" {
				t.Errorf("newSampler(%q).Description() is empty", tt.sampler)
			}
		})
	}
}

// TestNewSampler_InvalidArg verifies that an invalid or out-of-range
// OTEL_TRACES_SAMPLER_ARG falls back to 1.0 without panicking.
func TestNewSampler_InvalidArg(t *testing.T) {
	for _, arg := range []string{"invalid", "-0.5", "1.5"} {
		s := newSampler(OTelConfig{TracesSampler: "parentbased_traceidratio", TracesSamplerArg: arg})
		if s == nil {
			t.Fatalf("newSampler returned nil for arg %q", arg)
		}
	}
}

// TestNewSampler_ParentHonored verifies that the default sampler honors an
// upstream (remote) parent's sampling decision, keeping distributed traces intact.
func TestNewSampler_ParentHonored(t *testing.T) {
	s := newSampler(OTelConfig{}) // default = parentbased_traceidratio

	sampledParent := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    oteltrace.TraceID{0x01},
		SpanID:     oteltrace.SpanID{0x01},
		TraceFlags: oteltrace.FlagsSampled,
		Remote:     true,
	})
	parentCtx := oteltrace.ContextWithRemoteSpanContext(context.Background(), sampledParent)

	result := s.ShouldSample(sdktrace.SamplingParameters{ParentContext: parentCtx})
	if result.Decision != sdktrace.RecordAndSample {
		t.Errorf("expected RecordAndSample with sampled parent, got %v", result.Decision)
	}
}

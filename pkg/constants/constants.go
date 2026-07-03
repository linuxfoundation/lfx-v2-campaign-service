// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package constants defines application-wide constants.
package constants

// Environment variable names used to configure the service.
const (
	EnvPort     = "PORT"
	EnvHost     = "HOST"
	EnvDebug    = "DEBUG"
	EnvJWKSURL  = "JWKS_URL"
	EnvAudience = "JWT_AUDIENCE"
	EnvIssuer   = "JWT_ISSUER"
	EnvNATSURL  = "NATS_URL"
)

// Default configuration values. These mirror the defaults wired into the Helm
// chart so local runs match in-cluster behavior.
const (
	// DefaultHTTPPort is the default port the HTTP server listens on.
	DefaultHTTPPort = "8080"
	// DefaultHost is the default bind interface ("*" binds all interfaces).
	DefaultHost = "*"
	// DefaultJWKSURL is the default JSON Web Key Set endpoint for JWT validation.
	DefaultJWKSURL = "http://lfx-platform-heimdall.lfx.svc.cluster.local:4457/.well-known/jwks"
	// DefaultAudience is the default intended audience for JWT tokens.
	DefaultAudience = "lfx-v2-campaign-service"
	// DefaultIssuer is the default expected JWT issuer.
	DefaultIssuer = "heimdall"
	// DefaultNATSURL is the default NATS server URL.
	DefaultNATSURL = "nats://lfx-platform-nats.lfx.svc.cluster.local:4222"
)

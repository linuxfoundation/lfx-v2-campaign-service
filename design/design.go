// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package design contains the DSL for the campaign service Goa API generation.
package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

var _ = API("lfx-v2-campaign-service", func() {
	Title("LFX V2 - Campaign Service")
	Description("A collection of service endpoints to support Marketing Operations campaign creation and management.")
})

var _ = Service("lfx-v2-campaign-service-svc", func() {
	Description("The campaign service supports Marketing Operations campaign creation and management.")

	Method("readyz", func() {
		Description("Check if the service is able to take inbound requests.")
		Meta("swagger:generate", "false")
		Result(Bytes, func() {
			Example("OK")
		})
		Error("ServiceUnavailable", ServiceUnavailableError, "Service is unavailable")
		HTTP(func() {
			GET("/readyz")
			Response(StatusOK, func() {
				ContentType("text/plain")
			})
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("livez", func() {
		Description("Check if the service is alive.")
		Meta("swagger:generate", "false")
		Result(Bytes, func() {
			Example("OK")
		})
		HTTP(func() {
			GET("/livez")
			Response(StatusOK, func() {
				ContentType("text/plain")
			})
		})
	})

	// Serve the generated OpenAPI documents. These four file servers correspond
	// to the four http.FileSystem arguments passed to the generated server
	// constructor in cmd/campaign-service/server.go.
	Files("/_campaigns/openapi.json", "gen/http/openapi.json", func() {
		Meta("swagger:generate", "false")
	})
	Files("/_campaigns/openapi.yaml", "gen/http/openapi.yaml", func() {
		Meta("swagger:generate", "false")
	})
	Files("/_campaigns/openapi3.json", "gen/http/openapi3.json", func() {
		Meta("swagger:generate", "false")
	})
	Files("/_campaigns/openapi3.yaml", "gen/http/openapi3.yaml", func() {
		Meta("swagger:generate", "false")
	})
})

// ServiceUnavailableError is the DSL type for a service unavailable error.
var ServiceUnavailableError = Type("ServiceUnavailableError", func() {
	Attribute("code", String, "HTTP status code", func() {
		Example("503")
	})
	Attribute("message", String, "Error message", func() {
		Example("The service is unavailable.")
	})
	Required("code", "message")
})

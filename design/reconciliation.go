// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package design — operator reconciliation endpoints.
//
// The service deliberately models several AMBIGUOUS states and leaves them for a
// human: a stranded dispatch claim, a campaign whose upstream create is UNCONFIRMED,
// and a partially-built audience. Each is modelled precisely (see internal/service's
// orchestrator and internal/container's stuck-claim reporter) and each is deliberately
// NOT auto-resolved, because the ambiguity is between "a paid campaign exists upstream"
// and "it does not" — and guessing wrong creates a DUPLICATE PAID CAMPAIGN.
//
// Until now the only record of these states was a log line, which rotates away. These
// endpoints give the operator an INVENTORY (read-only, always safe) and exactly one
// narrowly-scoped write: releasing a claim that carries no evidence of an upstream
// create. Everything that could authorize a duplicate paid create stays manual — see
// docs/api-catalog.md and the PR description for the full rationale.
package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// ─── Reconciliation types ───

// ReconciliationItem is one thing needing operator attention. It is deliberately a
// SINGLE flat type across all three kinds (stuck claim / unconfirmed campaign /
// partial audience) rather than a union: the operator's question is always the same
// ("what is stuck, how old, and what do I do about it"), and a flat shape keeps the
// inventory renderable as one table.
//
// resolvable is the load-bearing field: it states whether THIS service can act on the
// item at all. False means the only safe resolution is out-of-band (verify upstream,
// then use the existing campaign endpoints) — the API refuses to pretend otherwise.
var ReconciliationItem = Type("reconciliation-item", func() {
	Attribute("kind", String, "What kind of stuck state this is", func() {
		Enum("stuck_claim", "unconfirmed_campaign", "partial_audience")
	})
	Attribute("brief_id", String, "Owning brief")
	Attribute("platform", String, "Platform the stuck state belongs to (empty for an audience item)")
	Attribute("campaign_id", String, "Campaign row UUID (stuck_claim / unconfirmed_campaign)")
	Attribute("audience_id", String, "Audience row UUID (partial_audience)")
	Attribute("status", String, "The row's current persisted status")
	Attribute("platform_campaign_id", String, "Upstream campaign id, when one was recorded. Its PRESENCE is what makes an item non-releasable: it proves something exists upstream.")
	Attribute("age_seconds", Int64, "How long the row has been in this state")
	Attribute("version", Int64, "Optimistic-concurrency version; supply as If-Match to resolve")
	Attribute("etag", String, "ETag to supply as If-Match when resolving")
	// The two operator-facing verdict fields.
	Attribute("resolvable", Boolean, "Whether release-claim can act on this item. False = resolve it out of band; the service will refuse.")
	Attribute("detail", String, "Why this item is stuck and what the operator must verify upstream before acting")
	Required("kind", "brief_id", "status", "age_seconds", "resolvable", "detail")
})

// ReconciliationReport is the whole inventory for a project.
//
// It reports counts alongside the items because the items array is BOUNDED (a project
// with a pathological number of stuck rows must not produce an unbounded response);
// a truncated list with an honest total is more useful than a silently short one.
var ReconciliationReport = Type("reconciliation-report", func() {
	Attribute("project_id", String, "Project this report covers")
	Attribute("items", ArrayOf(ReconciliationItem), "Items needing attention, oldest first")
	Attribute("total", Int64, "Total items found (may exceed len(items) when truncated)")
	Attribute("truncated", Boolean, "True when more items exist than were returned")
	Required("project_id", "items", "total", "truncated")
})

// ─── Reconciliation service ───

var _ = Service("lfx-v2-campaign-service-reconciliation", func() {
	Description("Inspect and resolve dispatch states the service deliberately leaves for a human.")

	Security(JWTAuth)

	Method("get-reconciliation", func() {
		Description("List everything in this project needing operator attention: stranded dispatch claims, campaigns whose upstream create is UNCONFIRMED, and partially-built audiences. Read-only and always safe.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(ReconciliationReport)
		commonBriefErrors(false)
		HTTP(func() {
			GET("/projects/{project_id}/reconciliation")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses(false)
		})
	})

	Method("release-dispatch-claim", func() {
		Description(`Release a STRANDED dispatch claim so the (brief, platform) pair can be dispatched again.

Deliberately narrow. It refuses unless the claim carries NO evidence of an upstream create (no platform_campaign_id and no result blob), is still 'pending', and is older than the release floor — a claim younger than that may still be in flight, and releasing it would let a concurrent dispatch double-create a paid campaign.

Requires If-Match carrying the version the operator observed in the report. That is not ceremony: a claim can be legitimately re-claimed by a new dispatch between the report and this call, which bumps the version. Without the gate that live claim would be deleted. Returns 409 when the item is not releasable and 412 when it changed underneath the operator.`)
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			campaignIDAttr()
			ifMatchAttr()
			// The operator must STATE what they verified upstream. The service never
			// infers it: this is the whole point of the endpoint.
			Attribute("verified_absent", Boolean, "Operator asserts they checked the ad platform and NO campaign exists for this brief/platform. Must be true; the service cannot verify it and will not guess.")
			Required("project_id", "brief_id", "campaign_id", "verified_absent")
		})
		Result(ReconciliationItem)
		commonBriefErrors(true)
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch — the claim changed since the report")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			POST("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/release-claim")
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusOK)
			briefErrorResponses(true)
			Response("PreconditionFailed", StatusPreconditionFailed)
			Response("PreconditionRequired", StatusPreconditionRequired)
		})
	})
})

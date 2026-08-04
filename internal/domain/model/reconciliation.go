// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "time"

// ReconciliationKind discriminates the three states this service deliberately leaves
// for a human. They are grouped under one type because the operator's question is the
// same for all three ("what is stuck, and what may exist upstream"), even though the
// underlying rows live in two different tables.
type ReconciliationKind string

// Reconciliation kinds.
const (
	// ReconcileStuckClaim is a bare 'pending' campaigns row: a claim whose holder
	// died. It carries NO evidence of an upstream create (no platform id, no result
	// blob), which is exactly what makes it the one releasable kind.
	ReconcileStuckClaim ReconciliationKind = "stuck_claim"
	// ReconcileUnconfirmedCampaign is a retained partial orphan: a non-terminal
	// campaigns row that DOES carry evidence something may exist upstream (a
	// platform_campaign_id, and/or a Result reconcile blob, and/or a partial-orphan
	// status like 'unconfirmed'/'group_created'). Never auto-resolvable.
	ReconcileUnconfirmedCampaign ReconciliationKind = "unconfirmed_campaign"
	// ReconcilePartialAudience is a campaign_audiences row left 'building': some
	// platform lists may exist. Never auto-resolvable.
	ReconcilePartialAudience ReconciliationKind = "partial_audience"
)

// ReconciliationItem is one stuck state needing operator attention.
//
// Resolvable is the load-bearing field. It is TRUE only for a bare stuck claim old
// enough that a live dispatch cannot still be running (see ClaimReleaseFloor). For
// every other item the only safe resolution is out of band: verify upstream, then use
// the normal campaign endpoints. The inventory says so explicitly rather than offering
// an action that would have to guess.
type ReconciliationItem struct {
	Kind               ReconciliationKind
	BriefID            string
	Platform           Provider
	CampaignID         string
	AudienceID         string
	Status             string
	PlatformCampaignID string
	Age                time.Duration
	Version            int64
	Resolvable         bool
	Detail             string
}

// ClaimReleaseFloor is the minimum age a 'pending' claim must have before the API will
// release it.
//
// It is DERIVED, not guessed. A live dispatch is hard-bounded by the orchestrator's
// providerCallTimeout (2m), after which the claim is either released or upserted on a
// detached context bounded by persistResultTimeout/claimReleaseTimeout (5s) — so a
// genuinely in-flight claim resolves within ~2m5s. This floor is set far above that,
// leaving a wide margin for replica clock skew and for a paused/descheduled pod that
// resumes late. A floor at or near the real bound would be a duplicate-paid-campaign
// risk for the sake of resolving a few minutes sooner, which is not a trade worth
// making. TestClaimReleaseFloorExceedsDispatchBound pins the relationship.
//
// The floor is NOT the only guard — the release is additionally gated on If-Match and
// on the row carrying no evidence of an upstream create. It is the backstop for the
// case the version gate cannot see: a claim re-created (not merely updated) by a new
// dispatch between the operator's read and their write.
const ClaimReleaseFloor = 15 * time.Minute

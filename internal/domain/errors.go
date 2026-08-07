// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package domain holds the core domain model, port interfaces, and sentinel
// errors for the campaign service. It has no infrastructure dependencies.
package domain

import "errors"

// Sentinel errors returned by repositories and mapped to HTTP status codes at
// the service/handler boundary.
var (
	// ErrNotFound indicates the requested resource does not exist (or has been
	// soft-deleted). Maps to 404.
	ErrNotFound = errors.New("resource not found")

	// ErrConflict indicates a uniqueness violation — for connections, that the
	// project already holds a connection for this provider (singleton). Maps to
	// 409.
	ErrConflict = errors.New("resource already exists")

	// ErrStaleApproval indicates the approve→dispatch guard fired: the brief was
	// no longer approved at the expected version when the job was created (a
	// concurrent replace/archive committed in the window, or it lost approval).
	// It is distinct from ErrConflict (a uniqueness violation) because the client
	// remedy differs — refresh and re-approve, then retry — even though both map
	// to 409. Maps to 409.
	ErrStaleApproval = errors.New("brief is no longer approved at the expected version")

	// ErrPreconditionFailed indicates an optimistic-concurrency version
	// mismatch on a conditional update (stale If-Match). Maps to 412.
	ErrPreconditionFailed = errors.New("version precondition failed")

	// ErrToggleUnsupported indicates the campaign's platform has no status-toggle
	// capability wired (no dispatcher, or the dispatcher is not a StatusToggler).
	// The platform is never contacted. Maps to 400. Lives here (not in the service
	// layer) so a platform dispatcher can return it directly without importing the
	// orchestration layer, and the service still maps it to an HTTP status.
	ErrToggleUnsupported = errors.New("status toggle is not supported for this platform")

	// ErrCampaignNotProvisioned indicates a status toggle was requested on a
	// campaign that is not fully provisioned for the requested change — no upstream
	// platform campaign id yet, or (on ACTIVATE) a missing child ad group/ad so the
	// tree cannot be made servable. It is a client/state error: the platform is
	// never contacted, and a retry now would fail the same way. Maps to 409.
	ErrCampaignNotProvisioned = errors.New("campaign is not fully provisioned for this status change")

	// ErrMetricsUnsupported indicates the campaign's platform has no metrics-read
	// capability wired (no dispatcher, or the dispatcher is not a MetricsReader).
	// The platform is never contacted. Maps to 400. Lives here for the same reason
	// as ErrToggleUnsupported: a platform dispatcher can return it directly without
	// importing the orchestration layer.
	ErrMetricsUnsupported = errors.New("metrics reads are not supported for this platform")

	// ErrMetricsWindowUnsupported indicates the requested window is one of the seven
	// closed model.MetricsWindow values but this platform's MetricsReader does not
	// support it (e.g. X Ads caps windows at 7 days and rejects last_30_days). This is
	// caller input, not an upstream failure — the platform is never contacted (X's
	// client validates before making a request) or the platform rejects synchronously.
	// Maps to 400, distinct from ErrMetricsUnsupported (platform has no MetricsReader
	// at all) — this platform IS a MetricsReader, just not for this window. A platform
	// adapter wraps its own typed "unsupported window" error with this sentinel
	// (%w) so the service layer can map it without importing every platform package.
	ErrMetricsWindowUnsupported = errors.New("this window is not supported for the campaign's platform")

	// ErrCampaignAccountMismatch indicates the campaign was created under one ad
	// account but the project's CURRENT connection for that platform resolves to a
	// different one. Platform campaign ids are unique only WITHIN an account, so an
	// account-scoped request issued under the wrong account is not merely unauthorized —
	// it is silently WRONG: the id most often matches nothing (indistinguishable from a
	// campaign with genuinely zero activity) and, on a collision, matches somebody
	// else's campaign. The platform is never contacted. It is a state error, not a
	// transport one — a retry now fails identically — so it maps to 409, not 503.
	ErrCampaignAccountMismatch = errors.New("the campaign belongs to a different ad account than the project's current connection")

	// ErrCampaignWriteInProgress indicates another writer already holds the claim for this
	// campaign, so this request did not acquire it. Maps to 409.
	//
	// This is why the claim is a TRY and not a wait. The winning claim is held across the
	// ad-platform call — up to 45 seconds — and it holds a pooled connection for that whole
	// span. A blocking pg_advisory_lock would make every loser hold a SECOND pooled
	// connection for the same span, so a small burst against one campaign could exhaust a
	// finite pool and stall unrelated requests and the readiness probe. Failing fast keeps
	// contention costing one connection per campaign rather than one per request.
	//
	// Distinct from ErrPreconditionFailed: the caller's ETag may be perfectly current. The
	// correct client response is to retry shortly, not to refetch and rebuild the request.
	ErrCampaignWriteInProgress = errors.New("another write to this campaign is already in progress")

	// ErrAccountsUnsupported indicates the platform has no account-listing capability
	// wired (no dispatcher, or the dispatcher is not an AccountLister). The platform is
	// never contacted. Maps to 400. Lives here for the same reason as ErrToggleUnsupported:
	// a platform dispatcher can return it directly without importing the orchestration layer.
	ErrAccountsUnsupported = errors.New("account discovery is not supported for this platform")
)

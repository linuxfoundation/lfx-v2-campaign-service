// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// VerificationState is the outcome of testing a stored credential against its provider.
//
// It is a THREE-state vocabulary on purpose. A boolean cannot carry this outcome without
// conflating two opposite operator actions: "the provider rejected your credential"
// (re-authenticate) and "we could not reach the provider" (do NOT re-authenticate — the
// credential may be perfectly good). Collapsing those is the same one-value-two-meanings
// defect that has already caused silent data loss on this stack, where a single value meant
// both "disabled" and "transiently unavailable" and was used for a permanent decision.
type VerificationState string

// Verification states.
const (
	// VerificationVerified: the provider was CONTACTED and ACCEPTED the credential, scoped to
	// the account the connection names. This is the only state that proves the connection
	// works.
	VerificationVerified VerificationState = "verified"

	// VerificationInvalid: the provider was CONTACTED and REJECTED the credential (or rejected
	// this credential's access to the configured account). ACTIONABLE — the operator must
	// re-authenticate or fix the account id. A retry without changing anything fails the same
	// way.
	VerificationInvalid VerificationState = "invalid"

	// VerificationUnverifiable: the credential's validity is UNKNOWN. Two causes, deliberately
	// sharing one state because the operator action is identical for both (wait/retry, do NOT
	// touch the credential):
	//   1. the provider could not be reached, or answered ambiguously (5xx, timeout, throttle);
	//   2. no verifier is wired for this provider yet.
	//
	// This is explicitly NOT a credential problem. Treating it as one sends an operator to
	// re-authenticate a working credential during a provider outage, which is strictly worse
	// than doing nothing.
	VerificationUnverifiable VerificationState = "unverifiable"
)

// Valid reports whether s is one of the three defined states. Used to fail closed if a future
// verifier returns an unrecognized value rather than letting it reach a caller that branches
// on the state.
func (s VerificationState) Valid() bool {
	switch s {
	case VerificationVerified, VerificationInvalid, VerificationUnverifiable:
		return true
	default:
		return false
	}
}

// VerificationResult is what a CredentialVerifier reports.
//
// Reason is REQUIRED for every non-verified state and must name WHICH system failed — the
// provider, or this service — so an operator is never sent to the wrong one. A bare
// "verification failed" is what this whole type exists to avoid.
type VerificationResult struct {
	State  VerificationState
	Reason string
}

// CredentialVerifier is an OPTIONAL dispatcher capability: check the project's stored
// credential against the provider without mutating anything.
//
// It is optional for the same reason StatusToggler is (see internal/service/orchestrator.go):
// not every provider has one yet, and a verifier must NOT be written against a guessed API
// contract. A dispatcher that does not implement this yields VerificationUnverifiable, which
// correctly tells the operator "we don't know" rather than inventing a verdict.
//
// Implementations MUST:
//   - be READ-ONLY and account-scoped — verify against the account the connection names, not
//     merely that the token belongs to some tenant. A token valid for the wrong ad account is
//     exactly the misconfiguration this endpoint exists to catch, and a tenant-scoped probe
//     would pass it.
//   - return VerificationInvalid ONLY when the provider definitively rejected the credential.
//     Anything ambiguous (5xx, timeout, throttle, transport failure) is VerificationUnverifiable.
//   - never return an error alongside a state; classification failures are themselves
//     VerificationUnverifiable with a reason.
type CredentialVerifier interface {
	VerifyCredential(ctx context.Context, projectID string, provider model.Provider) VerificationResult
}

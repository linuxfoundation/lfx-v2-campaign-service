// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// genericDispatchFailure is the job result's `error` for a dispatch failure nothing below
// can classify. Before issue #208 it was the ONLY value: every dispatch failure, whatever
// the service knew about it, reached the operator as this one string.
const genericDispatchFailure = "platform campaign creation failed"

// DispatchFailureMessage is the per-platform `error` of an async create job whose dispatch
// failed. Exported only so internal/dispatch's tests can run REAL dispatcher errors through it
// (that package imports this one, so it cannot be the other way round); the orchestrator is
// its one production caller. It says WHY, from what the service authored or classified, and never from the
// error's text: a dispatcher error can carry an upstream response body (Meta's error text
// falls back to the raw body), a request URL or query (a destination URL carries the
// caller's secret-bearing query parameters), or credential-derived bytes (one connection
// defect is detected by decoding the DECRYPTED blob). So:
//
//   - a service-authored pre-flight refusal (domain.PreflightRefusal) is copied verbatim —
//     the one text this function echoes, because the constructor is the trust boundary;
//   - everything else is a fixed sentence per class, completed only by values this service
//     owns: the provider name, a reason token from unusableConnectionReason's fixed
//     vocabulary, and an HTTP status number.
//
// preCreate is the ORCHESTRATOR's verdict — the dispatcher proved nothing was created and the
// claim was released — and it is taken, never recomputed. This function changes the message
// and nothing else: whether the claim was released or retained, the campaign row's status and
// the job's aggregate are decided before it runs and are exactly what they were before it
// existed. A retained (possibly created) failure is reported as UNCONFIRMED whatever its chain
// carries, including a pre-flight refusal: a dispatcher that returned a partial alongside the
// error said something may exist, and that outranks any message.
//
// The connection arms come BEFORE the refusal arm and mirror classifyDiscoveryError's order
// (internal/service/connection.go), for the same reasons: a defect in this service is not a
// connection fault; a missing LF system row is not the project's to fix; and the system arm
// precedes the broad not-usable arm because systemScoped wraps rather than replaces.
func DispatchFailureMessage(p model.Provider, derr error, preCreate bool) string {
	if !preCreate {
		// A retained failure is still UNCONFIRMED by default, but "something may have been
		// created" is the least that can be said and is wrong for most of the outcomes that
		// reach here: a budget-name collision created nothing at all, and a refused campaign
		// after a created budget is not an unknown state but a known, specific one. Where
		// the dispatcher classified the outcome, say which it was.
		//
		// Each arm is a FIXED sentence chosen by the token, never built from it, so the
		// message still cannot carry upstream text (#208) whatever the carrier holds. An
		// unrecognised token falls through to the status/unconfirmed wording below.
		if outcome, ok := domain.UpstreamCreateOutcome(derr); ok {
			switch outcome {
			case domain.CreateOutcomeBudgetNameTaken:
				return fmt.Sprintf("%s campaign creation was refused because a budget of the name this service composes for this event already exists; nothing new was created. This service tried to reuse that budget and could not confirm it is safe to — it may be shared, already carry a campaign, or hold a different amount — so the retry stays blocked until someone resolves it on the platform; an unattached non-shared budget is not listed in the shared budget library, so find it through the campaign it carries or the platform's change history", p)
			case domain.CreateOutcomeCampaignNameTaken:
				return fmt.Sprintf("%s campaign creation was refused because a campaign of the name this service composes for this event already exists; a budget for this attempt EXISTS, either created by it or reused from an earlier attempt — verify both on the platform before retrying", p)
			case domain.CreateOutcomeBudgetCreatedCampaignRefused:
				return fmt.Sprintf("%s created or reused the campaign budget and then refused the campaign itself; no campaign exists and the budget is unattached, so it accrues no spend — a retry of this same brief will reuse that budget rather than be blocked by its name, provided it is still unattached, non-shared and otherwise unchanged", p)
			case domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed:
				return fmt.Sprintf("%s created or reused the campaign budget; whether the campaign was created could not be confirmed — verify on the platform before retrying", p)
			case domain.CreateOutcomeCampaignCreatedSetupIncomplete:
				// "could not be confirmed", not "did not finish": the last mutate of the
				// cascade can answer 2xx with a response this client cannot read
				// (pmax_creative.go's assetGroupAssets:mutate, demandgen_creative.go's
				// adGroupAds:mutate), and those arms return a campaign partial whose own
				// text says the resources MAY exist. The campaign's existence is proven by
				// the returned id; the state of its setup is not.
				return fmt.Sprintf("%s created the campaign but could not confirm its setup finished; it may be partly configured and not ready to run — verify on the platform before retrying", p)
			case domain.CreateOutcomeUnconfirmed:
				// Deliberately no sentence of its own: this token means exactly what the
				// status/unconfirmed wording below already says, and a second copy here
				// would be one more sentence to keep in step with it.
			}
		}
		// Status only, never the body, method or path.
		if status, ok := domain.UpstreamStatus(derr); ok {
			return fmt.Sprintf("%s campaign creation could not be confirmed (the platform answered HTTP %d); something may have been created — verify on the platform before retrying", p, status)
		}
		return fmt.Sprintf("%s campaign creation could not be confirmed; something may have been created — verify on the platform before retrying", p)
	}
	switch {
	case errors.Is(derr, domain.ErrServiceDefect):
		return fmt.Sprintf("%s campaign creation was stopped before anything was sent by a defect in this service; the connection is not at fault", p)
	case errors.Is(derr, domain.ErrSystemConnectionMissing):
		return fmt.Sprintf("the LF system %s connection is not installed; nothing was sent", p)
	case errors.Is(derr, domain.ErrConnectionAbsent):
		return fmt.Sprintf("no %s connection is configured for this project; nothing was sent", p)
	case errors.Is(derr, domain.ErrCredentialDecryptionFailed):
		return fmt.Sprintf("the stored %s credentials could not be decrypted; nothing was sent", p)
	case errors.Is(derr, domain.ErrSystemConnectionNotUsable):
		return fmt.Sprintf("the LF system %s connection is not usable as configured%s; nothing was sent", p, reasonSuffix(derr))
	case errors.Is(derr, domain.ErrConnectionNotUsable):
		return fmt.Sprintf("this project's %s connection cannot be used as configured%s; nothing was sent", p, reasonSuffix(derr))
	}
	if msg, ok := domain.AsPreflightRefusal(derr); ok {
		return msg
	}
	// A refresh that ended because THIS call was cancelled or ran out of time says nothing
	// about the credential, so "may need re-authorizing" would send an operator to repair a
	// connection that may be fine. It falls through to the generic string instead.
	if domain.IsTokenRefreshFailure(derr) && !errors.Is(derr, context.Canceled) && !errors.Is(derr, context.DeadlineExceeded) {
		return fmt.Sprintf("%s token refresh failed, so nothing was sent; the connection may need re-authorizing", p)
	}
	if status, ok := domain.UpstreamStatus(derr); ok {
		// The status may come from a READ that preceded the create (a find-existing lookup, a
		// geo resolve, a portal GET), not from the create itself, so neither sentence claims
		// the create was refused. 5xx and 429 are the platform declining to answer, which a
		// retry can clear; any other status is the platform refusing the request.
		if status >= 500 || status == http.StatusTooManyRequests {
			return fmt.Sprintf("%s answered HTTP %d before anything was created (a retry may succeed); nothing was created", p, status)
		}
		return fmt.Sprintf("%s refused the request (HTTP %d) before anything was created; nothing was created", p, status)
	}
	return genericDispatchFailure
}

// reasonSuffix renders unusableConnectionReason's token as " (reason: <token>)", or nothing
// when the chain carries no reason sentinel — "unclassified" would read as a diagnosis.
func reasonSuffix(err error) string {
	r := unusableConnectionReason(err)
	if r == "" || r == "unclassified" {
		return ""
	}
	return " (reason: " + r + ")"
}

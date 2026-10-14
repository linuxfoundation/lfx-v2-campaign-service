// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"errors"
	"fmt"

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
	if domain.IsTokenRefreshFailure(derr) {
		return fmt.Sprintf("%s token refresh failed, so nothing was sent; the connection may need re-authorizing", p)
	}
	if status, ok := domain.UpstreamStatus(derr); ok {
		return fmt.Sprintf("%s refused the create (HTTP %d); nothing was created", p, status)
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

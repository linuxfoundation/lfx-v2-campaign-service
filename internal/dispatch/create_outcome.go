// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// createOutcomeError tags a partial-result create failure with what the failed create left in
// the ad account, so service.DispatchFailureMessage can say which of several very different
// states the operator is actually in without reading a word of the error's text.
//
// Error() is the wrapped error's text UNCHANGED and Unwrap keeps the chain intact, so every
// errors.Is/As a caller already makes through this error — the platform client's *apiError and
// the HTTP status it now carries, a context.Canceled — still resolves. This type adds a
// question to the error, it does not replace it.
type createOutcomeError struct {
	err     error
	outcome string
}

func (e *createOutcomeError) Error() string                 { return e.err.Error() }
func (e *createOutcomeError) Unwrap() error                 { return e.err }
func (e *createOutcomeError) UpstreamCreateOutcome() string { return e.outcome }

// withCreateOutcome tags err with outcome. A nil err or an empty outcome returns err
// unchanged, so a classifier that declined to classify adds no carrier for a reader to find.
func withCreateOutcome(err error, outcome string) error {
	if err == nil || outcome == "" {
		return err
	}
	return &createOutcomeError{err: err, outcome: outcome}
}

// Compile-time proof the tag is readable through domain's helper, which is the only way any
// caller reaches it.
var _ domain.UpstreamCreateOutcomeCarrier = (*createOutcomeError)(nil)

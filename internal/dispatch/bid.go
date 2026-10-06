// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

// Shared outcome types for the BidWriter adapters (microsoft_bid.go, reddit_bid.go). They are
// the bid lever's counterparts of unconfirmedBudgetWriteError and rejectedBudgetAmountError, and
// are separate types rather than reuses because the service detects the amount reason through a
// BEHAVIOURAL interface named for the lever (BidAmountReason), and an error carrying the budget
// method would be read by nothing on this path.

// unconfirmedBidWriteError marks a bid write whose outcome is unknowable — the mutate was sent
// and may have applied. The service detects it through Unconfirmed() and answers 503 "verify
// before retrying", holding the claim lock for the cooldown.
type unconfirmedBidWriteError struct{ err error }

func (e *unconfirmedBidWriteError) Error() string {
	return "bid write outcome is unconfirmed (it may have been applied): " + e.err.Error()
}
func (e *unconfirmedBidWriteError) Unwrap() error     { return e.err }
func (e *unconfirmedBidWriteError) Unconfirmed() bool { return true }

// rejectedBidAmountError marks a bid write refused because of the REQUESTED AMOUNT, with the
// platform unchanged, and carries the one sentence safe to hand back to the caller. err wraps
// domain.ErrBidAmountRejected; reason is the platform client's own client-safe sentence, never
// the rendered chain.
type rejectedBidAmountError struct {
	reason string
	err    error
}

func (e *rejectedBidAmountError) Error() string { return e.err.Error() }
func (e *rejectedBidAmountError) Unwrap() error { return e.err }

// BidAmountReason returns the client-safe explanation of why the bid was refused.
func (e *rejectedBidAmountError) BidAmountReason() string { return e.reason }

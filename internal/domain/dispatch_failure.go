// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PreflightRefusal is a create refusal THIS SERVICE authored and raised before anything was
// sent to the ad platform: a missing conversion pixel, a budget under the platform minimum, an
// event name too long for the platform's name limit. Its message is written to be read by an
// operator, so it is the one dispatch failure the asynchronous create job (issue #208) may
// copy verbatim into its per-platform `error` — every other failure is reported there only by
// classification, because its text may carry an upstream response body, a URL, a query string
// or credential-derived bytes.
//
// That makes the constructors below a trust boundary, and the rule for using them is the
// whole of their contract:
//
//   - wrap ONLY a refusal raised before the first upstream request of the operation. A
//     refusal raised after a request was sent describes something that may have happened, and
//     is not this type's to report;
//   - the message must be service-authored. Caller-supplied config scalars (a date, a count, a
//     two-letter geo code) are fine; an upstream body, a URL (registration, image or post URLs
//     can carry a caller's secret-bearing query parameters), a token or a credential field
//     never are.
//
// Error() returns the message unchanged, so wrapping an existing refusal changes no log line
// and no string a test pins. Unwrap returns the wrapped cause (nil for a fresh refusal), so
// errors.Is/As against an existing sentinel — ErrBudgetAmountInvalid, say — still holds.
type PreflightRefusal struct {
	msg   string
	cause error
}

// Error returns the operator-safe message.
func (e *PreflightRefusal) Error() string { return e.msg }

// Unwrap returns the refusal's cause, if it wraps one.
func (e *PreflightRefusal) Unwrap() error { return e.cause }

// OperatorMessage is the text that may be surfaced to an operator. It is Error() under a name
// that says why a caller is reading it.
func (e *PreflightRefusal) OperatorMessage() string { return e.msg }

// Deliberately NO NoUpstreamCreate method. Whether a failed create released or retained its
// claim is the dispatcher's call (its notCreated wrapper) and must not change because a
// message became visible: a refusal found anywhere in a chain the dispatcher reported as
// possibly-created must not quietly turn that chain into a releasable one.

// elidedErrorArg replaces an error passed as a format argument. An error's text is the one
// argument shape that can carry an upstream body, so the formatter refuses to render it
// rather than trusting every call site to remember. A refusal that must keep a cause uses
// WrapPreflightRefusal, whose caller vouches for the cause's text explicitly.
const elidedErrorArg = "(detail withheld)"

// PreflightRefusalf builds a fresh refusal from a service-authored format string. An argument
// that is an error is rendered as a fixed placeholder, never as its text (see
// elidedErrorArg), and %w is not supported — use WrapPreflightRefusal to keep a cause.
func PreflightRefusalf(format string, args ...any) error {
	safe := make([]any, len(args))
	for i, a := range args {
		if _, isErr := a.(error); isErr {
			safe[i] = elidedErrorArg
			continue
		}
		safe[i] = a
	}
	return &PreflightRefusal{msg: fmt.Sprintf(format, safe...)}
}

// WrapPreflightRefusal marks an existing, service-authored refusal as operator-safe, keeping
// it as the cause so errors.Is/As still find it. The caller vouches that err's ENTIRE text
// (including anything it wraps) is service-authored and was produced before any upstream
// request. A nil err returns nil.
func WrapPreflightRefusal(err error) error {
	if err == nil {
		return nil
	}
	return &PreflightRefusal{msg: err.Error(), cause: err}
}

// AsPreflightRefusal reports the operator-safe message of the first PreflightRefusal in err's
// chain, and whether there was one.
func AsPreflightRefusal(err error) (string, bool) {
	var r *PreflightRefusal
	if errors.As(err, &r) {
		return r.msg, true
	}
	return "", false
}

// maxCallerValueRunes bounds how much of one caller-supplied value a refusal may quote.
const maxCallerValueRunes = 64

// callerValueWithheld replaces a caller value that is not shown.
const callerValueWithheld = "(value withheld)"

var (
	// callerValueCharset is the WHOLE alphabet a shown caller value may use. It is an
	// allowlist, not a list of dangerous shapes: every shape that can carry a secret — a URL
	// or protocol-relative URL, any path ("/SECRET", "reset/SECRET"), a query or bare
	// "k=v&k=v", userinfo, a fragment, an escaped or percent-encoded payload, whitespace, a
	// control character, quotes — needs a character outside it, so none needs its own rule.
	callerValueCharset = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	opaqueTokenRun     = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)
	canonicalUUID      = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
)

// CallerValue renders a caller-supplied scalar (a date, an id, a URN) for a refusal that may
// reach the job result verbatim (PreflightRefusal). Caller input is not upstream text, but it
// is not ours either: an "id" field can hold a pasted URL, path or query carrying a token.
//
// It is a strict ALLOWLIST (issue #208, after a blocklist kept missing shapes):
//
//   - the value is shown only if every character is in [A-Za-z0-9._:-] and it holds no opaque
//     token — a run of 32+ characters from [A-Za-z0-9_-] that includes a digit and is not a
//     canonical UUID (a base64url or hex secret fits the alphabet, so it needs this one extra
//     rule). Anything else is "(value withheld)", never partially shown;
//   - a shown value is cut to maxCallerValueRunes ("…" plus a trailing " (truncated)", so a cut
//     value cannot pass for a whole one) and quoted.
//
// Every legitimate shape the refusals quote fits: dates and the ISO instants derived from them
// ("2099-01-04", "2099-01-04T00:00:00Z"), numeric and alphanumeric account / tweet / pixel /
// list / customer / funding-instrument ids, "act_…" and "t2_…" ids, "urn:li:…" URNs, UUIDs,
// geo and match-type codes, objectives ("video_views"), video goals, targeting-profile and
// project slugs, connection statuses. A value outside it is, at these call sites, already
// invalid input — a CTA or keyword with a space, a MIME type (use MIMEValue) — so withholding
// it costs the operator only the echo of a value they supplied, and the refusal still says
// which field and why.
//
// Every caller-supplied value formatted into a PreflightRefusal must go through this or
// MIMEValue.
func CallerValue(s string) string {
	if s == "" {
		return `""`
	}
	if !callerValueCharset.MatchString(s) || hasOpaqueToken(s) {
		return callerValueWithheld
	}
	if n := len([]rune(s)); n > maxCallerValueRunes {
		return strconv.Quote(string([]rune(s)[:maxCallerValueRunes])+"…") + " (truncated)"
	}
	return strconv.Quote(s)
}

// imageMIMEs are the only MIME types an image refusal may quote verbatim: the formats the
// stored-asset store and the platform image validators deal in.
var imageMIMEs = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// MIMEValue renders a recorded or declared MIME type for an image refusal. A value in
// imageMIMEs is quoted; anything else goes through CallerValue, whose alphabet has no "/", so
// it is withheld. A dedicated helper rather than a "/" exception inside CallerValue keeps the
// one character a path needs out of every other call site.
func MIMEValue(s string) string {
	if imageMIMEs[s] {
		return strconv.Quote(s)
	}
	return CallerValue(s)
}

// hasOpaqueToken reports whether s contains a run shaped like a credential token (see
// CallerValue).
func hasOpaqueToken(s string) bool {
	for _, run := range opaqueTokenRun.FindAllString(s, -1) {
		if canonicalUUID.MatchString(run) {
			continue
		}
		if strings.ContainsAny(run, "0123456789") {
			return true
		}
	}
	return false
}

// UpstreamStatusCarrier is implemented by a platform client's HTTP error type: the platform
// answered, and this is the HTTP status it answered with. It exposes the STATUS ONLY — never
// the body, the method or the path — so a caller that reports it cannot leak what the
// response carried (Meta's error text, for one, falls back to the raw body).
type UpstreamStatusCarrier interface {
	UpstreamStatus() int
}

// UpstreamStatus returns the HTTP status of the first UpstreamStatusCarrier in err's chain.
func UpstreamStatus(err error) (int, bool) {
	var c UpstreamStatusCarrier
	if errors.As(err, &c) {
		if s := c.UpstreamStatus(); s > 0 {
			return s, true
		}
	}
	return 0, false
}

// UpstreamCreateOutcomeCarrier is implemented by an error from a create that failed AFTER the
// first upstream request of the operation was sent. It answers one question — what did the
// failed create leave in the ad account? — as a short, fixed-vocabulary token.
//
// It is the POST-request counterpart of PreflightRefusal, and deliberately a separate type:
// PreflightRefusal's contract is that it wraps only a refusal raised BEFORE the first upstream
// request, so reusing it here would make "nothing was created" claims about requests that were
// in fact sent.
//
// The token is a SERVICE-AUTHORED CONSTANT, never upstream text and never a caller-supplied
// value, so #208's rule that a dispatch-failure message is never built from the error's text
// still holds. A reader must switch on it and emit its own fixed sentence per case — never
// format the token into a message — so an unrecognised token can only fall through to the
// reader's existing wording, never put words in the operator's mouth.
type UpstreamCreateOutcomeCarrier interface {
	UpstreamCreateOutcome() string
}

// The create-outcome vocabulary. Platform-neutral on purpose: every platform this service
// dispatches to builds a campaign as a budget-then-campaign-then-setup cascade, so a second
// platform adopting the carrier reuses these rather than inventing a parallel set that
// DispatchFailureMessage would have to learn separately.
//
// Each token answers "what is in the ad account now?", not "which code path failed" — two arms
// that leave the same state share a token, and one arm that can leave two different states does
// not get to pick.
const (
	// CreateOutcomeBudgetNameTaken: nothing new was created. A budget of the name this
	// service composes already exists from an earlier attempt, and this attempt tried to
	// adopt it and did not get far enough to use it — the lookup failed, or what it read
	// back did not satisfy the reuse gates. A later retry may still succeed: the decline can
	// be transient (the lookup read failing) as well as a standing property of that budget.
	CreateOutcomeBudgetNameTaken = "budget_name_taken"
	// CreateOutcomeBudgetCreatedCampaignRefused: the budget for this brief exists — created
	// by this attempt, or an earlier attempt's budget of the same name adopted by it — and
	// the platform then definitely refused the campaign. The budget is an orphan: it exists,
	// carries no campaign, and accrues no spend. Which of the two put it there does not
	// change what is in the account or what to do about it, so both share this token.
	CreateOutcomeBudgetCreatedCampaignRefused = "budget_created_campaign_refused"
	// CreateOutcomeBudgetCreatedCampaignUnconfirmed: the budget for this brief exists
	// (created or adopted, as above); whether the campaign was created is genuinely unknown
	// (a 5xx, a 429, a mid-flight timeout).
	CreateOutcomeBudgetCreatedCampaignUnconfirmed = "budget_created_campaign_unconfirmed"
	// CreateOutcomeCampaignNameTaken: the budget for this brief exists (created or adopted,
	// as above) and a campaign of the composed name already exists from an earlier attempt.
	CreateOutcomeCampaignNameTaken = "campaign_name_taken"
	// CreateOutcomeCampaignCreatedSetupIncomplete: the campaign itself exists; a later step of
	// the cascade (ad group, ad, targeting, assets) did not report success, so whether its
	// setup finished is unknown — the last mutate can also answer 2xx unreadably, past which
	// the step DID commit. The campaign's existence is proven by its id; its setup is not.
	CreateOutcomeCampaignCreatedSetupIncomplete = "campaign_created_setup_incomplete"
	// CreateOutcomeUnconfirmed: the first create may or may not have committed and nothing
	// downstream was reached. The least that can be said, and the pre-existing wording.
	CreateOutcomeUnconfirmed = "create_unconfirmed"
)

// UpstreamCreateOutcome returns the outcome token of the first UpstreamCreateOutcomeCarrier in
// err's chain. An empty token reports false, so a carrier that could not classify the failure
// is indistinguishable from no carrier at all and leaves the reader on its default wording.
func UpstreamCreateOutcome(err error) (string, bool) {
	var c UpstreamCreateOutcomeCarrier
	if errors.As(err, &c) {
		if o := c.UpstreamCreateOutcome(); o != "" {
			return o, true
		}
	}
	return "", false
}

// TokenRefreshFailure is implemented by a platform client's OAuth token-exchange error: the
// access token this request needed could not be minted from the stored credential. The
// request it was for was therefore never sent.
type TokenRefreshFailure interface {
	TokenRefreshFailed() bool
}

// IsTokenRefreshFailure reports whether err's chain carries a TokenRefreshFailure.
func IsTokenRefreshFailure(err error) bool {
	var t TokenRefreshFailure
	return errors.As(err, &t) && t.TokenRefreshFailed()
}

// tokenRefreshFailure is the one TokenRefreshFailure implementation: a marker over the
// client's own token-exchange error. Error() is the wrapped error's text unchanged, and Unwrap
// keeps every errors.Is/As a caller already makes against it (a client's
// ErrCredentialRejected, context.Canceled) working.
type tokenRefreshFailure struct{ err error }

func (e *tokenRefreshFailure) Error() string            { return e.err.Error() }
func (e *tokenRefreshFailure) Unwrap() error            { return e.err }
func (e *tokenRefreshFailure) TokenRefreshFailed() bool { return true }

// MarkTokenRefreshFailure tags a platform client's token-exchange error so a caller can say
// "the token refresh failed" without reading — or reporting — its text. A nil err returns nil.
func MarkTokenRefreshFailure(err error) error {
	if err == nil {
		return nil
	}
	return &tokenRefreshFailure{err: err}
}

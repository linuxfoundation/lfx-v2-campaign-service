// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
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

// callerValueWithheld replaces a caller value that is shaped like something that can carry a
// secret.
const callerValueWithheld = "(value withheld)"

// CallerValue renders a caller-supplied scalar (a date, an id, a keyword) for a refusal that
// may reach the job result verbatim (PreflightRefusal). Caller input is not upstream text, but
// it is not ours either: an "id" field can hold a pasted URL whose query carries a token, and a
// value can be arbitrarily long or carry control characters. So, modelled on the dispatchers'
// safeAssetIDForError:
//
//   - a value is not shown at all when it is shaped like something that carries a secret:
//     it contains "://", "?", "#", "@", "=", "&" or any whitespace (a URL, a query — including
//     a bare one such as "access_token=…&scope=ads" — a userinfo credential, or a multi-line
//     paste), or it contains an opaque token: a run of 32 or more characters from
//     [A-Za-z0-9+/_-] that includes a digit and is not a canonical UUID (base64, base64url
//     and hex tokens). No value these refusals quote legitimately has either shape: dates,
//     numeric and short alphanumeric account/tweet/pixel/list ids, "urn:li:…" URNs (the
//     colons break the run), objectives and CTAs are all far shorter, and the UUIDs used as
//     asset and project ids are exempted by shape. A digit-free run (a long slug) is kept;
//   - otherwise it is cut to maxCallerValueRunes (marked with "…"), every non-graphic rune is
//     replaced with U+FFFD, and the result is quoted.
//
// Every caller-supplied value formatted into a PreflightRefusal must go through this.
func CallerValue(s string) string {
	if strings.Contains(s, "://") || strings.ContainsAny(s, "?#@=&") || strings.IndexFunc(s, unicode.IsSpace) >= 0 || hasOpaqueToken(s) {
		return callerValueWithheld
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxCallerValueRunes {
			b.WriteString("…")
			break
		}
		if !unicode.IsGraphic(r) {
			r = unicode.ReplacementChar
		}
		b.WriteRune(r)
		n++
	}
	return strconv.Quote(b.String())
}

var (
	opaqueTokenRun = regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}`)
	canonicalUUID  = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
)

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

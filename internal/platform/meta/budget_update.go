// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
)

// AdSetBudget is the live budget state of one Meta ad set AND of the campaign above it, as the
// platform reports it. It is the read half of the budget-write capability.
//
// IT DELIBERATELY CARRIES BOTH LEVELS. Meta is the one supported platform where a budget can
// live somewhere other than the object being written: with Campaign Budget Optimization (CBO)
// the amount sits on the CAMPAIGN and is distributed across every ad set under it, and the ad
// set's own budget fields are then absent. Writing an ad-set budget on such a campaign either
// fails or converts the campaign off CBO — in both cases changing the spend of ad sets this
// request never named. That is Meta's form of the shared-budget problem the Google Ads path
// refuses, so the campaign-level fields are read for the guard to refuse on, not for display.
//
// Every amount is in the ACCOUNT CURRENCY'S MINOR UNITS (cents for USD, whole yen for JPY) —
// the units Meta reports and accepts — and each is a POINTER because nil means "the platform
// did not report this field", which is NOT zero. A budget field read as zero would name the
// wrong budget level and the wrong pacing model.
type AdSetBudget struct {
	// AdSetID is the id the platform answered about, echoed back.
	AdSetID string
	// Status is the ad set's current status. Not a guard — a paused ad set's budget is
	// writable.
	Status string
	// DailyBudgetMinor / LifetimeBudgetMinor are the AD SET's own budget fields.
	DailyBudgetMinor    *int64
	LifetimeBudgetMinor *int64
	// CampaignID is the campaign the ad set belongs to, as reported.
	CampaignID string
	// CampaignDailyBudgetMinor / CampaignLifetimeBudgetMinor are the CAMPAIGN's budget
	// fields. A non-nil value here is CBO: the campaign owns the budget and the ad sets
	// share it.
	CampaignDailyBudgetMinor    *int64
	CampaignLifetimeBudgetMinor *int64
	// AmountUnparseable records that the platform reported a budget field this client could
	// not parse. Surfaced rather than swallowed: every guard downstream reasons about the
	// current budget, and reasoning about a value that failed to parse is reasoning about
	// zero — which would answer "no budget here" for a budget that exists.
	AmountUnparseable bool
}

// CampaignBudgetOptimized reports whether the CAMPAIGN above this ad set holds the budget
// (CBO). When it does, the amount is shared across every ad set under the campaign and an
// ad-set budget write would move spend this request never named.
func (b *AdSetBudget) CampaignBudgetOptimized() bool {
	return b.CampaignDailyBudgetMinor != nil || b.CampaignLifetimeBudgetMinor != nil
}

// ErrBudgetAmountInvalid marks every refusal budgetToMinorUnits makes, so a caller can tell
// "this amount is permanently unacceptable" apart from "the platform call failed" or "the
// account's currency could not be resolved".
//
// It exists because the distinction decides an HTTP status. Meta's floor is ONE MINOR UNIT in
// the account's own currency, a value nothing above this package can know — the service layer
// holds no per-platform floor by design — so without a sentinel this refusal reaches the
// service as a bare error, is classified as an upstream failure and answered 503 with an
// invitation to retry a request that can never succeed. The dispatcher maps this to domain's
// ErrBudgetAmountRejected, which is answered 400.
//
// SCOPE IS DELIBERATELY NARROW. resolveCurrencyOffset's failures (a failed account preflight,
// an unknown currency, a conflicting explicit offset) do NOT wrap it: none of them is a
// statement about the caller's amount, and a failed preflight in particular may well be
// transient. Blanket-wrapping ResolveBudgetMinorUnits would turn a transport failure into a
// 400 telling the caller their amount was wrong.
var ErrBudgetAmountInvalid = errors.New("meta: budget amount is not acceptable")

// budgetAmountError is the concrete type every amount refusal returns. It carries ONLY the
// validator's own sentence as its message and reaches ErrBudgetAmountInvalid through Unwrap,
// so errors.Is matches while the text stays a clean, client-safe sentence — wrapping the
// sentinel with %w instead would append its text to every message and make the sentence a
// caller can be shown indistinguishable from the chain it arrived in.
type budgetAmountError struct{ msg string }

func (e *budgetAmountError) Error() string { return e.msg }
func (e *budgetAmountError) Unwrap() error { return ErrBudgetAmountInvalid }

func invalidBudgetAmount(format string, args ...any) error {
	return &budgetAmountError{msg: fmt.Sprintf(format, args...)}
}

// BudgetAmountReason reports whether err came from this package's budget-amount validation
// and, if so, returns the validator's own sentence. It is the ONLY sanctioned way to turn one
// of these refusals into text shown to a caller: the sentence names the amount and Meta's
// published minimum and nothing about upstream account configuration, which is what makes it
// safe to return where the surrounding error chain is not.
func BudgetAmountReason(err error) (string, bool) {
	var amountErr *budgetAmountError
	if errors.As(err, &amountErr) {
		return amountErr.msg, true
	}
	return "", false
}

// ErrAccountCurrencyUnresolvable marks a budget write refused because the AD ACCOUNT's
// currency does not resolve to a minor-unit scale — Meta reported a code this service's
// supported-currency map does not carry, or a stored explicit offset contradicts it.
//
// It is a permanent property of that account, not a transient upstream failure and not a
// property of the requested amount, which is why it is neither ErrBudgetAmountInvalid nor left
// bare: the dispatcher maps it to domain.ErrBudgetUnwritable and it is answered as a settled
// refusal, rather than falling through to a 503 that invites a retry nothing can change.
//
// A failed account PREFLIGHT is deliberately not marked with it: that may well be transient.
var ErrAccountCurrencyUnresolvable = errors.New("meta: the ad account's currency has no known minor-unit scale")

// resolveCurrencyOffset derives the minor-unit multiplier used to encode a budget, from the
// account preflight's currency and the optionally-configured explicit offset.
//
// IT IS THE SINGLE SOURCE OF THIS PRECEDENCE FOR BOTH THE CREATE AND THE BUDGET-WRITE PATH.
// The rule and its error texts are carried over from the create path unchanged: the ACCOUNT
// CURRENCY is authoritative, a conflicting explicit override is REJECTED rather than trusted
// (a stale CurrencyOffset:100 on an account now denominated in JPY would encode the budget
// 100x too high), and the explicit offset is relied on only when the preflight failed or
// returned a currency this client does not know. Neither usable → fail, rather than guess 100.
//
// acctCurrency is the preflight's reported code and preflightErr its error; exactly one of
// them is meaningful, which is why both are parameters rather than a single result.
func (c *Client) resolveCurrencyOffset(acctCurrency string, preflightErr error) (int64, error) {
	offset := c.account.CurrencyOffset
	if offset == 0 {
		if preflightErr != nil {
			// Wrap with %w (not %s) so the underlying error chain is preserved and a
			// caller can errors.As it back to *APIError like other Graph failures — a
			// %s would flatten it to a string and break that unwrap.
			return 0, fmt.Errorf("meta: could not determine the account currency because the account preflight failed; set AccountConfig.CurrencyOffset explicitly (100 for most currencies, 1 for zero-decimal like JPY/KRW/CLP): %w", preflightErr)
		}
		derived, ok := currencyOffsetFor(acctCurrency)
		if !ok {
			return 0, fmt.Errorf("meta: account preflight returned an unsupported or missing currency code (got %q); it is not in the supported-currency map, so set AccountConfig.CurrencyOffset explicitly (100 for most currencies, 1 for zero-decimal like JPY/KRW/CLP) rather than assuming a default that could encode a zero-decimal budget 100x too high", acctCurrency)
		}
		return derived, nil
	}
	if preflightErr == nil {
		if derived, ok := currencyOffsetFor(acctCurrency); ok && derived != offset {
			return 0, fmt.Errorf("meta: AccountConfig.CurrencyOffset (%d) conflicts with the account's currency %q (correct offset %d) reported by the preflight; the account currency is authoritative — remove or correct the explicit offset to avoid encoding the budget with the wrong minor-unit scale", offset, acctCurrency, derived)
		}
	}
	return offset, nil
}

// budgetToMinorUnits converts a whole-account-currency amount to Meta minor units. It is NOT an
// FX conversion — the caller's amount is already in the account's currency.
//
// Shared by the create and the budget-write path so an amount this service would refuse to
// CREATE with cannot be reached by EDITING — the divergence the Google Ads and LinkedIn slices
// closed the same way.
//
// The conversion is EXACT (PR #316 review): amount×offset is computed in rational arithmetic
// from the amount's shortest round-tripping decimal form (minorUnitsExact), never as a binary
// float product. The decimal fraction check alone did not make the ENCODING exact —
// 90071992547253.9 × 100 passed it and was then written as 9007199254725391 rather than
// 9007199254725390. The exact product is the authority for every refusal:
//
//   - too large: above MaxExactMinorUnits (999,999,999,999,999 minor units — 15 digits; +Inf
//     too). The amount arrives as an already-decoded float64, which reproduces a decimal exactly
//     only up to 15 significant digits; past that the caller's digits may be gone before any
//     check runs — JSON 9007199254740992.4 decodes to 9007199254740992 and would pass as a whole
//     JPY amount (PR #316 review). See MaxExactMinorUnits for why the bound is 15 digits and not
//     2^53. This is the only budget-magnitude ceiling, and it sits well inside
//     int64, so the conversion can never overflow;
//   - too small: below one minor unit (zero and negatives included);
//   - finer than one minor unit (1.4 on a JPY account, 25.505 on a USD one): REFUSED before
//     anything is written, not rounded (#235).
//
// NaN is rejected explicitly and named for what it is.
// Every refusal wraps ErrBudgetAmountInvalid; the currency-offset failures above deliberately
// do NOT, because those are upstream account configuration, not the caller's amount.
func budgetToMinorUnits(budget float64, offset int64) (int64, error) {
	if math.IsNaN(budget) {
		return 0, invalidBudgetAmount("budget must be a finite number, got %v", budget)
	}
	switch minor, verdict := minorUnitsExact(budget, offset); verdict {
	case minorTooLarge:
		return 0, invalidBudgetAmount("budget too large to represent exactly: at most %d minor units of the ad account's currency (offset %d)", MaxExactMinorUnits, offset)
	case minorTooSmall:
		return 0, invalidBudgetAmount("budget too small: must be at least one minor currency unit (offset %d)", offset)
	case minorFractional:
		// A fraction of a minor unit is REFUSED, never rounded (#235): rounding 1.4 on a JPY
		// account to ¥1 and answering 200 is a silent change to money the caller never asked for.
		return 0, invalidBudgetAmount("budget %s is finer than the ad account currency's smallest unit; %s",
			strconv.FormatFloat(budget, 'f', -1, 64), wholeAmountsAdvice(offset))
	default:
		return minor, nil
	}
}

// minorVerdict is minorUnitsExact's classification of amount×offset.
type minorVerdict int

const (
	minorOK minorVerdict = iota
	minorTooLarge
	minorTooSmall
	minorFractional
)

// MaxExactMinorUnits is the largest minor-unit amount the Meta encoders accept:
// 999,999,999,999,999 (15 digits). The amount reaches this package already decoded into a
// float64, and a float64 round-trips every decimal of at most 15 significant digits exactly
// (DBL_DIG), so a valid amount — at most this many minor units, no finer than one — is the
// caller's own digits. 2^53 is NOT a sufficient bound once the offset is above 1: at USD scale
// 90071992547409.91 (9007199254740991 cents, below 2^53) decodes to 90071992547409.9 and would
// be written one cent short. The bound cannot make a request carrying more than 15 significant
// digits exact (1.0000000000000001 decodes to 1); that precision is lost in JSON decoding,
// before this service sees the amount.
const MaxExactMinorUnits = 999_999_999_999_999

var maxExactMinorRat = new(big.Rat).SetInt64(MaxExactMinorUnits)

// minorUnitsExact computes amount×offset EXACTLY and classifies it, in this order: above
// MaxExactMinorUnits (or +Inf) is minorTooLarge — checked first, because past it the fraction
// check below cannot be trusted — below 1 (or -Inf, zero, negative) is minorTooSmall,
// a non-integer is minorFractional; otherwise minorOK with the product. The amount is taken as
// its shortest round-tripping decimal form — the digits the caller sent, since a JSON number is
// parsed from exactly that text — so 19.99 is 1999 minor units although 19.99*100 is
// 1998.9999999999998 as a float, and 1.005 is fractional. NaN is the caller's to refuse first; it
// is reported minorTooSmall here. offset must be positive (resolveCurrencyOffset guarantees it).
func minorUnitsExact(amount float64, offset int64) (int64, minorVerdict) {
	switch {
	case math.IsInf(amount, 1):
		return 0, minorTooLarge
	case math.IsNaN(amount) || math.IsInf(amount, -1) || offset <= 0:
		return 0, minorTooSmall
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(amount, 'f', -1, 64))
	if !ok {
		return 0, minorTooSmall // unreachable: 'f' formatting of a finite float always parses
	}
	r.Mul(r, new(big.Rat).SetInt64(offset))
	switch {
	case r.Cmp(maxExactMinorRat) > 0:
		return 0, minorTooLarge
	case r.Cmp(big.NewRat(1, 1)) < 0:
		return 0, minorTooSmall
	case !r.IsInt():
		return 0, minorFractional
	}
	return r.Num().Int64(), minorOK
}

// MinorUnitsExact is minorUnitsExact for callers outside this package that must encode a
// recorded amount exactly as the create and write paths do (the settings readback). ok=false
// when the encoders would refuse it.
func MinorUnitsExact(amount float64, offset int64) (int64, bool) {
	minor, verdict := minorUnitsExact(amount, offset)
	return minor, verdict == minorOK
}

// decimalPlaces returns n for offset == 10^n (n >= 0), ok=false otherwise.
func decimalPlaces(offset int64) (int, bool) {
	if offset < 1 {
		return 0, false
	}
	n := 0
	for offset > 1 {
		if offset%10 != 0 {
			return 0, false
		}
		offset /= 10
		n++
	}
	return n, true
}

// wholeAmountsAdvice is the client-safe remedy sentence for a fractional-minor-unit refusal.
func wholeAmountsAdvice(offset int64) string {
	if places, ok := decimalPlaces(offset); ok && places > 0 {
		return fmt.Sprintf("enter at most %d decimal places for this currency", places)
	}
	return "enter whole amounts for this currency"
}

// ResolveBudgetMinorUnits runs the account preflight and converts a whole-account-currency
// budget into the minor-unit integer Meta accepts, returning the amount and the offset used.
//
// It is a PURE READ plus arithmetic and mutates nothing, so its failure is always definite. It
// exists so the budget-write path encodes an amount by exactly the rule the create path uses,
// against the SAME authoritative source — the account's own currency — rather than assuming a
// scale. An unknown currency fails HERE, before anything is written.
//
// IT CANNOT FALL BACK TO AN EXPLICIT OFFSET, and that is a real limitation rather than an
// oversight. The budget path's client is built from the connection row alone, so
// AccountConfig.CurrencyOffset is always zero here — the value only ever reaches the client on
// a CREATE, from that request's own per-campaign config, and it is persisted nowhere the write
// path could read it back. The consequence is narrow but real: a project whose ad account is
// denominated in a currency Meta reports but this service's map does not carry can be CREATED
// with an explicit offset and can never have its budget EDITED here. That is the fail-closed
// side of the trade — the alternative is encoding an amount at a guessed scale, which is how a
// budget gets written 100x wrong — and the refusal says so without advising a setting this
// endpoint has no way to accept.
//
// Unlike the create path this does NOT gate on account_status: an ad account in a non-serving
// state still has a real budget, and LOWERING it is a reasonable thing to do on an account
// under review. Refusing here would block the one action that can reduce exposure.
func (c *Client) ResolveBudgetMinorUnits(ctx context.Context, budget float64) (int64, int64, error) {
	accountID := strings.TrimSpace(c.account.AccountID)
	if accountID == "" {
		return 0, 0, fmt.Errorf("meta: an ad account must be selected to write a budget: the account's currency determines the minor-unit scale the amount is encoded in, and it cannot be assumed")
	}
	var acct accountPreflight
	preflightErr := c.doRequest(ctx, http.MethodGet, "/"+accountID+"?fields=name,account_status,currency", nil, &acct)
	if preflightErr != nil && ctx.Err() != nil {
		return 0, 0, fmt.Errorf("meta budget write aborted during account preflight: %w", ctx.Err())
	}
	offset, err := c.resolveCurrencyOffset(acct.Currency, preflightErr)
	if err != nil {
		// resolveCurrencyOffset's texts are written for the CREATE path, which is handed an
		// AccountConfig built from the caller's own per-campaign config and can therefore
		// act on "set CurrencyOffset explicitly". THIS path cannot: its client is built from
		// the connection row alone (see the dispatcher's cachedMetaClient), the offset is
		// not carried on the row or on the campaign, and there is no request field that
		// reaches here — so repeating that advice would send an operator to a setting this
		// endpoint has no way to read. The underlying error is still wrapped, for the log.
		//
		// The two causes are classified differently on purpose. A failed preflight may be
		// transient, so it stays an ordinary failure. Everything else — a currency Meta
		// reports that is not in the supported map, or a stored offset conflicting with it —
		// is a permanent property of the ad account, so it is marked unresolvable and
		// answered as a settled refusal rather than as a retryable upstream error.
		if preflightErr != nil {
			return 0, 0, fmt.Errorf("meta: the ad account's currency could not be determined because the account preflight failed, and the minor-unit scale a budget is encoded in cannot be assumed: %w", err)
		}
		return 0, 0, fmt.Errorf("meta: the ad account's currency does not resolve to a minor-unit scale this service can encode a budget in, so the amount cannot be written safely; the remedy is in Meta Ads Manager or in this service's supported-currency map, not in the request (%w): %w", err, ErrAccountCurrencyUnresolvable)
	}
	minor, err := budgetToMinorUnits(budget, offset)
	if err != nil {
		return 0, 0, fmt.Errorf("meta: %w", err)
	}
	return minor, offset, nil
}

// GetAdSetBudget reads one ad set's live budget state AND the budget state of the campaign
// above it. It is a PURE READ and mutates nothing, so its failure is always definite — a caller
// must not classify it as an unconfirmed write outcome, because no write was built.
//
// The campaign's budget fields are fetched in the SAME request via field expansion rather than
// a second call: two calls could observe the two levels at different moments, and a CBO budget
// that appeared between them would be missed by exactly the guard that exists to catch it.
//
// The response must echo the requested ad set id; one naming another ad set, or naming none,
// is refused.
func (c *Client) GetAdSetBudget(ctx context.Context, adSetID string) (*AdSetBudget, error) {
	adSetID = strings.TrimSpace(adSetID)
	if adSetID == "" {
		return nil, fmt.Errorf("meta: ad set id is required")
	}
	if !numericIDRE.MatchString(adSetID) {
		return nil, fmt.Errorf("meta: invalid ad set id %q: must be numeric", adSetID)
	}

	var resp struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		DailyBudget    string `json:"daily_budget"`
		LifetimeBudget string `json:"lifetime_budget"`
		CampaignID     string `json:"campaign_id"`
		Campaign       *struct {
			ID             string `json:"id"`
			DailyBudget    string `json:"daily_budget"`
			LifetimeBudget string `json:"lifetime_budget"`
		} `json:"campaign"`
	}
	// The braces of the field-expansion term are percent-encoded: this path is concatenated
	// onto the base URL and parsed as a whole, and a literal brace in a query is not a
	// portable thing to rely on.
	const fields = "id,status,daily_budget,lifetime_budget,campaign_id,campaign%7Bid,daily_budget,lifetime_budget%7D"
	if err := c.doRequest(ctx, http.MethodGet, "/"+adSetID+"?fields="+fields, nil, &resp); err != nil {
		return nil, fmt.Errorf("meta: read ad set %s budget: %w", adSetID, err)
	}
	// The platform must have answered about the ad set that was asked for. A response
	// describing a different id is not a budget this caller may act on. A response with NO id
	// (absent, null or empty) is refused too: it proves nothing about which ad set it
	// describes, so its fields — an absent daily_budget read as "this ad set does not use it",
	// an absent campaign budget read as "not CBO" — would drive the dispatcher's guards from an
	// answer about nothing in particular. GetAdSetBid reads the same object and already
	// requires the echo.
	if got := resp.ID; got != adSetID {
		if got == "" {
			got = "no ad set id"
		} else {
			got = "ad set " + got
		}
		return nil, fmt.Errorf("meta: read ad set %s budget returned %s instead", adSetID, got)
	}

	out := &AdSetBudget{AdSetID: adSetID, Status: resp.Status, CampaignID: resp.CampaignID}
	out.DailyBudgetMinor = parseMinorUnits(resp.DailyBudget, &out.AmountUnparseable)
	out.LifetimeBudgetMinor = parseMinorUnits(resp.LifetimeBudget, &out.AmountUnparseable)
	if resp.Campaign != nil {
		if out.CampaignID == "" {
			out.CampaignID = resp.Campaign.ID
		}
		out.CampaignDailyBudgetMinor = parseMinorUnits(resp.Campaign.DailyBudget, &out.AmountUnparseable)
		out.CampaignLifetimeBudgetMinor = parseMinorUnits(resp.Campaign.LifetimeBudget, &out.AmountUnparseable)
	}
	return out, nil
}

// parseMinorUnits reads one Meta budget field, which is reported as a DECIMAL STRING of minor
// units ("1000" = $10.00 on a USD account), not a number.
//
// An absent or empty field yields nil — "not reported" — which is the shape every guard
// upstream reads as "this level holds no budget". A field that is present but does NOT parse
// yields nil AND sets the unparseable flag, so the caller refuses rather than concluding the
// level is empty: a budget that exists and could not be read is the one case where "nil" would
// be an actively wrong answer.
//
// Meta documents these as integers. A value carrying a fraction is still refused rather than
// truncated: truncation is a silent change to money.
func parseMinorUnits(raw string, unparseable *bool) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		*unparseable = true
		return nil
	}
	return &v
}

// UpdateAdSetBudget sets an ad set's budget amount on the platform. lifetime selects which of
// the two mutually exclusive fields is written (lifetime_budget vs daily_budget); the caller is
// responsible for having established that this matches the ad set's CURRENT pacing model and
// that the campaign above it is not CBO — this function writes what it is told.
//
// budgetMinor must be the value ResolveBudgetMinorUnits produced. Taking the ENCODED INTEGER
// rather than a major-unit float is deliberate: it makes it impossible for a caller to validate
// one value and send another, the same contract the LinkedIn wire string carries.
//
// A lifetime budget requires an end_time on the ad set. None is sent, and none is needed: this
// path only ever writes lifetime_budget on an ad set that ALREADY reports one, which therefore
// already has its end_time. Sending a schedule field here would change the flight as a side
// effect of an amount change.
//
// Errors are returned UNWRAPPED for the caller to classify with IsOutcomeUnconfirmed. This
// function must never decide that a failure was definite: a transport failure, 3xx, 429 or 5xx
// on a budget mutate may have applied the new amount upstream.
func (c *Client) UpdateAdSetBudget(ctx context.Context, adSetID string, budgetMinor int64, lifetime bool) error {
	adSetID = strings.TrimSpace(adSetID)
	if adSetID == "" {
		return fmt.Errorf("meta: ad set id is required")
	}
	if !numericIDRE.MatchString(adSetID) {
		return fmt.Errorf("meta: invalid ad set id %q: must be numeric", adSetID)
	}
	// A construction guard, not a revalidation: it catches a caller that computed its own
	// minor-unit value and bypassed the range and floor checks, which is the one way the
	// shared-encoder contract can be broken.
	if budgetMinor < 1 {
		return fmt.Errorf("meta: budget %d minor units is not a valid amount; it must come from ResolveBudgetMinorUnits", budgetMinor)
	}
	budgetField := "daily_budget"
	if lifetime {
		budgetField = "lifetime_budget"
	}
	// Sent as a STRING, the vocabulary Meta reports these fields in and accepts them in.
	body := map[string]any{budgetField: strconv.FormatInt(budgetMinor, 10)}
	if err := c.doRequest(ctx, http.MethodPost, "/"+adSetID, body, nil); err != nil {
		return fmt.Errorf("meta: update ad set %s %s to %d: %w", adSetID, budgetField, budgetMinor, err)
	}
	return nil
}

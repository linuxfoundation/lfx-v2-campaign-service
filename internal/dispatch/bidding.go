// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ---------------------------------------------------------------------------
// Per-campaign bidding choice at create time (LFXV2-2665)
//
// Every lane platform's dispatch config — microsoftConfig, metaConfig, redditConfig,
// twitterConfig and linkedInConfig — accepts one optional object:
//
//	"bidding": {"strategy": "automatic" | "manual", "bidAmount": 1.25}
//
// OMITTED means automatic: the platform sets every bid. Only a genuinely ABSENT field defaults —
// an explicit `"bidding": null`, `"strategy": null` or `""`, or `"bidAmount": null` is a 400 (see
// biddingConfig and checkBiddingShape). MANUAL creates the campaign
// with a manual max cost-per-click bid of bidAmount, in the account currency's MAJOR units (the
// same unit as the config's budget, no FX), and in EXACTLY the shape the bid lever
// (update-campaign-bid) accepts on that platform — so the lever, which refuses every
// automatically-bid campaign, can change the bid of a campaign created this way.
//
// The JSON spelling is camelCase because the config envelope is (budgetUsd, cpcBid,
// imageAssetId); the top-level Goa payloads are snake_case and this object is not one of them.
//
// VALIDATION IS A PURE FUNCTION OF THE CONFIG and runs twice: in each dispatcher's
// PreflightCreate, on the synchronous create path, where a refusal is a 400 the caller can act on
// (domain.ErrCreateConfigInvalid); and again in Dispatch, before any upstream call, for any path
// that skipped the preflight. The one rule that needs the platform is Meta's: an amount must be a
// whole number of the account currency's minor units, and the currency is known only after Meta's
// account preflight (a read), so it is checked there, still before anything is created.
//
// NOTHING IS ROUNDED. A bid that is not exactly representable in the unit the platform takes is
// refused, never rounded: rounding would send one bid upstream and record another on the row.
// ---------------------------------------------------------------------------

// biddingConfig is the wire shape of the optional `bidding` object.
//
// PRESENCE IS PRESERVED (PR #332 review). encoding/json alone decodes an omitted strategy, an
// explicit `"strategy": ""` and `"strategy": null` to the same "", and an omitted bidAmount and
// `"bidAmount": null` to the same nil — so a typo would launch a paid campaign on automatic bidding
// instead of getting the promised 400. UnmarshalJSON records which fields were PRESENT, and
// resolveBidding defaults only fields that are genuinely absent.
type biddingConfig struct {
	Strategy string `json:"strategy"`
	// BidAmount is a pointer so "absent" and "0" are different facts: an automatic request that
	// names any amount, zero included, is refused rather than silently dropped.
	BidAmount *float64 `json:"bidAmount"`

	// strategyPresent / bidAmountPresent: the key appeared in the JSON (with any value, null
	// included). Set by UnmarshalJSON only.
	strategyPresent, bidAmountPresent bool
}

// UnmarshalJSON decodes the bidding object keeping field presence and JSON type: a present
// strategy must be a string and a present bidAmount a number (null is recorded as present-but-null
// and refused by resolveBidding). A wrong JSON type is an error here — the synchronous pre-check
// (checkBiddingShape) reports it as a 400 before this decode ever runs on the create path.
func (b *biddingConfig) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("bidding must be an object")
	}
	// Exact keys only, as checkBiddingShape enforces: a case variant ("Strategy") or an unknown key
	// would otherwise be silently ignored, and the campaign created automatic without the cap.
	if msg := biddingKeyProblem(fields); msg != "" {
		return fmt.Errorf("bidding: %s", msg)
	}
	*b = biddingConfig{}
	if v, ok := fields["strategy"]; ok {
		b.strategyPresent = true
		if !isJSONNull(v) {
			if err := json.Unmarshal(v, &b.Strategy); err != nil {
				return fmt.Errorf("bidding.strategy must be a string")
			}
		}
	}
	if v, ok := fields["bidAmount"]; ok {
		b.bidAmountPresent = true
		if !isJSONNull(v) {
			var n float64
			if err := json.Unmarshal(v, &n); err != nil {
				return fmt.Errorf("bidding.bidAmount must be a number")
			}
			b.BidAmount = &n
		}
	}
	return nil
}

// The strategy spellings the config accepts, exactly — no case folding, so the value persisted is
// the value sent.
const (
	biddingStrategyAutomatic = "automatic"
	biddingStrategyManual    = "manual"
)

// maxCreateBid is the contract's ceiling for a starting bid, the same 1,000,000 the bid lever
// applies (service.maxCampaignBid): absurd in USD, ordinary in JPY, KRW or IDR. Each platform's
// own lower bound is applied on top where the platform documents one.
const maxCreateBid = 1_000_000.0

// microsPerMajorUnit is the finest unit any lane platform bids in (Reddit's bid_value and X's
// bid_amount_local_micro are micro-units), so a bid must be a whole number of micros everywhere.
const microsPerMajorUnit = 1_000_000

// bidPlan is a validated bidding choice: what the create path sends and what the row records.
type bidPlan struct {
	strategy model.BiddingStrategy
	// amount is the manual starting bid in the account currency's major units; zero for automatic.
	amount float64
}

// manual reports whether the plan creates a manual max-CPC campaign.
func (p bidPlan) manual() bool { return p.strategy == model.BiddingManual }

// stamp records the plan on the campaign row a dispatch returns: the strategy always, and the
// starting bid onto max_cpc_bid when it is manual (the bid lever later overwrites it). nil-safe,
// so it can sit in a defer over a named return that may be nil. Each Dispatch calls it ONLY on a
// successful result whose client reports BidApplied — the run created the entity carrying the
// bid. A reused entity, or a partial, leaves both columns NULL: the row must never claim a bid the
// platform was not given.
func (p bidPlan) stamp(c *model.Campaign) {
	if c == nil || p.strategy == "" {
		return
	}
	s := p.strategy
	c.BiddingStrategy = &s
	if p.manual() {
		amount := p.amount
		c.MaxCPCBid = &amount
	}
}

// biddingInvalid builds a refusal the create path answers 400. key is the platform's config key
// (e.g. "metaConfig"), so the message names the exact field to change. The text is this service's
// own: no caller value other than the amount, and nothing from a platform, is placed in it.
func biddingInvalid(key, format string, args ...any) error {
	return fmt.Errorf("%w: %s.bidding: %s", domain.ErrCreateConfigInvalid, key, fmt.Sprintf(format, args...))
}

// formatBid renders an amount at full precision for a refusal message.
func formatBid(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// resolveBidding applies the platform-independent rules: strategy automatic (the default) or
// manual; manual requires a finite bidAmount greater than zero, at most maxCreateBid, and a whole
// number of micro-units; automatic must not name an amount. Platform rules are applied by each
// dispatcher on the plan this returns.
func resolveBidding(key string, cfg *biddingConfig) (bidPlan, error) {
	if cfg == nil {
		return bidPlan{strategy: model.BiddingAutomatic}, nil
	}
	// Only a genuinely ABSENT field takes a default. An explicit null or empty strategy, or an
	// explicit null bidAmount, is a mistake the caller must fix — never read as "automatic".
	if cfg.strategyPresent && cfg.Strategy == "" {
		return bidPlan{}, biddingInvalid(key, "strategy must be \"automatic\" or \"manual\"; an empty or null strategy is refused — omit the field for the automatic default")
	}
	if cfg.bidAmountPresent && cfg.BidAmount == nil {
		return bidPlan{}, biddingInvalid(key, "bidAmount must be a number; null is refused — omit bidAmount for automatic bidding, or send the bid for manual")
	}
	switch cfg.Strategy {
	case "", biddingStrategyAutomatic:
		if cfg.BidAmount != nil {
			return bidPlan{}, biddingInvalid(key, "bidAmount is only accepted with strategy \"manual\"; automatic bidding lets the platform set every bid, so remove bidAmount or choose manual")
		}
		return bidPlan{strategy: model.BiddingAutomatic}, nil
	case biddingStrategyManual:
	default:
		return bidPlan{}, biddingInvalid(key, "strategy must be \"automatic\" or \"manual\"")
	}
	if cfg.BidAmount == nil {
		return bidPlan{}, biddingInvalid(key, "strategy \"manual\" requires bidAmount, the starting max cost-per-click bid in the ad account's currency")
	}
	amount := *cfg.BidAmount
	switch {
	case math.IsNaN(amount) || math.IsInf(amount, 0):
		return bidPlan{}, biddingInvalid(key, "bidAmount must be a finite number")
	case amount <= 0:
		return bidPlan{}, biddingInvalid(key, "bidAmount must be greater than zero")
	case amount > maxCreateBid:
		return bidPlan{}, biddingInvalid(key, "bidAmount %s exceeds the maximum %s this service sets", formatBid(amount), formatBid(maxCreateBid))
	}
	if _, ok := exactScaled(amount, microsPerMajorUnit); !ok {
		return bidPlan{}, biddingInvalid(key, "bidAmount %s is not a whole number of micro-units (at most six decimal places); it is refused rather than rounded", formatBid(amount))
	}
	return bidPlan{strategy: model.BiddingManual, amount: amount}, nil
}

// exactScaled returns amount×scale as an integer when that product is EXACTLY an integer at the
// precision a decimal input carries, and false otherwise. The test is a round trip: the nearest
// double to n/scale is unique, and IEEE division is correctly rounded, so float64(n)/scale equals
// amount precisely when amount is the double nearest to the decimal n/scale — i.e. when the
// caller wrote a number with no more decimals than scale allows. 0.29 at scale 100 passes (29);
// 0.295 at scale 100 does not, and is never turned into 30 or 29.
func exactScaled(amount float64, scale int64) (int64, bool) {
	scaled := math.Round(amount * float64(scale))
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || math.Abs(scaled) >= float64(math.MaxInt64) {
		return 0, false
	}
	n := int64(scaled)
	return n, float64(n)/float64(scale) == amount
}

// checkBiddingShape type-checks ONLY the `bidding` object of one platform's config, for the
// synchronous pre-check (PR #332 review). Each lane PreflightCreate decodes its whole config and
// treats a decode failure as ErrPreflightNotApplicable — leaving it to Dispatch, which fails the job
// after the 202 — and that is still right for every OTHER field, whose type errors were never
// promised as a 400. But `bidding` IS promised as a synchronous 400, so a wrong-typed one (bidAmount
// "1.25", strategy 7, bidding "manual") must not fall through to a failed job. This decodes the
// envelope to raw JSON, finds key.bidding, and refuses a wrong JSON type with an actionable
// ErrCreateConfigInvalid naming the field. Anything it cannot attribute to `bidding` — a malformed
// envelope, a config that is not an object, no `bidding` at all — returns nil and is left to the
// ordinary decode. An explicit null — of `bidding`, `strategy` or `bidAmount` — or an empty
// strategy is refused too: only a genuinely ABSENT field takes a default (PR #332 review). Dispatch
// runs this same check before any upstream call, for any path that skipped the pre-check.
func checkBiddingShape(config json.RawMessage, key string) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope[key], &platformCfg) != nil {
		return nil
	}
	// The config struct is decoded by encoding/json, which matches `bidding` CASE-INSENSITIVELY and
	// lets the LAST matching key win ("Bidding", "BIDDING" and "bidding" all land in the same field).
	// Reading only the exact key here would therefore disagree with what Dispatch decodes (PR #332
	// review): `"Bidding": null` would slip past the null check. Any case variant is refused, so the
	// one key both decoders see is the exact `bidding`.
	cfgKeys := make([]string, 0, len(platformCfg))
	for k := range platformCfg {
		cfgKeys = append(cfgKeys, k)
	}
	sort.Strings(cfgKeys)
	for _, k := range cfgKeys {
		if k != "bidding" && strings.EqualFold(k, "bidding") {
			return biddingInvalid(key, "%s is not a config field; use \"bidding\" (exact spelling)", quoteKey(k))
		}
	}
	raw, ok := platformCfg["bidding"]
	if !ok {
		return nil
	}
	if isJSONNull(raw) {
		// Explicit null is not "omitted": only a genuinely absent bidding defaults to automatic.
		return biddingInvalid(key, "bidding is null; omit it for the automatic default, or send {\"strategy\": \"automatic\" | \"manual\"}")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return biddingInvalid(key, "bidding must be an object, {\"strategy\": \"automatic\" | \"manual\", \"bidAmount\": number}")
	}
	if msg := biddingKeyProblem(fields); msg != "" {
		return biddingInvalid(key, "%s", msg)
	}
	if v, ok := fields["strategy"]; ok {
		var str string
		if isJSONNull(v) || json.Unmarshal(v, &str) != nil || str == "" {
			return biddingInvalid(key, "strategy must be a string, \"automatic\" or \"manual\"; null and \"\" are refused — omit the field for the automatic default")
		}
	}
	if v, ok := fields["bidAmount"]; ok {
		var n float64
		if isJSONNull(v) || json.Unmarshal(v, &n) != nil {
			return biddingInvalid(key, "bidAmount must be a number (for example 1.25), not null, a string or other JSON type")
		}
	}
	return nil
}

// biddingFields are the only keys a bidding object may carry, spelled exactly.
var biddingFields = []string{"strategy", "bidAmount"}

// biddingKeyProblem refuses any key of a bidding object that is not exactly "strategy" or
// "bidAmount" (PR #332 review): a case variant names the field the caller meant ("use
// \"strategy\""), and anything else is an unknown field. Without this, `{"Strategy":"manual",
// "BidAmount":1.25}` decoded to an empty object — automatic bidding, no cap.
func biddingKeyProblem(fields map[string]json.RawMessage) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a deterministic message when several keys are wrong
	for _, k := range keys {
		known := false
		for _, f := range biddingFields {
			if k == f {
				known = true
				break
			}
			if strings.EqualFold(k, f) {
				return fmt.Sprintf("%s is not a bidding field; use %q (exact spelling)", quoteKey(k), f)
			}
		}
		if !known {
			return fmt.Sprintf("unknown bidding field %s; bidding accepts only \"strategy\" and \"bidAmount\"", quoteKey(k))
		}
	}
	return ""
}

// quoteKey renders a caller-supplied key for a refusal message: quoted, and cut to 40 runes so a
// pathological key cannot bloat the 400.
func quoteKey(k string) string {
	if r := []rune(k); len(r) > 40 {
		k = string(r[:40]) + "…"
	}
	return strconv.Quote(k)
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

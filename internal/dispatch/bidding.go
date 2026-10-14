// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

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
// OMITTED (or null) means automatic: the platform sets every bid. MANUAL creates the campaign
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
type biddingConfig struct {
	Strategy string `json:"strategy"`
	// BidAmount is a pointer so "absent" and "0" are different facts: an automatic request that
	// names any amount, zero included, is refused rather than silently dropped.
	BidAmount *float64 `json:"bidAmount"`
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
// ordinary decode. JSON null is accepted wherever the field is optional, as encoding/json does.
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
	raw, ok := platformCfg["bidding"]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return biddingInvalid(key, "bidding must be an object, {\"strategy\": \"automatic\" | \"manual\", \"bidAmount\": number}")
	}
	if v, ok := fields["strategy"]; ok && !isJSONNull(v) {
		var str string
		if json.Unmarshal(v, &str) != nil {
			return biddingInvalid(key, "strategy must be a string, \"automatic\" or \"manual\"")
		}
	}
	if v, ok := fields["bidAmount"]; ok && !isJSONNull(v) {
		var n float64
		if json.Unmarshal(v, &n) != nil {
			return biddingInvalid(key, "bidAmount must be a number (for example 1.25), not a string or other JSON type")
		}
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

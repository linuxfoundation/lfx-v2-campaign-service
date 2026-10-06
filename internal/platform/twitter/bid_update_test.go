// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"errors"
	"math"
	"testing"
)

func TestBidMicros(t *testing.T) {
	for _, tc := range []struct {
		amount  float64
		want    int64
		refused bool
	}{
		{2.35, 2_350_000, false},
		{0.000001, 1, false},
		{0.0000004, 0, true},
		{1_000_000, 1_000_000_000_000, false},
		{1_000_000.5, 0, true},
		{0, 0, true},
		{-2, 0, true},
		{math.NaN(), 0, true},
		{math.Inf(1), 0, true},
	} {
		got, err := BidMicros(tc.amount)
		if tc.refused {
			if !errors.Is(err, ErrBidAmountInvalid) {
				t.Errorf("BidMicros(%v) = %d, %v; want ErrBidAmountInvalid", tc.amount, got, err)
			}
			if _, ok := BidAmountReason(err); !ok {
				t.Errorf("refusal for %v carries no client-safe reason", tc.amount)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("BidMicros(%v) = %d, %v; want %d", tc.amount, got, err, tc.want)
		}
	}
}

func TestBidAmountErrorCode(t *testing.T) {
	for codes, want := range map[string]bool{
		"INVALID_BID_AMOUNT":   true,
		"BID_TOO_LOW":          true,
		"INVALID_BID_STRATEGY": false,
		"INVALID_PARAMETER":    false,
		"":                     false,
	} {
		if got := bidAmountErrorCode([]string{codes}); got != want {
			t.Errorf("bidAmountErrorCode(%q) = %v, want %v", codes, got, want)
		}
	}
}

func TestLineItemBid_ManualCPC(t *testing.T) {
	if !(&LineItemBid{BidStrategy: "MAX", PayBy: "LINK_CLICK"}).ManualCPC() {
		t.Error("MAX charged per link click must be a manual CPC")
	}
	for _, b := range []LineItemBid{
		{BidStrategy: "AUTO", PayBy: "LINK_CLICK"},
		{BidStrategy: "TARGET", PayBy: "LINK_CLICK"},
		{BidStrategy: "MAX", PayBy: "IMPRESSION"},
		{BidStrategy: "MAX"},
	} {
		if b.ManualCPC() {
			t.Errorf("%+v must not be a manual CPC", b)
		}
	}
}

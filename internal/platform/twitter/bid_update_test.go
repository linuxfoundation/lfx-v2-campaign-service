// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
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

func TestBidAmountRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"INVALID_PARAMETER on bid_amount_local_micro", `{"errors":[{"code":"INVALID_PARAMETER","parameter":"bid_amount_local_micro","message":"too low"}]}`, true},
		{"INVALID_PARAMETER on another parameter", `{"errors":[{"code":"INVALID_PARAMETER","parameter":"start_time"}]}`, false},
		{"FORBIDDEN", `{"errors":[{"code":"FORBIDDEN","parameter":"bid_amount_local_micro"}]}`, false},
		{"a bid-unit code", `{"errors":[{"code":"INVALID_BID_TYPE"}]}`, false},
		{"a bid-amount-looking code with no parameter", `{"errors":[{"code":"INVALID_BID_AMOUNT"}]}`, false},
		{"MISSING_PARAMETER on the bid", `{"errors":[{"code":"MISSING_PARAMETER","parameter":"bid_amount_local_micro"}]}`, false},
		{"not an envelope", `nope`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bidAmountRefused(parseErrorParams([]byte(tc.body))); got != tc.want {
				t.Errorf("bidAmountRefused = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseErrorParams_Bounded(t *testing.T) {
	long := strings.Repeat("A", maxErrorCodeCodeLength+1)
	body := `{"errors":[{"code":"` + long + `","parameter":"x"},{"code":"INVALID_PARAMETER","parameter":"` + long + `"},{"code":"INVALID_PARAMETER","parameter":"bid_amount_local_micro"}]}`
	got := parseErrorParams([]byte(body))
	if len(got) != 1 || got[0].Parameter != "bid_amount_local_micro" {
		t.Fatalf("over-long entries must be dropped, got %+v", got)
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

// A quoted numeric amount reads like the integer form (X documents an integer; a string of the
// same digits must not make every write fail closed), while a quoted non-number stays unreadable.
func TestParseMicros_AcceptsQuotedDigits(t *testing.T) {
	for in, want := range map[string]int64{`1500000`: 1500000, `"1500000"`: 1500000, `" 42 "`: 42} {
		got, bad := parseMicros(json.RawMessage(in))
		if bad || got == nil || *got != want {
			t.Errorf("parseMicros(%s) = %v, %v; want %d", in, got, bad, want)
		}
	}
	for _, in := range []string{`"abc"`, `"1.5"`, `true`} {
		if _, bad := parseMicros(json.RawMessage(in)); !bad {
			t.Errorf("parseMicros(%s) should be unreadable", in)
		}
	}
	if got, bad := parseMicros(json.RawMessage(`null`)); got != nil || bad {
		t.Errorf("parseMicros(null) = %v, %v; want absent", got, bad)
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// TestValidateDailyCap pins #584's rules: a cap exists only beside a lifetime budget, obeys every
// daily rule, and may not exceed the lifetime total. Every refusal is a budget-amount refusal, so
// BudgetAmountReason can surface it as a 400.
func TestValidateDailyCap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		capUSD     float64
		totalUSD   float64
		lifetime   bool
		wantWire   string
		wantReason string
	}{
		{name: "LF's usual shape", capUSD: 20, totalUSD: 1000, lifetime: true, wantWire: "20.00"},
		{name: "a cap equal to the total", capUSD: 100, totalUSD: 100, lifetime: true, wantWire: "100.00"},
		{name: "rounded to cents", capUSD: 25.005, totalUSD: 1000, lifetime: true, wantWire: "25.00"},
		{name: "beside a daily budget", capUSD: 20, totalUSD: 50, lifetime: false, wantReason: "applies only to a lifetime budget"},
		{name: "under the daily minimum", capUSD: 9.99, totalUSD: 1000, lifetime: true, wantReason: "daily cap: daily budget 9.99 is below LinkedIn's minimum"},
		{name: "zero", capUSD: 0, totalUSD: 1000, lifetime: true, wantReason: "daily cap: budget must be greater than zero"},
		{name: "not a number", capUSD: math.NaN(), totalUSD: 1000, lifetime: true, wantReason: "daily cap: budget must be a finite number"},
		{name: "above the total", capUSD: 1500, totalUSD: 1000, lifetime: true, wantReason: "daily cap 1500.00 is more than the lifetime budget 1000.00"},
		// The total's own refusal belongs to ValidateBudgetAmount, but a cap above even an
		// unacceptable total is refused here, so the caller hears about it synchronously.
		{name: "a total under the lifetime minimum: a cap within it passes here", capUSD: 20, totalUSD: 50, lifetime: true, wantWire: "20.00"},
		{name: "a total under the lifetime minimum: a cap above it is refused", capUSD: 150, totalUSD: 50, lifetime: true, wantReason: "daily cap 150.00 is more than the lifetime budget 50.00"},
		{name: "a non-finite total is not compared", capUSD: 20, totalUSD: math.Inf(1), lifetime: true, wantWire: "20.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := ValidateDailyCap(tc.capUSD, tc.totalUSD, tc.lifetime)
			if tc.wantReason == "" {
				if err != nil || wire != tc.wantWire {
					t.Fatalf("ValidateDailyCap = %q, %v; want %q", wire, err, tc.wantWire)
				}
				return
			}
			reason, ok := BudgetAmountReason(err)
			if !ok || !errors.Is(err, ErrBudgetAmountInvalid) || !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("ValidateDailyCap err = %v (reason %q, ok %v); want a budget-amount refusal containing %q", err, reason, ok, tc.wantReason)
			}
		})
	}
}

// TestValidateTotalCoversCap pins the edit-side twin: a lifetime total may not drop below the
// campaign's existing daily cap.
func TestValidateTotalCoversCap(t *testing.T) {
	if err := ValidateTotalCoversCap(1000, 20); err != nil {
		t.Fatalf("a total above the cap: %v", err)
	}
	if err := ValidateTotalCoversCap(200, 200); err != nil {
		t.Fatalf("a total equal to the cap: %v", err)
	}
	err := ValidateTotalCoversCap(150, 200)
	if reason, ok := BudgetAmountReason(err); !ok || !strings.Contains(reason, "lifetime budget 150.00 is less than this campaign's daily cap 200.00") {
		t.Fatalf("a total below the cap: err = %v, reason %q", err, reason)
	}
	if _, ok := BudgetAmountReason(ValidateTotalCoversCap(50, 20)); !ok {
		t.Fatal("a total under LinkedIn's lifetime minimum must still be refused as an amount")
	}
}

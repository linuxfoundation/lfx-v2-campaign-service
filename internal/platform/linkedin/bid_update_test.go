// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
)

func TestCampaignSettings_ManualCPC(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		cost, opt *string
		want      bool
	}{
		{str("CPC"), str("NONE"), true},
		{str("CPC"), str("ENHANCED_CONVERSION"), true},
		{str("CPC"), str("MAX_CLICK"), false},
		{str("CPC"), str("TARGET_COST_PER_CLICK"), false},
		{str("CPM"), str("NONE"), false},
		{str("CPM"), str("CAP_COST_AND_MAXIMIZE_CLICKS"), false},
		{nil, str("NONE"), false},
		{str("CPC"), nil, false},
	} {
		s := &CampaignSettings{CostType: tc.cost, OptimizationTargetType: tc.opt}
		if got := s.ManualCPC(); got != tc.want {
			t.Errorf("ManualCPC(%v,%v) = %v, want %v", deref2(tc.cost), deref2(tc.opt), got, tc.want)
		}
	}
}

func deref2(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestValidateBidAmount(t *testing.T) {
	for _, tc := range []struct {
		amount float64
		wire   string
	}{{5.25, "5.25"}, {0.29, "0.29"}, {2, "2.00"}, {0.01, "0.01"}, {1_000_000, "1000000.00"}} {
		if got, err := ValidateBidAmount(tc.amount); err != nil || got != tc.wire {
			t.Errorf("ValidateBidAmount(%v) = %q, %v; want %q", tc.amount, got, err, tc.wire)
		}
	}
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1), 1_000_000.01, 2.345, 0.001, 0.004, 5e-9, 1e-12, 2.340000001, 2.3400000001, 0.010000001} {
		_, err := ValidateBidAmount(bad)
		if !errors.Is(err, ErrBidAmountInvalid) {
			t.Errorf("ValidateBidAmount(%v) = %v, want ErrBidAmountInvalid", bad, err)
		}
		if _, ok := BidAmountReason(err); !ok {
			t.Errorf("ValidateBidAmount(%v) carries no client-safe reason", bad)
		}
	}
}

func TestUpdateCampaignUnitCost_WritesTheAmountOnlyAsAPartialUpdate(t *testing.T) {
	s, c := newLeverStub(t)
	if err := c.UpdateCampaignUnitCost(context.Background(), "777", "5.25"); err != nil {
		t.Fatalf("UpdateCampaignUnitCost: %v", err)
	}
	reqs := s.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v, want exactly one", reqs)
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/adAccounts/509430019/adCampaigns/777" || r.RestliMethod != "PARTIAL_UPDATE" {
		t.Errorf("request = %+v", r)
	}
	var body map[string]map[string]map[string]map[string]string
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatalf("body %s: %v", r.Body, err)
	}
	set := body["patch"]["$set"]
	if len(set) != 1 || set["unitCost"]["amount"] != "5.25" || set["unitCost"]["currencyCode"] != "USD" {
		t.Errorf("$set = %v, want unitCost {5.25 USD} and nothing else (never a strategy or cost type)", set)
	}
}

func TestUpdateCampaignUnitCost_RefusesANonCanonicalAmountBeforeSending(t *testing.T) {
	for _, wire := range []string{"5.2", "5.250", " 5.25", "1e1", "NaN", "0.00", "2.345", ""} {
		s, c := newLeverStub(t)
		if err := c.UpdateCampaignUnitCost(context.Background(), "777", wire); err == nil {
			t.Errorf("wire %q was accepted", wire)
		}
		if n := len(s.requests()); n != 0 {
			t.Errorf("wire %q: %d request(s) sent", wire, n)
		}
	}
}

// amountEnvelope is LinkedIn's documented structured refusal, padded past apiError.Body's
// 400-character cut so the test proves the FULL body is what is parsed.
func amountEnvelope(status int, fieldPath, code, value string) string {
	return `{"serviceErrorCode":10007,"status":` + itoa(status) + `,"code":"` + code + `",` +
		`"message":"` + strings.Repeat("x", 450) + `","errorDetailType":"com.linkedin.common.error.BadRequest",` +
		`"errorDetails":{"inputErrors":[{"input":{"value":{"decimalValue":"` + value + `"},"inputPath":{"fieldPath":"` + fieldPath + `"}},` +
		`"minValue":"6.00","code":"` + code + `","description":"too low"}]}}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestUpdateCampaignUnitCost_Classification(t *testing.T) {
	cases := []struct {
		name            string
		code            int
		body            string
		wantAmount      bool
		wantUnconfirmed bool
		wantRequests    int
	}{
		{"204 success", 204, "", false, false, 1},
		{"structured amount refusal", 400, amountEnvelope(400, "/Campaign/unitCost/amount", "FIELD_VALUE_TOO_LOW", "5.25"), true, false, 1},
		{"refusal blaming another field", 400, amountEnvelope(400, "/Campaign/dailyBudget/amount", "FIELD_VALUE_TOO_LOW", "5.25"), false, false, 1},
		{"refusal with a non-amount code", 400, amountEnvelope(400, "/Campaign/unitCost/amount", "FIELD_IMMUTABLE_FOR_NON_DRAFT_CAMPAIGNS", "5.25"), false, false, 1},
		{"envelope status disagrees with HTTP", 400, amountEnvelope(422, "/Campaign/unitCost/amount", "FIELD_VALUE_TOO_LOW", "5.25"), false, false, 1},
		{"envelope echoes another value", 400, amountEnvelope(400, "/Campaign/unitCost/amount", "FIELD_VALUE_TOO_LOW", "9.99"), false, false, 1},
		{"envelope with a duplicated key", 400, strings.Replace(amountEnvelope(400, "/Campaign/unitCost/amount", "FIELD_VALUE_TOO_LOW", "5.25"), `"status":400,`, `"status":400,"status":400,`, 1), false, false, 1},
		{"unstructured 400", 400, `{"message":"bid too low","status":400}`, false, false, 1},
		{"403 is definite", 403, `{"status":403}`, false, false, 1},
		{"500 may have applied", 500, `{}`, false, true, 1},
		// SEND ONCE: a 429 is NOT retried — it comes back as itself, unconfirmed.
		{"429 is unconfirmed and never retried", 429, `{}`, false, true, 1},
		{"401 on the write may have applied", 401, `{"status":401}`, false, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newLeverStub(t)
			s.writeCode, s.writeBody = tc.code, tc.body
			err := c.UpdateCampaignUnitCost(context.Background(), "777", "5.25")
			if tc.code == 204 {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
			} else if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ErrBidAmountInvalid); got != tc.wantAmount {
				t.Errorf("amount refusal = %v, want %v (%v)", got, tc.wantAmount, err)
			}
			if got := err != nil && IsOutcomeUnconfirmed(err); got != tc.wantUnconfirmed {
				t.Errorf("unconfirmed = %v, want %v (%v)", got, tc.wantUnconfirmed, err)
			}
			if tc.wantAmount && strings.Contains(err.Error(), "too low") {
				t.Errorf("LinkedIn's own text leaked into the refusal: %v", err)
			}
			if n := len(s.requests()); n != tc.wantRequests {
				t.Errorf("%d request(s), want %d", n, tc.wantRequests)
			}
		})
	}
}

// The budget write's PARTIAL_UPDATE still retries a 429: the send-once opt-out is per call.
func TestSendOnce_IsScopedToTheBidWrite(t *testing.T) {
	s, c := newLeverStub(t)
	s.writeCode = 429
	_ = c.UpdateCampaignBudget(context.Background(), "777", "100.00", false)
	if n := len(s.requests()); n < 2 {
		t.Fatalf("the budget write sent %d request(s); its 429 retry must be untouched by the bid write's opt-out", n)
	}
}

// Both sub-cent refusals are reachable and each says what is wrong: an amount that rounds to zero
// cents (including one under 1e-8, inside the whole-cent tolerance) is "rounds to zero", never a
// "0.00" wire; a non-zero sub-cent remainder is "not a whole number of cents".
func TestValidateBidAmount_SubCentRefusalsAreDistinct(t *testing.T) {
	for amount, want := range map[float64]string{
		0.004: "rounds to zero",
		5e-9:  "rounds to zero",
		2.345: "not a whole number of cents",
		0.015: "not a whole number of cents",
		// Inside the old 1e-6-cent tolerance, which rounded these to "2.34" and "1.00".
		2.340000001:  "not a whole number of cents",
		1.0000000001: "not a whole number of cents",
	} {
		wire, err := ValidateBidAmount(amount)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateBidAmount(%v) = %q, %v; want a refusal saying %q", amount, wire, err, want)
		}
	}
}

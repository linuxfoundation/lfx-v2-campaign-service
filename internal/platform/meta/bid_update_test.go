// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBidToMinorUnits(t *testing.T) {
	for _, tc := range []struct {
		amount  float64
		offset  int64
		want    int64
		refused bool
	}{
		{2.35, 100, 235, false},
		{0.005, 100, 1, false}, // half a cent rounds away from zero to the one-cent floor
		{0.004, 100, 0, true},
		{120, 1, 120, false},
		{0.4, 1, 0, true},
		{1_000_000, 100, 100_000_000, false},
		{1_000_000.01, 100, 0, true},
		{0, 100, 0, true},
		{-1, 100, 0, true},
		{math.NaN(), 100, 0, true},
		{math.Inf(1), 100, 0, true},
	} {
		got, err := bidToMinorUnits(tc.amount, tc.offset)
		if tc.refused {
			if !errors.Is(err, ErrBidAmountInvalid) {
				t.Errorf("bidToMinorUnits(%v, %d) = %d, %v; want ErrBidAmountInvalid", tc.amount, tc.offset, got, err)
			}
			if _, ok := BidAmountReason(err); !ok {
				t.Errorf("refusal for %v carries no client-safe reason", tc.amount)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("bidToMinorUnits(%v, %d) = %d, %v; want %d", tc.amount, tc.offset, got, err, tc.want)
		}
	}
}

func TestParseBidAmount(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		want        *int64
		unparseable bool
	}{
		{``, nil, false},
		{`null`, nil, false},
		{`150`, bidPtr(150), false},
		{`"150"`, bidPtr(150), false},
		{`1.5`, nil, true},
		{`"abc"`, nil, true},
		{`true`, nil, true},
	} {
		got, bad := parseBidAmount(json.RawMessage(tc.raw))
		if bad != tc.unparseable || (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("parseBidAmount(%s) = %v, %v; want %v, %v", tc.raw, got, bad, tc.want, tc.unparseable)
		}
	}
}

func TestAdSetBid_CPCBidCap(t *testing.T) {
	ok := AdSetBid{BidStrategy: BidStrategyBidCap, BillingEvent: "LINK_CLICKS", OptimizationGoal: "LINK_CLICKS"}
	if !ok.CPCBidCap() {
		t.Error("a link-click bid cap must be a CPC cap")
	}
	for _, b := range []AdSetBid{
		{BidStrategy: "LOWEST_COST_WITHOUT_CAP", BillingEvent: "IMPRESSIONS", OptimizationGoal: "LINK_CLICKS"},
		{BidStrategy: BidStrategyBidCap, BillingEvent: "IMPRESSIONS", OptimizationGoal: "LINK_CLICKS"},
		{BidStrategy: BidStrategyBidCap, BillingEvent: "CLICKS", OptimizationGoal: "LINK_CLICKS"},
		{BidStrategy: "COST_CAP", BillingEvent: "LINK_CLICKS", OptimizationGoal: "LINK_CLICKS"},
	} {
		if b.CPCBidCap() {
			t.Errorf("%+v must not be a CPC cap", b)
		}
	}
}

func bidPtr(v int64) *int64 { return &v }

func TestGraphError_BlameFieldSpecs(t *testing.T) {
	long := strings.Repeat("x", maxBlameFieldNameSize+1)
	for _, tc := range []struct {
		name string
		data string
		want [][]string
	}{
		{"object", `{"blame_field_specs":[["bid_amount"]]}`, [][]string{{"bid_amount"}}},
		{"nested path", `{"blame_field_specs":[["targeting_spec","interested_in"],["bid_amount"]]}`, [][]string{{"targeting_spec", "interested_in"}, {"bid_amount"}}},
		{"json-encoded string", `"{\"blame_field_specs\":[[\"bid_amount\"]]}"`, [][]string{{"bid_amount"}}},
		{"absent", ``, nil},
		{"not an object", `[1]`, nil},
		{"over-long name dropped", `{"blame_field_specs":[["` + long + `"],["bid_amount"]]}`, [][]string{{"bid_amount"}}},
		{"non-string spec dropped", `{"blame_field_specs":[[1],["bid_amount"]]}`, [][]string{{"bid_amount"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &graphError{ErrorData: json.RawMessage(tc.data)}
			got := g.blameFieldSpecs()
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("blameFieldSpecs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveBidMinorUnits_RefusesMalformedAccountIDBeforeAnyRequest(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1?fields=x"}, WithBaseURL("http://127.0.0.1:0"))
	if _, err := c.ResolveBidMinorUnits(context.Background(), 2); !errors.Is(err, ErrInvalidAccountID) {
		t.Fatalf("want ErrInvalidAccountID, got %v", err)
	}
}

// A complete Graph envelope blaming bid_amount, followed by a connection closed on a mismatched
// Content-Length, must still classify as an amount refusal: the truncated-body path carries the
// same structured fields as the normal one (copyEnvelope).
func TestUpdateAdSetBid_TruncatedEnvelopeKeepsBlame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"error":{"message":"Invalid parameter","type":"OAuthException","code":100,"error_subcode":1487851,"error_data":{"blame_field_specs":[["bid_amount"]]},"fbtrace_id":"T"}}`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, herr := hj.Hijack(); herr == nil {
				_ = conn.Close()
			}
		}
	}))
	defer srv.Close()
	c := NewClient(
		Credentials{AccessToken: "tok"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
		withSleepFn(func(context.Context, time.Duration) error { return nil }),
	)
	err := c.UpdateAdSetBid(context.Background(), "120000000000001", 250)
	if err == nil {
		t.Fatal("UpdateAdSetBid succeeded on a 400")
	}
	if _, ok := BidAmountReason(err); !ok {
		t.Errorf("truncated envelope blaming bid_amount was not an amount refusal: %v", err)
	}
	if IsOutcomeUnconfirmed(err) {
		t.Errorf("a definite 400 must not be unconfirmed: %v", err)
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// budgetUpdateServers wires a token endpoint and an API server that records the mutate body.
func budgetUpdateServers(t *testing.T) (*Client, func() string) {
	t.Helper()
	// Guarded for the reason TestUpdateCampaignStatus_SendsUpdateMask documents: the handler
	// runs on the server goroutine and the assertions read before any happens-before edge.
	var mu sync.Mutex
	var body string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/555"}]}`)
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
	return c, func() string {
		mu.Lock()
		defer mu.Unlock()
		return body
	}
}

// TestUpdateCampaignBudget_DailyWritesAmountMicrosOnly pins BOTH halves of the field
// selection: the DAILY amount goes to amount_micros, and total_amount_micros must be ABSENT
// from the payload — not present as a zero. That absence is what the pointer+omitempty fields
// exist for: a non-pointer int64 would encode the unused field as 0, asking Google to set a
// second, zero-valued amount alongside the real one on a budget that already has one.
func TestUpdateCampaignBudget_DailyWritesAmountMicrosOnly(t *testing.T) {
	c, body := budgetUpdateServers(t)

	if err := c.UpdateCampaignBudget(context.Background(), "555", 25_500_000, budgetPeriodDaily); err != nil {
		t.Fatalf("UpdateCampaignBudget: %v", err)
	}

	got := body()
	for _, want := range []string{`"updateMask":"amount_micros"`, `"amountMicros":25500000`, `campaignBudgets/555`} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, `"totalAmountMicros"`) {
		t.Errorf("a DAILY update must not carry totalAmountMicros: %s", got)
	}
	if strings.Contains(got, `"create"`) {
		t.Errorf("an update operation must not carry a create: %s", got)
	}
	// period, deliveryMethod and explicitlyShared are deliberately never written — this
	// method changes the amount and nothing else, and explicitlyShared in particular is the
	// fact the dispatcher's shared-budget guard reads.
	for _, forbidden := range []string{`"period"`, `"deliveryMethod"`, `"explicitlyShared"`} {
		if strings.Contains(got, forbidden) {
			t.Errorf("body wrote %s, which this method must never touch: %s", forbidden, got)
		}
	}
}

// TestUpdateCampaignBudget_CustomPeriodWritesTotalAmountMicrosOnly is the mirror. Getting this
// arm wrong is not a smaller version of the same bug: a daily amount written onto a
// CUSTOM_PERIOD budget produces the self-contradictory row (both amount fields set) that
// GetCampaignSettings refuses to read back at all, leaving a campaign whose budget this
// service can no longer report.
func TestUpdateCampaignBudget_CustomPeriodWritesTotalAmountMicrosOnly(t *testing.T) {
	c, body := budgetUpdateServers(t)

	if err := c.UpdateCampaignBudget(context.Background(), "555", 120_000_000, budgetPeriodCustom); err != nil {
		t.Fatalf("UpdateCampaignBudget: %v", err)
	}

	got := body()
	for _, want := range []string{`"updateMask":"total_amount_micros"`, `"totalAmountMicros":120000000`} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, `"amountMicros"`) {
		t.Errorf("a CUSTOM_PERIOD update must not carry amountMicros: %s", got)
	}
}

// TestUpdateCampaignBudget_RejectsBadInput guards the input contract BEFORE any request. The
// budget id interpolates into a resourceName, so a non-numeric id could alter the resource
// path — the same guard, for the same reason, that UpdateCampaignStatus applies to a campaign
// id. A non-positive amount is refused here too even though the dispatcher converts through
// ValidateBudgetMicros: this method is exported, and Google accepts a zero as a real
// instruction to stop the campaign spending.
func TestUpdateCampaignBudget_RejectsBadInput(t *testing.T) {
	var mu sync.Mutex
	var reached bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer apiSrv.Close()
	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	cases := []struct {
		name     string
		budgetID string
		micros   int64
		period   string
	}{
		{"empty budget id", "  ", 1_000_000, budgetPeriodDaily},
		{"non-numeric budget id", "555/../666", 1_000_000, budgetPeriodDaily},
		{"budget id with a query delimiter", "555?x=1", 1_000_000, budgetPeriodDaily},
		{"zero micros", "555", 0, budgetPeriodDaily},
		{"negative micros", "555", -1, budgetPeriodDaily},
		{"empty period", "555", 1_000_000, ""},
		// Google's UNKNOWN/UNSPECIFIED, or a value added after this client's pinned
		// version. Guessing between the two mutually exclusive amount fields writes the
		// wrong one, so an unmapped period is refused rather than defaulted to DAILY.
		{"unmapped period", "555", 1_000_000, "UNKNOWN"},
		{"lowercase period", "555", 1_000_000, "daily"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			reached = false
			mu.Unlock()
			if err := c.UpdateCampaignBudget(context.Background(), tc.budgetID, tc.micros, tc.period); err == nil {
				t.Fatal("expected a rejection")
			}
			mu.Lock()
			sawCall := reached
			mu.Unlock()
			if sawCall {
				t.Error("no API call should be made — the guard is up front")
			}
		})
	}
}

// TestUpdateCampaignBudget_RetriesThrottle pins the idempotent=true choice, which is the
// OPPOSITE of the create path's and the SAME as UpdateCampaignStatus's. Setting an amount
// twice converges on identical state, so a throttled attempt must be retried rather than
// surfaced as an avoidable failure; a create, by contrast, could double-create. Without this
// test, flipping the flag to false would silently remove throttle resilience.
func TestUpdateCampaignBudget_RetriesThrottle(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/555"}]}`)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
	if err := c.UpdateCampaignBudget(context.Background(), "555", 1_000_000, budgetPeriodDaily); err != nil {
		t.Fatalf("a throttled budget update must be retried, not failed: %v", err)
	}
	mu.Lock()
	total := attempts
	mu.Unlock()
	if total < 2 {
		t.Errorf("attempts = %d, want >1 (the 429 must be retried; idempotent=false would abort)", total)
	}
}

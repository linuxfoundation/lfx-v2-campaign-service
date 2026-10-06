// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

const signedPromotionURL = "https://events.example.org/register?sig=promo-s3cr3t"

func fullPromotionConfig() googleAdsPromotionConfig {
	return googleAdsPromotionConfig{
		PromotionTarget:     "Conference passes",
		DiscountPercent:     25,
		DiscountAmount:      49.99,
		OrdersOverAmount:    200,
		CurrencyCode:        "USD",
		PromotionCode:       "EARLYBIRD",
		Occasion:            "BLACK_FRIDAY",
		LanguageCode:        "pt-BR",
		StartDate:           "2026-11-01",
		EndDate:             "2026-11-30",
		RedemptionStartDate: "2026-11-01",
		RedemptionEndDate:   "2026-12-15",
		FinalURL:            signedPromotionURL,
	}
}

func fullPriceConfig() googleAdsPriceConfig {
	return googleAdsPriceConfig{
		Type:           "EVENTS",
		PriceQualifier: "FROM",
		LanguageCode:   "en",
		Offerings: []googleAdsPriceOfferingConfig{
			{Header: "Attendee", Description: "Full access", Amount: 650, CurrencyCode: "USD", Unit: "PER_DAY", FinalURL: "https://events.example.org/attendee?sig=a-s3cr3t"},
			{Header: "Student", Description: "With valid ID", Amount: 150, CurrencyCode: "USD", FinalURL: "https://events.example.org/student?sig=b-s3cr3t"},
			{Header: "Virtual", Description: "Stream only", Amount: 50, CurrencyCode: "USD", FinalURL: "https://events.example.org/virtual?sig=c-s3cr3t"},
		},
	}
}

// ---------------------------------------------------------------------------
// Mapping
// ---------------------------------------------------------------------------

// Every wire field must land in its own platform field. Promotion is the trap
// here: four of its thirteen fields are two Google oneofs, and a mapper that
// dropped one arm would produce a promotion that validates and discounts by the
// wrong mechanism.
func TestGoogleAdsPromotions_MapsEveryField(t *testing.T) {
	got := googleAdsPromotions([]googleAdsPromotionConfig{fullPromotionConfig()})
	want := []googleads.PromotionExtension{{
		PromotionTarget:     "Conference passes",
		DiscountPercent:     25,
		DiscountAmount:      49.99,
		OrdersOverAmount:    200,
		CurrencyCode:        "USD",
		PromotionCode:       "EARLYBIRD",
		Occasion:            "BLACK_FRIDAY",
		LanguageCode:        "pt-BR",
		StartDate:           "2026-11-01",
		EndDate:             "2026-11-30",
		RedemptionStartDate: "2026-11-01",
		RedemptionEndDate:   "2026-12-15",
		FinalURL:            signedPromotionURL,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// Omitting any single field must change the mapped result. A mapper that never
// read a field passes a whole-struct comparison whenever the fixture happens to
// leave that field at its zero value, which is how a dropped field survives.
func TestGoogleAdsPromotions_EachFieldIsActuallyRead(t *testing.T) {
	cases := map[string]func(c *googleAdsPromotionConfig){
		"promotionTarget":     func(c *googleAdsPromotionConfig) { c.PromotionTarget = "" },
		"discountPercent":     func(c *googleAdsPromotionConfig) { c.DiscountPercent = 0 },
		"discountAmount":      func(c *googleAdsPromotionConfig) { c.DiscountAmount = 0 },
		"ordersOverAmount":    func(c *googleAdsPromotionConfig) { c.OrdersOverAmount = 0 },
		"currencyCode":        func(c *googleAdsPromotionConfig) { c.CurrencyCode = "" },
		"promotionCode":       func(c *googleAdsPromotionConfig) { c.PromotionCode = "" },
		"occasion":            func(c *googleAdsPromotionConfig) { c.Occasion = "" },
		"languageCode":        func(c *googleAdsPromotionConfig) { c.LanguageCode = "" },
		"startDate":           func(c *googleAdsPromotionConfig) { c.StartDate = "" },
		"endDate":             func(c *googleAdsPromotionConfig) { c.EndDate = "" },
		"redemptionStartDate": func(c *googleAdsPromotionConfig) { c.RedemptionStartDate = "" },
		"redemptionEndDate":   func(c *googleAdsPromotionConfig) { c.RedemptionEndDate = "" },
		"finalUrl":            func(c *googleAdsPromotionConfig) { c.FinalURL = "" },
	}
	full := googleAdsPromotions([]googleAdsPromotionConfig{fullPromotionConfig()})
	for name, drop := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := fullPromotionConfig()
			drop(&cfg)
			if reflect.DeepEqual(googleAdsPromotions([]googleAdsPromotionConfig{cfg}), full) {
				t.Fatalf("dropping %s changed nothing; the mapper never reads it", name)
			}
		})
	}
}

func TestGoogleAdsPrices_MapsEveryFieldIncludingEachOffering(t *testing.T) {
	got := googleAdsPrices([]googleAdsPriceConfig{fullPriceConfig()})
	if len(got) != 1 {
		t.Fatalf("got %d price extensions, want 1", len(got))
	}
	if got[0].Type != "EVENTS" || got[0].PriceQualifier != "FROM" || got[0].LanguageCode != "en" {
		t.Errorf("header fields dropped: %+v", got[0])
	}
	want := []googleads.PriceOffering{
		{Header: "Attendee", Description: "Full access", Amount: 650, CurrencyCode: "USD", Unit: "PER_DAY", FinalURL: "https://events.example.org/attendee?sig=a-s3cr3t"},
		{Header: "Student", Description: "With valid ID", Amount: 150, CurrencyCode: "USD", FinalURL: "https://events.example.org/student?sig=b-s3cr3t"},
		{Header: "Virtual", Description: "Stream only", Amount: 50, CurrencyCode: "USD", FinalURL: "https://events.example.org/virtual?sig=c-s3cr3t"},
	}
	if !reflect.DeepEqual(got[0].Offerings, want) {
		t.Errorf("offerings = %+v, want %+v", got[0].Offerings, want)
	}
}

func TestGoogleAdsCallExtensions_MapsBothFields(t *testing.T) {
	got := googleAdsCallExtensions([]googleAdsCallExtensionConfig{{CountryCode: "US", PhoneNumber: "+1 415 555 0147"}})
	want := []googleads.CallExtension{{CountryCode: "US", PhoneNumber: "+1 415 555 0147"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// An absent list maps to nil, not to an empty slice: the client reads nil as "no
// extension asked for", and an empty non-nil slice would be indistinguishable
// only by luck.
func TestGoogleAdsExtensionMappers_EmptyInputIsNil(t *testing.T) {
	if got := googleAdsCallExtensions(nil); got != nil {
		t.Errorf("call extensions = %+v, want nil", got)
	}
	if got := googleAdsPromotions(nil); got != nil {
		t.Errorf("promotions = %+v, want nil", got)
	}
	if got := googleAdsPrices(nil); got != nil {
		t.Errorf("prices = %+v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// Snapshot sanitization
// ---------------------------------------------------------------------------

// A promotion destination is a clickable ad destination exactly as a sitelink's
// is, and config_snapshot is persisted UNENCRYPTED.
func TestGoogleAdsSnapshotConfig_SanitizesThePromotionURL(t *testing.T) {
	cfg := googleAdsConfig{Promotions: []googleAdsPromotionConfig{fullPromotionConfig()}}
	snapshot := googleAdsSnapshotConfig(cfg)
	got := snapshot.Promotions[0].FinalURL
	if strings.Contains(got, "s3cr3t") {
		t.Errorf("snapshot still carries the signing query: %q", got)
	}
	if !strings.HasPrefix(got, "https://events.example.org") {
		t.Errorf("snapshot lost the host: %q", got)
	}
	// The FULL url must still reach the client — it is the destination the ad
	// actually points at.
	if cfg.Promotions[0].FinalURL != signedPromotionURL {
		t.Errorf("the caller's promotion was mutated to %q", cfg.Promotions[0].FinalURL)
	}
}

// EVERY offering's URL, not just the first. A price table whose first row is
// sanitized and whose other rows are not persists two signed URLs in plain text
// while passing any "the snapshot is clean" check that reads only row zero.
func TestGoogleAdsSnapshotConfig_SanitizesEveryPriceOfferingURL(t *testing.T) {
	cfg := googleAdsConfig{Prices: []googleAdsPriceConfig{fullPriceConfig()}}
	snapshot := googleAdsSnapshotConfig(cfg)
	if len(snapshot.Prices) != 1 || len(snapshot.Prices[0].Offerings) != 3 {
		t.Fatalf("the snapshot dropped offerings: %+v", snapshot.Prices)
	}
	for i, o := range snapshot.Prices[0].Offerings {
		if strings.Contains(o.FinalURL, "s3cr3t") {
			t.Errorf("offering %d still carries the signing query: %q", i, o.FinalURL)
		}
		if !strings.HasPrefix(o.FinalURL, "https://events.example.org") {
			t.Errorf("offering %d lost the host: %q", i, o.FinalURL)
		}
	}
}

// The offerings slice is shared with the caller's config through the top-level
// copy, so the copy has to be DEEP. Reducing in place would strip the path off
// the config the client is about to send — the bug this function exists to avoid.
func TestGoogleAdsSnapshotConfig_DoesNotMutateTheCallersPriceOfferings(t *testing.T) {
	prices := []googleAdsPriceConfig{fullPriceConfig()}
	cfg := googleAdsConfig{Prices: prices}

	snapshot := googleAdsSnapshotConfig(cfg)
	if prices[0].Offerings[0].FinalURL != "https://events.example.org/attendee?sig=a-s3cr3t" {
		t.Errorf("the caller's offering was mutated to %q", prices[0].Offerings[0].FinalURL)
	}
	if &snapshot.Prices[0].Offerings[0] == &prices[0].Offerings[0] {
		t.Error("the snapshot shares the caller's offerings array")
	}
}

// The early return names six fields now. A config carrying ONLY a promotion, or
// ONLY a price table, must not take it — the same bug each creative condition
// was added to prevent, two extension types later.
func TestGoogleAdsSnapshotConfig_SanitizesEachNewExtensionAlone(t *testing.T) {
	t.Run("promotion alone", func(t *testing.T) {
		cfg := googleAdsConfig{Promotions: []googleAdsPromotionConfig{{FinalURL: signedPromotionURL}}}
		if got := googleAdsSnapshotConfig(cfg).Promotions[0].FinalURL; strings.Contains(got, "s3cr3t") {
			t.Errorf("not sanitized: %q", got)
		}
	})
	t.Run("price alone", func(t *testing.T) {
		cfg := googleAdsConfig{Prices: []googleAdsPriceConfig{fullPriceConfig()}}
		if got := googleAdsSnapshotConfig(cfg).Prices[0].Offerings[0].FinalURL; strings.Contains(got, "s3cr3t") {
			t.Errorf("not sanitized: %q", got)
		}
	})
}

// A call extension carries no URL at all, so it must survive the snapshot
// untouched — and must not be what keeps a config out of the early return.
func TestGoogleAdsSnapshotConfig_LeavesCallExtensionsAlone(t *testing.T) {
	cfg := googleAdsConfig{CallExtensions: []googleAdsCallExtensionConfig{{CountryCode: "US", PhoneNumber: "+1 415 555 0147"}}}
	snapshot := googleAdsSnapshotConfig(cfg)
	if !reflect.DeepEqual(snapshot.CallExtensions, cfg.CallExtensions) {
		t.Errorf("call extensions changed: %+v", snapshot.CallExtensions)
	}
}

// ---------------------------------------------------------------------------
// The wire shape
// ---------------------------------------------------------------------------

// The config keys are the contract the BFF writes against; renaming one silently
// drops the field, since an unknown key decodes to the zero value rather than an
// error.
func TestGoogleAdsConfig_DecodesTheNewExtensionKeys(t *testing.T) {
	raw := `{
	  "callExtensions": [{"countryCode": "US", "phoneNumber": "+1 415 555 0147"}],
	  "promotions": [{"promotionTarget": "Conference passes", "discountPercent": 25,
	                  "finalUrl": "https://events.example.org/register"}],
	  "prices": [{"type": "EVENTS", "languageCode": "en", "priceQualifier": "FROM",
	              "offerings": [{"header": "Attendee", "description": "Full access",
	                             "amount": 650, "currencyCode": "USD", "unit": "PER_DAY",
	                             "finalUrl": "https://events.example.org/attendee"}]}]
	}`
	var cfg googleAdsConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cfg.CallExtensions) != 1 || cfg.CallExtensions[0].CountryCode != "US" {
		t.Errorf("callExtensions = %+v", cfg.CallExtensions)
	}
	if len(cfg.Promotions) != 1 || cfg.Promotions[0].DiscountPercent != 25 {
		t.Errorf("promotions = %+v", cfg.Promotions)
	}
	if len(cfg.Prices) != 1 || len(cfg.Prices[0].Offerings) != 1 || cfg.Prices[0].Offerings[0].Amount != 650 {
		t.Errorf("prices = %+v", cfg.Prices)
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func sampleCallExtension() CallExtension {
	return CallExtension{CountryCode: "US", PhoneNumber: "+1 (415) 555-0147"}
}

func samplePromotion() PromotionExtension {
	return PromotionExtension{
		PromotionTarget: "Conference passes",
		DiscountPercent: 25,
		FinalURL:        "https://events.example.org/register",
	}
}

func samplePrice() PriceExtension {
	return PriceExtension{
		Type:         "EVENTS",
		LanguageCode: "en",
		Offerings: []PriceOffering{
			{Header: "Attendee", Description: "Full access", Amount: 650, CurrencyCode: "USD", FinalURL: "https://events.example.org/attendee"},
			{Header: "Student", Description: "With valid ID", Amount: 150, CurrencyCode: "USD", FinalURL: "https://events.example.org/student"},
			{Header: "Virtual", Description: "Stream only", Amount: 50, CurrencyCode: "USD", FinalURL: "https://events.example.org/virtual"},
		},
	}
}

// ---------------------------------------------------------------------------
// Call extensions
// ---------------------------------------------------------------------------

func TestValidateCallExtensions_BuildsTheAssetAndUppercasesTheCountry(t *testing.T) {
	got, err := validateCallExtensions([]CallExtension{{CountryCode: " us ", PhoneNumber: " +1 (415) 555-0147 "}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].CallAsset == nil {
		t.Fatalf("got %+v, want one call asset", got)
	}
	want := callAsset{CountryCode: "US", PhoneNumber: "+1 (415) 555-0147"}
	if *got[0].CallAsset != want {
		t.Errorf("got %+v, want %+v", *got[0].CallAsset, want)
	}
	// A call asset is the one extension here with no destination: Google routes
	// the click to the phone, and a finalUrls key would be a second destination.
	if got[0].FinalURLs != nil {
		t.Errorf("a call asset carries no final URL, got %v", got[0].FinalURLs)
	}
	if got[0].SitelinkAsset != nil || got[0].PromotionAsset != nil || got[0].PriceAsset != nil {
		t.Errorf("a call asset must set exactly one arm of the oneof, got %+v", got[0])
	}
}

func TestValidateCallExtensions_RejectsBadInput(t *testing.T) {
	cases := map[string][]CallExtension{
		"no country":        {{PhoneNumber: "4155550147"}},
		"country not ISO-2": {{CountryCode: "USA", PhoneNumber: "4155550147"}},
		"country not ASCII": {{CountryCode: "ÜS", PhoneNumber: "4155550147"}},
		"no number":         {{CountryCode: "US"}},
		// Google rejects vanity numbers on call assets, so a caller who typed one
		// is told here rather than after the campaign exists.
		"vanity number":  {{CountryCode: "US", PhoneNumber: "1-800-FLOWERS"}},
		"no digits":      {{CountryCode: "US", PhoneNumber: "+-()"}},
		"too few digits": {{CountryCode: "US", PhoneNumber: "123"}},
		"duplicate": {
			{CountryCode: "US", PhoneNumber: "+1 (415) 555-0147"},
			{CountryCode: "us", PhoneNumber: "14155550147"},
		},
	}
	for name, calls := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := validateCallExtensions(calls); err == nil {
				t.Fatalf("%s must be refused", name)
			}
		})
	}
}

func TestValidateCallExtensions_CapsTheList(t *testing.T) {
	calls := make([]CallExtension, maxCallExtensions+1)
	for i := range calls {
		calls[i] = CallExtension{CountryCode: "US", PhoneNumber: "415555" + strings.Repeat("0", 3) + string(rune('0'+i%10))}
	}
	_, err := validateCallExtensions(calls)
	if err == nil {
		t.Fatal("over-long call extension list must be refused")
	}
	// The refusal has to say whose limit it is: a reader widening this bound is
	// arguing with this client, not with Google.
	if !strings.Contains(err.Error(), "not an upstream limit") {
		t.Errorf("refusal must name the bound as this client's, got %q", err)
	}
}

// ---------------------------------------------------------------------------
// Promotions
// ---------------------------------------------------------------------------

// A promotion click is an ad click, exactly as a sitelink click is. An untagged
// destination lands in attribution as organic traffic the campaign paid for.
func TestValidatePromotions_TagsTheDestinationAndScalesThePercentage(t *testing.T) {
	in := sampleInput()
	in.Promotions = []PromotionExtension{samplePromotion()}

	got, err := validatePromotionExtensions(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].PromotionAsset == nil || len(got[0].FinalURLs) != 1 {
		t.Fatalf("got %+v, want one promotion with one final URL", got)
	}
	for _, want := range []string{"utm_source=google", "utm_medium=cpc", "events.example.org/register"} {
		if !strings.Contains(got[0].FinalURLs[0], want) {
			t.Errorf("promotion destination %q is missing %q", got[0].FinalURLs[0], want)
		}
	}
	// 1,000,000 is 100% in Google's units, so 25% is 250,000. A client that sent
	// 25 or 25,000,000 here would discount by a factor of 10,000 either way, and
	// the campaign is live before anyone sees it.
	if got[0].PromotionAsset.PercentOff != 250_000 {
		t.Errorf("percentOff = %d, want 250000", got[0].PromotionAsset.PercentOff)
	}
	if got[0].PromotionAsset.MoneyAmountOff != nil {
		t.Errorf("a percentage promotion must not also carry a money amount: %+v", got[0].PromotionAsset)
	}
}

func TestValidatePromotions_BuildsTheMoneyArms(t *testing.T) {
	in := sampleInput()
	p := samplePromotion()
	p.DiscountPercent = 0
	p.DiscountAmount = 49.99
	p.OrdersOverAmount = 200
	p.CurrencyCode = "usd"
	in.Promotions = []PromotionExtension{p}

	got, err := validatePromotionExtensions(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	asset := got[0].PromotionAsset
	if asset.MoneyAmountOff == nil || *asset.MoneyAmountOff != (money{CurrencyCode: "USD", AmountMicros: 49_990_000}) {
		t.Errorf("moneyAmountOff = %+v, want USD 49990000 micros", asset.MoneyAmountOff)
	}
	if asset.OrdersOverAmount == nil || *asset.OrdersOverAmount != (money{CurrencyCode: "USD", AmountMicros: 200_000_000}) {
		t.Errorf("ordersOverAmount = %+v, want USD 200000000 micros", asset.OrdersOverAmount)
	}
	if asset.PercentOff != 0 {
		t.Errorf("percentOff = %d, want 0 on a money promotion", asset.PercentOff)
	}
}

// Every optional key must be ABSENT rather than empty when unset: Google reads
// `"promotionCode": ""` as a blank code and `"occasion": ""` as an invalid enum,
// where omitting the key is its own documented default. A typed decode cannot
// tell the two apart, so this asserts key presence on the marshalled operation.
func TestValidatePromotions_OmitsUnsetOptionalKeys(t *testing.T) {
	in := sampleInput()
	in.Promotions = []PromotionExtension{samplePromotion()}
	got, err := validatePromotionExtensions(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		"promotionCode", "ordersOverAmount", "moneyAmountOff", "occasion",
		"languageCode", "startDate", "endDate", "redemptionStartDate", "redemptionEndDate",
	} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("unset %s must be omitted, got %s", key, b)
		}
	}
	if !strings.Contains(string(b), `"percentOff"`) {
		t.Errorf("the set discount arm must be present, got %s", b)
	}
}

func TestValidatePromotions_CarriesTheOptionalFields(t *testing.T) {
	in := sampleInput()
	p := samplePromotion()
	p.PromotionCode = "EARLYBIRD"
	p.Occasion = "BLACK_FRIDAY"
	p.LanguageCode = "pt-BR"
	p.StartDate, p.EndDate = "2026-11-01", "2026-11-30"
	p.RedemptionStartDate, p.RedemptionEndDate = "2026-11-01", "2026-12-15"
	in.Promotions = []PromotionExtension{p}

	got, err := validatePromotionExtensions(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	asset := got[0].PromotionAsset
	if asset.PromotionCode != "EARLYBIRD" || asset.Occasion != "BLACK_FRIDAY" || asset.LanguageCode != "pt-BR" {
		t.Errorf("optional fields dropped: %+v", asset)
	}
	// The redemption window may legitimately outlive the serving one — an offer
	// can be redeemable after the ad stops running — so the two are ordered
	// independently and neither is checked against the other.
	if asset.EndDate != "2026-11-30" || asset.RedemptionEndDate != "2026-12-15" {
		t.Errorf("the two windows must stay independent: %+v", asset)
	}
}

// A non-English occasion name Google adds after this client was written must be
// accepted: the check is shape, not membership, exactly as for the structured
// snippet header. Refusing a value Google accepts is the over-refusal these
// guards exist to avoid.
func TestValidatePromotions_AcceptsAnUnknownOccasionOfTheRightShape(t *testing.T) {
	in := sampleInput()
	p := samplePromotion()
	p.Occasion = "SOME_FUTURE_GOOGLE_OCCASION_2030"
	in.Promotions = []PromotionExtension{p}
	got, err := validatePromotionExtensions(in)
	if err != nil {
		t.Fatalf("an unknown occasion of the right shape must be accepted, got %v", err)
	}
	if got[0].PromotionAsset.Occasion != "SOME_FUTURE_GOOGLE_OCCASION_2030" {
		t.Errorf("occasion = %q", got[0].PromotionAsset.Occasion)
	}
}

func TestValidatePromotions_RejectsBadInput(t *testing.T) {
	cases := map[string]func(p *PromotionExtension){
		"no target":        func(p *PromotionExtension) { p.PromotionTarget = "" },
		"target too long":  func(p *PromotionExtension) { p.PromotionTarget = strings.Repeat("x", maxPromotionTargetRunes+1) },
		"no discount":      func(p *PromotionExtension) { p.DiscountPercent = 0 },
		"both discounts":   func(p *PromotionExtension) { p.DiscountAmount = 10; p.CurrencyCode = "USD" },
		"percent over 100": func(p *PromotionExtension) { p.DiscountPercent = 101 },
		"percent zero-ish": func(p *PromotionExtension) { p.DiscountPercent = 0.000001 },
		"money with no currency": func(p *PromotionExtension) {
			p.DiscountPercent, p.DiscountAmount = 0, 10
		},
		"bad currency": func(p *PromotionExtension) {
			p.DiscountPercent, p.DiscountAmount, p.CurrencyCode = 0, 10, "dollars"
		},
		"both eligibility arms": func(p *PromotionExtension) {
			p.PromotionCode, p.OrdersOverAmount, p.CurrencyCode = "X", 10, "USD"
		},
		"code too long":      func(p *PromotionExtension) { p.PromotionCode = strings.Repeat("x", maxPromotionCodeRunes+1) },
		"lowercase occasion": func(p *PromotionExtension) { p.Occasion = "black_friday" },
		"occasion as prose":  func(p *PromotionExtension) { p.Occasion = "Black Friday" },
		"bad language":       func(p *PromotionExtension) { p.LanguageCode = "english!" },
		"no destination":     func(p *PromotionExtension) { p.FinalURL = "" },
		"bad date":           func(p *PromotionExtension) { p.StartDate = "01/11/2026" },
		"impossible date":    func(p *PromotionExtension) { p.StartDate = "2026-02-31" },
		"window backwards": func(p *PromotionExtension) {
			p.StartDate, p.EndDate = "2026-11-30", "2026-11-01"
		},
		"redemption window backwards": func(p *PromotionExtension) {
			p.RedemptionStartDate, p.RedemptionEndDate = "2026-11-30", "2026-11-01"
		},
	}
	in := sampleInput()
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := samplePromotion()
			mutate(&p)
			in.Promotions = []PromotionExtension{p}
			if _, err := validatePromotionExtensions(in); err == nil {
				t.Fatalf("%s must be refused", name)
			}
		})
	}
}

// The refusal must not echo the caller's URL back: a promotion destination can
// carry a signed query string, and an error message is the one copy that reaches
// logs, the job row and the UI.
func TestValidatePromotions_ErrorDoesNotEchoTheRawURL(t *testing.T) {
	in := sampleInput()
	p := samplePromotion()
	p.FinalURL = "https://user:SECRETVALUE@events.example.org/register"
	in.Promotions = []PromotionExtension{p}
	_, err := validatePromotionExtensions(in)
	if err == nil {
		t.Fatal("a destination carrying embedded credentials must be refused")
	}
	if strings.Contains(err.Error(), "SECRETVALUE") {
		t.Errorf("error echoes the raw URL: %q", err)
	}
}

func TestValidatePromotions_DedupesByTarget(t *testing.T) {
	in := sampleInput()
	a, b := samplePromotion(), samplePromotion()
	b.PromotionTarget = "conference PASSES"
	in.Promotions = []PromotionExtension{a, b}
	if _, err := validatePromotionExtensions(in); err == nil {
		t.Fatal("two promotions for the same target must be refused")
	}
}

// ---------------------------------------------------------------------------
// Prices
// ---------------------------------------------------------------------------

func TestValidatePrices_TagsEveryOfferingsOwnDestination(t *testing.T) {
	in := sampleInput()
	in.Prices = []PriceExtension{samplePrice()}

	got, err := validatePriceExtensions(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].PriceAsset == nil {
		t.Fatalf("got %+v, want one price asset", got)
	}
	asset := got[0].PriceAsset
	if len(asset.PriceOfferings) != 3 {
		t.Fatalf("got %d offerings, want 3", len(asset.PriceOfferings))
	}
	// Each ROW is separately clickable, so each row's URL has to be tagged — a
	// table whose first row attributes correctly and whose others do not is the
	// failure mode a single top-level URL would produce.
	for i, o := range asset.PriceOfferings {
		if len(o.FinalURLs) != 1 {
			t.Fatalf("offering %d has %d final URLs, want 1", i, len(o.FinalURLs))
		}
		for _, want := range []string{"utm_source=google", "utm_medium=cpc"} {
			if !strings.Contains(o.FinalURLs[0], want) {
				t.Errorf("offering %d destination %q is missing %q", i, o.FinalURLs[0], want)
			}
		}
	}
	if asset.PriceOfferings[0].Price != (money{CurrencyCode: "USD", AmountMicros: 650_000_000}) {
		t.Errorf("price = %+v, want USD 650000000 micros", asset.PriceOfferings[0].Price)
	}
	// The whole price asset is one arm of the oneof, and carries no asset-level
	// destination of its own — the rows own the URLs.
	if got[0].FinalURLs != nil {
		t.Errorf("a price asset carries no asset-level final URL, got %v", got[0].FinalURLs)
	}
}

func TestValidatePrices_OmitsUnsetOptionalKeys(t *testing.T) {
	in := sampleInput()
	in.Prices = []PriceExtension{samplePrice()}
	got, err := validatePriceExtensions(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"priceQualifier", "unit"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("unset %s must be omitted, got %s", key, b)
		}
	}
}

func TestValidatePrices_RejectsBadInput(t *testing.T) {
	cases := map[string]func(p *PriceExtension){
		"no type":           func(p *PriceExtension) { p.Type = "" },
		"type as prose":     func(p *PriceExtension) { p.Type = "Events" },
		"bad qualifier":     func(p *PriceExtension) { p.PriceQualifier = "from" },
		"no language":       func(p *PriceExtension) { p.LanguageCode = "" },
		"bad language":      func(p *PriceExtension) { p.LanguageCode = "english!" },
		"too few offerings": func(p *PriceExtension) { p.Offerings = p.Offerings[:2] },
		"too many offerings": func(p *PriceExtension) {
			for len(p.Offerings) <= maxPriceOfferings {
				o := p.Offerings[0]
				o.Header = o.Header + strings.Repeat("x", len(p.Offerings))
				p.Offerings = append(p.Offerings, o)
			}
		},
		"no header":        func(p *PriceExtension) { p.Offerings[0].Header = "" },
		"header too long":  func(p *PriceExtension) { p.Offerings[0].Header = strings.Repeat("x", maxPriceHeaderRunes+1) },
		"duplicate header": func(p *PriceExtension) { p.Offerings[1].Header = "attendee" },
		"no description":   func(p *PriceExtension) { p.Offerings[0].Description = "" },
		"description too long": func(p *PriceExtension) {
			p.Offerings[0].Description = strings.Repeat("x", maxPriceDescRunes+1)
		},
		"zero price":      func(p *PriceExtension) { p.Offerings[0].Amount = 0 },
		"negative price":  func(p *PriceExtension) { p.Offerings[0].Amount = -1 },
		"sub-micro price": func(p *PriceExtension) { p.Offerings[0].Amount = 0.0000001 },
		"no currency":     func(p *PriceExtension) { p.Offerings[0].CurrencyCode = "" },
		"bad unit":        func(p *PriceExtension) { p.Offerings[0].Unit = "per day" },
		"no destination":  func(p *PriceExtension) { p.Offerings[0].FinalURL = "" },
	}
	in := sampleInput()
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := samplePrice()
			// Deep-copy the offerings so one case's mutation cannot reach another's.
			p.Offerings = append([]PriceOffering(nil), p.Offerings...)
			mutate(&p)
			in.Prices = []PriceExtension{p}
			if _, err := validatePriceExtensions(in); err == nil {
				t.Fatalf("%s must be refused", name)
			}
		})
	}
}

// NaN and Inf are checked explicitly rather than left to the bounds comparison:
// every comparison against NaN is false, so a bare `amount > max` check would
// pass it straight through to a multiply that yields a meaningless micros value.
func TestValidateMoney_RejectsNonFiniteAmounts(t *testing.T) {
	for name, amount := range map[string]float64{
		"NaN":  math.NaN(),
		"+Inf": math.Inf(1),
		"-Inf": math.Inf(-1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateMoney("test amount", amount, "USD"); err == nil {
				t.Fatalf("%s must be refused", name)
			}
		})
	}
	if _, err := validatePercentOff("test", math.NaN()); err == nil {
		t.Fatal("a NaN percentage must be refused")
	}
}

// ---------------------------------------------------------------------------
// The plan
// ---------------------------------------------------------------------------

// The field type is carried positionally and cannot be read back off a created
// asset, so a plan whose two slices drift links every asset under the wrong slot.
// This asserts the pairing across all SEVEN extension types at once.
func TestValidateAssetPlan_PairsEveryTypeWithItsFieldType(t *testing.T) {
	in := sampleInput()
	in.Sitelinks = []Sitelink{sampleSitelink()}
	in.Callouts = []string{"Free to attend"}
	in.StructuredSnippets = []StructuredSnippet{{Header: "Brands", Values: []string{"a", "b", "c"}}}
	in.CallExtensions = []CallExtension{sampleCallExtension()}
	in.Promotions = []PromotionExtension{samplePromotion()}
	in.Prices = []PriceExtension{samplePrice()}
	in.LeadForms = []LeadFormExtension{sampleLeadForm()}

	plan, err := validateAssetPlan(campaignKindSearch, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.assets) != len(plan.fieldTypes) {
		t.Fatalf("assets (%d) and fieldTypes (%d) must stay the same length", len(plan.assets), len(plan.fieldTypes))
	}
	want := []string{
		assetFieldSitelink, assetFieldCallout, assetFieldStructuredSnippet,
		assetFieldCall, assetFieldPromotion, assetFieldPrice, assetFieldLeadForm,
	}
	if len(plan.fieldTypes) != len(want) {
		t.Fatalf("got %d field types, want %d", len(plan.fieldTypes), len(want))
	}
	for i := range want {
		if plan.fieldTypes[i] != want[i] {
			t.Errorf("fieldTypes[%d] = %q, want %q", i, plan.fieldTypes[i], want[i])
		}
	}
	if plan.calls != 1 || plan.promotions != 1 || plan.prices != 1 || plan.leadForms != 1 {
		t.Errorf("counts = %d/%d/%d/%d, want 1/1/1/1", plan.calls, plan.promotions, plan.prices, plan.leadForms)
	}
	step := assetStep(plan)
	for _, want := range []string{"1 call extensions", "1 promotions", "1 prices", "1 lead forms"} {
		if !strings.Contains(step, want) {
			t.Errorf("assetStep %q is missing %q", step, want)
		}
	}
}

// A step sentence must name only what was asked for: a promotion clause on a
// campaign with no promotion asset is a lie about a paid resource.
func TestAssetStep_OmitsTheNewClausesWhenUnasked(t *testing.T) {
	in := sampleInput()
	in.CallExtensions = []CallExtension{sampleCallExtension()}
	plan, err := validateAssetPlan(campaignKindSearch, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if step := assetStep(plan); step != "1 call extensions" {
		t.Errorf("assetStep = %q, want only the call clause", step)
	}
}

// Every extension type must reach the plan and be named in the step sentence.
// A type wired into validateAssetPlan but missing from assetStep creates paid
// assets the campaign result never mentions — and a type missing from the
// `asked` count takes the zero-asked early return and is silently dropped.
func TestValidateAssetPlan_CountsAndNamesAllSevenExtensionTypes(t *testing.T) {
	in := CampaignInput{
		Sitelinks:          []Sitelink{sampleSitelink()},
		Callouts:           []string{"Free workshops"},
		StructuredSnippets: []StructuredSnippet{{Header: "Courses", Values: []string{"Kubernetes", "Observability", "Security"}}},
		CallExtensions:     []CallExtension{sampleCallExtension()},
		Promotions:         []PromotionExtension{samplePromotion()},
		Prices:             []PriceExtension{samplePrice()},
		LeadForms:          []LeadFormExtension{sampleLeadForm()},
	}
	plan, err := validateAssetPlan(campaignKindSearch, in)
	if err != nil {
		t.Fatalf("validateAssetPlan: %v", err)
	}
	if plan.count() != 7 {
		t.Errorf("plan holds %d assets, want 7: %+v", plan.count(), plan)
	}
	if len(plan.fieldTypes) != plan.count() {
		t.Errorf("assets and fieldTypes are positionally paired; got %d and %d", plan.count(), len(plan.fieldTypes))
	}
	want := "1 sitelinks, 1 callouts, 1 structured snippets, 1 call extensions, 1 promotions, 1 prices, 1 lead forms"
	if got := assetStep(plan); got != want {
		t.Errorf("assetStep = %q, want %q", got, want)
	}
}

// Each new type ALONE must leave the zero-asked early return. A type left out of
// the `asked` sum returns an empty plan here while every mixed fixture still
// passes, because some other type keeps the count above zero.
func TestValidateAssetPlan_EachNewTypeAloneIsPlanned(t *testing.T) {
	cases := map[string]func(in *CampaignInput){
		"call extensions": func(in *CampaignInput) { in.CallExtensions = []CallExtension{sampleCallExtension()} },
		"promotions":      func(in *CampaignInput) { in.Promotions = []PromotionExtension{samplePromotion()} },
		"prices":          func(in *CampaignInput) { in.Prices = []PriceExtension{samplePrice()} },
		"lead forms":      func(in *CampaignInput) { in.LeadForms = []LeadFormExtension{sampleLeadForm()} },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			var in CampaignInput
			set(&in)
			plan, err := validateAssetPlan(campaignKindSearch, in)
			if err != nil {
				t.Fatalf("validateAssetPlan: %v", err)
			}
			if plan.count() == 0 {
				t.Fatalf("%s alone produced an empty plan; it is missing from the asked count", name)
			}
		})
	}
}

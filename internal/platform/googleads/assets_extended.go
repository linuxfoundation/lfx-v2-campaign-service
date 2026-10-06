// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Call, promotion and price extensions (LFXV2-2665)
//
// Three more members of the asset oneof assets.go already creates and links.
// Everything structural — the two mutates, the positional asset/fieldType
// pairing, the resource-name verification, the SEARCH-ONLY fence — is that
// file's and is not repeated here. What lives here is each type's own shape and
// the validation that has to happen BEFORE the budget mutate.
//
// Why these three and not the other three Google publishes:
//
//   - IMAGE assets carry bytes. Creating one means fetching the image, which
//     would give the Search cascade a network phase it does not have today; it
//     is a separate change rather than another validator, and is a named gap.
//   - LOCATION assets are not created by this API at all. They come from a
//     linked Business Profile account, and `locationAsset` references a
//     Business Profile location id rather than an address this service could
//     send. An account with no such link cannot have one, whatever this client
//     does, so there is nothing here to implement — also a named gap.
//   - LEAD_FORM assets are creatable and are the obvious next one; they carry a
//     field-type enum and a privacy-policy URL of their own, and are deliberately
//     left to their own change rather than squeezed in alongside these.
//
// Enum-valued fields (occasion, price type, price qualifier, price unit) are
// validated for SHAPE and not for membership, exactly as the structured-snippet
// header is in assets.go and for the same reason: Google adds values to these
// enums without this client's involvement, and a hardcoded list refuses a create
// Google would have accepted. The shape check still catches a lowercase value or
// free text, which is what a caller actually gets wrong.
// ---------------------------------------------------------------------------

// Payload bounds, not upstream limits. Google's documented per-campaign ceiling
// for linked assets is 20, which is what sitelinks and callouts already use; a
// campaign wanting more price or call extensions than this is not a campaign
// this service has a use for, and the bound keeps one malformed brief from
// sending a thousand-operation mutate. Each refusal says which kind of bound it
// is, so a reader widening one knows whether they are arguing with Google.
const (
	maxCallExtensions      = 20
	maxPromotionExtensions = 20
	maxPriceExtensions     = 10
)

// Per-asset bounds, in Google's documented limits.
const (
	maxPromotionTargetRunes = 25
	maxPromotionCodeRunes   = 20
	maxPriceHeaderRunes     = 25
	maxPriceDescRunes       = 25
	minPriceOfferings       = 3
	maxPriceOfferings       = 8
	// maxPhoneNumberRunes bounds the payload. Google validates the number
	// against the country itself and this client cannot, so the shape check
	// below is deliberately loose and this is only a ceiling on what is sent.
	maxPhoneNumberRunes = 35
	// minPhoneDigits refuses a "number" with no number in it. Four is below any
	// real phone number anywhere, which is the point: it catches an empty or
	// punctuation-only string without taking a position on national formats.
	minPhoneDigits = 4
)

// Asset field types for the three kinds this file adds.
const (
	assetFieldCall      = "CALL"
	assetFieldPromotion = "PROMOTION"
	assetFieldPrice     = "PRICE"
)

// percentOffScale converts a human percentage to the units Google's
// PromotionAsset.percent_off takes, where 1,000,000 means 100%. So 25% is
// 250,000 — micros of a FRACTION, not micros of a percentage, and getting that
// wrong by a factor of 100 is the kind of error that shows up as a live discount.
const percentOffScale = 10_000

// enumShapeRE is the shape every Google enum name has: upper-case ASCII letters,
// digits and underscores, starting with a letter. It is not a membership test —
// see this file's header for why there deliberately is not one.
var enumShapeRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// phoneAllowedRE is every character a phone number may contain here. Letters are
// refused: Google rejects vanity numbers on call assets, and a caller who typed
// one has made a mistake this client can name before anything is paid for.
var phoneAllowedRE = regexp.MustCompile(`^[0-9+\-(). ]+$`)

// currencyCodeRE is the ISO 4217 shape. Membership is Google's to judge, for the
// same reason the enums above are.
var currencyCodeRE = regexp.MustCompile(`^[A-Z]{3}$`)

// languageCodeRE is loose on purpose: Google takes "en", "pt-BR" and "zh-Hant"
// alike here, and a tighter rule would refuse a locale that works.
var languageCodeRE = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// CallExtension is a phone number shown with the ad.
type CallExtension struct {
	// CountryCode is the ISO alpha-2 country the number belongs to. Google needs
	// it to interpret the number and refuses the asset without it.
	CountryCode string
	// PhoneNumber is the number as a human would write it for that country.
	// Google normalises it; this client does not try to.
	PhoneNumber string
}

// PromotionExtension is a discount shown with the ad.
//
// Exactly ONE of DiscountPercent and DiscountAmount must be set — they are two
// arms of a Google oneof, and a promotion that is neither has nothing to
// promote. At most one of PromotionCode and OrdersOverAmount may be set, for the
// same reason: they are the other oneof, and both are optional.
type PromotionExtension struct {
	// PromotionTarget is what is on sale ("Conference passes"). Required.
	PromotionTarget string
	// DiscountPercent is a percentage, 0 < p <= 100, as a human writes it.
	DiscountPercent float64
	// DiscountAmount is a money amount off, in CurrencyCode units.
	DiscountAmount float64
	// OrdersOverAmount is the minimum spend the promotion applies above, in
	// CurrencyCode units.
	OrdersOverAmount float64
	// CurrencyCode is required when DiscountAmount or OrdersOverAmount is set,
	// and meaningless otherwise.
	CurrencyCode string
	// PromotionCode is the coupon code, if the promotion has one.
	PromotionCode string
	// Occasion is Google's PromotionExtensionOccasion enum name, e.g.
	// "BLACK_FRIDAY". Optional, shape-checked only.
	Occasion string
	// LanguageCode is the promotion text's language. Optional.
	LanguageCode string
	// StartDate/EndDate bound when the asset serves; RedemptionStartDate and
	// RedemptionEndDate bound when the offer can be redeemed. All YYYY-MM-DD,
	// all optional, and each pair is checked for ordering independently — they
	// are different windows and Google does not require them to agree.
	StartDate           string
	EndDate             string
	RedemptionStartDate string
	RedemptionEndDate   string
	// FinalURL is where the promotion clicks through to. Required, and tagged
	// exactly as the ad's own final URL is — a promotion click IS an ad click.
	FinalURL string
}

// PriceExtension is a table of priced offerings shown with the ad.
type PriceExtension struct {
	// Type is Google's PriceExtensionType enum name, e.g. "EVENTS". Required,
	// shape-checked only.
	Type string
	// PriceQualifier is the PriceExtensionPriceQualifier enum name, e.g.
	// "FROM". Optional, shape-checked only.
	PriceQualifier string
	// LanguageCode is the offerings' language. Required — Google refuses a
	// price asset without one, and defaulting it here would pick a language on
	// the caller's behalf.
	LanguageCode string
	// Offerings are the rows of the table. Google requires 3 to 8.
	Offerings []PriceOffering
}

// PriceOffering is one row of a price extension.
type PriceOffering struct {
	Header      string
	Description string
	// Amount is the price in CurrencyCode units. Must be > 0: a free offering
	// is not a price extension row, and 0 reaches Google as "unset".
	Amount       float64
	CurrencyCode string
	// Unit is Google's PriceExtensionPriceUnit enum name, e.g. "PER_DAY".
	// Optional, shape-checked only.
	Unit string
	// FinalURL is this row's destination. Required and tagged, as the
	// promotion's is.
	FinalURL string
}

// ---------------------------------------------------------------------------
// Wire payloads
// ---------------------------------------------------------------------------

type callAsset struct {
	CountryCode string `json:"countryCode"`
	PhoneNumber string `json:"phoneNumber"`
}

// money is Google's Money message. AmountMicros is rendered as a JSON number
// here, as every other amount this client sends is.
type money struct {
	CurrencyCode string `json:"currencyCode"`
	AmountMicros int64  `json:"amountMicros"`
}

type promotionAsset struct {
	PromotionTarget string `json:"promotionTarget"`
	// Exactly one of these two is ever set — the discount oneof.
	PercentOff     int64  `json:"percentOff,omitempty"`
	MoneyAmountOff *money `json:"moneyAmountOff,omitempty"`
	// At most one of these two — the eligibility oneof. Both omitempty: an
	// empty promotion code is not the same as no promotion code.
	PromotionCode    string `json:"promotionCode,omitempty"`
	OrdersOverAmount *money `json:"ordersOverAmount,omitempty"`

	Occasion            string `json:"occasion,omitempty"`
	LanguageCode        string `json:"languageCode,omitempty"`
	StartDate           string `json:"startDate,omitempty"`
	EndDate             string `json:"endDate,omitempty"`
	RedemptionStartDate string `json:"redemptionStartDate,omitempty"`
	RedemptionEndDate   string `json:"redemptionEndDate,omitempty"`
}

type priceOfferingPayload struct {
	Header      string   `json:"header"`
	Description string   `json:"description"`
	Price       money    `json:"price"`
	Unit        string   `json:"unit,omitempty"`
	FinalURLs   []string `json:"finalUrls"`
}

type priceAsset struct {
	Type           string                 `json:"type"`
	PriceQualifier string                 `json:"priceQualifier,omitempty"`
	LanguageCode   string                 `json:"languageCode"`
	PriceOfferings []priceOfferingPayload `json:"priceOfferings"`
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// validateCallExtensions checks each call extension and builds its asset.
func validateCallExtensions(calls []CallExtension) ([]assetCreate, error) {
	if len(calls) > maxCallExtensions {
		return nil, fmt.Errorf("google-ads campaign accepts at most %d call extensions in one request (a bound on this request's payload, not an upstream limit), got %d", maxCallExtensions, len(calls))
	}
	out := make([]assetCreate, 0, len(calls))
	seen := make(map[string]struct{}, len(calls))
	for i, c := range calls {
		country := strings.ToUpper(strings.TrimSpace(c.CountryCode))
		if country == "" {
			return nil, fmt.Errorf("google-ads call extension %d has no country code; Google cannot interpret a phone number without one", i)
		}
		if len(country) != 2 || !isASCIILetters(country) {
			return nil, fmt.Errorf("google-ads call extension %d country code %q must be an ISO alpha-2 code such as US or DE", i, capForError(country))
		}
		number := strings.TrimSpace(c.PhoneNumber)
		if number == "" {
			return nil, fmt.Errorf("google-ads call extension %d has no phone number", i)
		}
		if n := utf8.RuneCountInString(number); n > maxPhoneNumberRunes {
			return nil, fmt.Errorf("google-ads call extension %d phone number is %d characters, exceeding the %d this client sends", i, n, maxPhoneNumberRunes)
		}
		if !phoneAllowedRE.MatchString(number) {
			return nil, fmt.Errorf("google-ads call extension %d phone number %q may contain only digits and the separators + - ( ) . and space; Google refuses vanity numbers on call assets", i, number)
		}
		if countDigits(number) < minPhoneDigits {
			return nil, fmt.Errorf("google-ads call extension %d phone number %q has fewer than %d digits", i, number, minPhoneDigits)
		}
		// Two identical numbers on one campaign render as one extension, and the
		// caller who listed it twice meant once — the sitelink rule.
		key := country + "|" + stripNonDigits(number)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads call extension %q (%s) is listed more than once", number, country)
		}
		seen[key] = struct{}{}
		out = append(out, assetCreate{CallAsset: &callAsset{CountryCode: country, PhoneNumber: number}})
	}
	return out, nil
}

// validatePromotionExtensions checks each promotion and builds its asset. The
// final URL is tagged exactly like the ad's own, for the reason validateSitelinks
// gives: an untagged promotion click lands in the attribution data as organic
// traffic the campaign paid for.
func validatePromotionExtensions(in CampaignInput) ([]assetCreate, error) {
	promos := in.Promotions
	if len(promos) > maxPromotionExtensions {
		return nil, fmt.Errorf("google-ads campaign accepts at most %d promotion extensions in one request (a bound on this request's payload, not an upstream limit), got %d", maxPromotionExtensions, len(promos))
	}
	out := make([]assetCreate, 0, len(promos))
	seen := make(map[string]struct{}, len(promos))
	for i, p := range promos {
		target := strings.TrimSpace(p.PromotionTarget)
		if target == "" {
			return nil, fmt.Errorf("google-ads promotion extension %d has no promotion target; Google needs to know what is on sale", i)
		}
		if n := utf8.RuneCountInString(target); n > maxPromotionTargetRunes {
			return nil, fmt.Errorf("google-ads promotion extension %d target is %d characters, exceeding the %d limit", i, n, maxPromotionTargetRunes)
		}
		key := strings.ToLower(target)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads promotion extension target %q is listed more than once", target)
		}
		seen[key] = struct{}{}

		asset := &promotionAsset{PromotionTarget: target}

		// The discount oneof. Both set and neither set are both refusals: a
		// promotion with two discounts is ambiguous, and one with none is not a
		// promotion. Checked before anything else about the amounts so the
		// message names the real problem rather than a bound.
		hasPercent := p.DiscountPercent != 0
		hasAmount := p.DiscountAmount != 0
		switch {
		case hasPercent && hasAmount:
			return nil, fmt.Errorf("google-ads promotion extension %q sets both a percentage and a money discount; Google takes exactly one", target)
		case hasPercent:
			micros, err := validatePercentOff(target, p.DiscountPercent)
			if err != nil {
				return nil, err
			}
			asset.PercentOff = micros
		case hasAmount:
			m, err := validateMoney(fmt.Sprintf("promotion extension %q discount amount", target), p.DiscountAmount, p.CurrencyCode)
			if err != nil {
				return nil, err
			}
			asset.MoneyAmountOff = &m
		default:
			return nil, fmt.Errorf("google-ads promotion extension %q sets neither a percentage nor a money discount; one of the two is what makes it a promotion", target)
		}

		// The eligibility oneof, where BOTH absent is fine.
		code := strings.TrimSpace(p.PromotionCode)
		hasOrdersOver := p.OrdersOverAmount != 0
		if code != "" && hasOrdersOver {
			return nil, fmt.Errorf("google-ads promotion extension %q sets both a promotion code and a minimum order amount; Google takes at most one", target)
		}
		switch {
		case code != "":
			if n := utf8.RuneCountInString(code); n > maxPromotionCodeRunes {
				return nil, fmt.Errorf("google-ads promotion extension %q promotion code is %d characters, exceeding the %d limit", target, n, maxPromotionCodeRunes)
			}
			asset.PromotionCode = code
		case hasOrdersOver:
			m, err := validateMoney(fmt.Sprintf("promotion extension %q minimum order amount", target), p.OrdersOverAmount, p.CurrencyCode)
			if err != nil {
				return nil, err
			}
			asset.OrdersOverAmount = &m
		}

		if occ := strings.TrimSpace(p.Occasion); occ != "" {
			if !enumShapeRE.MatchString(occ) {
				return nil, fmt.Errorf("google-ads promotion extension %q occasion %q is not an occasion name; Google spells these in upper case with underscores, such as BLACK_FRIDAY", target, capForError(occ))
			}
			asset.Occasion = occ
		}
		if lang := strings.TrimSpace(p.LanguageCode); lang != "" {
			if !languageCodeRE.MatchString(lang) {
				return nil, fmt.Errorf("google-ads promotion extension %q language code %q is not a language tag such as en or pt-BR", target, capForError(lang))
			}
			asset.LanguageCode = lang
		}

		// Two independent windows, each ordered on its own. Google does not
		// require the redemption window to sit inside the serving one — an offer
		// can be redeemable after the ad stops running — so neither does this.
		var err error
		if asset.StartDate, asset.EndDate, err = validateAssetDateWindow(fmt.Sprintf("promotion extension %q serving window", target), p.StartDate, p.EndDate); err != nil {
			return nil, err
		}
		if asset.RedemptionStartDate, asset.RedemptionEndDate, err = validateAssetDateWindow(fmt.Sprintf("promotion extension %q redemption window", target), p.RedemptionStartDate, p.RedemptionEndDate); err != nil {
			return nil, err
		}

		finalURL, err := buildTaggedFinalURL(fmt.Sprintf("promotion extension %q destination URL", target), p.FinalURL, in.EventSlug, in.EventName, in.Project, in.NameSuffix)
		if err != nil {
			return nil, fmt.Errorf("google-ads promotion extension %d is unservable: %w", i, err)
		}
		if n := len(finalURL); n > maxFinalURLBytes {
			return nil, fmt.Errorf("google-ads promotion extension %q destination URL is %d bytes, exceeding the %d limit", target, n, maxFinalURLBytes)
		}

		out = append(out, assetCreate{FinalURLs: []string{finalURL}, PromotionAsset: asset})
	}
	return out, nil
}

// validatePriceExtensions checks each price extension and builds its asset.
func validatePriceExtensions(in CampaignInput) ([]assetCreate, error) {
	prices := in.Prices
	if len(prices) > maxPriceExtensions {
		return nil, fmt.Errorf("google-ads campaign accepts at most %d price extensions in one request (a bound on this request's payload, not an upstream limit), got %d", maxPriceExtensions, len(prices))
	}
	out := make([]assetCreate, 0, len(prices))
	for i, p := range prices {
		kind := strings.TrimSpace(p.Type)
		if kind == "" {
			return nil, fmt.Errorf("google-ads price extension %d has no type; Google needs to know what kind of table this is, such as EVENTS or SERVICES", i)
		}
		if !enumShapeRE.MatchString(kind) {
			return nil, fmt.Errorf("google-ads price extension %d type %q is not a type name; Google spells these in upper case with underscores, such as PRODUCT_TIERS", i, capForError(kind))
		}
		asset := &priceAsset{Type: kind}

		if q := strings.TrimSpace(p.PriceQualifier); q != "" {
			if !enumShapeRE.MatchString(q) {
				return nil, fmt.Errorf("google-ads price extension %d price qualifier %q is not a qualifier name; Google spells these in upper case, such as FROM or UP_TO", i, capForError(q))
			}
			asset.PriceQualifier = q
		}

		lang := strings.TrimSpace(p.LanguageCode)
		if lang == "" {
			return nil, fmt.Errorf("google-ads price extension %d has no language code; Google refuses a price asset without one and this client will not pick a language for you", i)
		}
		if !languageCodeRE.MatchString(lang) {
			return nil, fmt.Errorf("google-ads price extension %d language code %q is not a language tag such as en or pt-BR", i, capForError(lang))
		}
		asset.LanguageCode = lang

		if len(p.Offerings) < minPriceOfferings || len(p.Offerings) > maxPriceOfferings {
			return nil, fmt.Errorf("google-ads price extension %d has %d offerings; Google requires between %d and %d", i, len(p.Offerings), minPriceOfferings, maxPriceOfferings)
		}
		seenHeaders := make(map[string]struct{}, len(p.Offerings))
		for j, o := range p.Offerings {
			label := fmt.Sprintf("price extension %d offering %d", i, j)
			header := strings.TrimSpace(o.Header)
			if header == "" {
				return nil, fmt.Errorf("google-ads %s has no header", label)
			}
			if n := utf8.RuneCountInString(header); n > maxPriceHeaderRunes {
				return nil, fmt.Errorf("google-ads %s header is %d characters, exceeding the %d limit", label, n, maxPriceHeaderRunes)
			}
			// Google renders the table by header and serves one row per header,
			// so a repeat is a row the caller would silently lose.
			hkey := strings.ToLower(header)
			if _, dup := seenHeaders[hkey]; dup {
				return nil, fmt.Errorf("google-ads price extension %d lists the header %q more than once; Google serves one row per header", i, header)
			}
			seenHeaders[hkey] = struct{}{}

			desc := strings.TrimSpace(o.Description)
			if desc == "" {
				return nil, fmt.Errorf("google-ads %s has no description", label)
			}
			if n := utf8.RuneCountInString(desc); n > maxPriceDescRunes {
				return nil, fmt.Errorf("google-ads %s description is %d characters, exceeding the %d limit", label, n, maxPriceDescRunes)
			}

			amount, err := validateMoney(label+" price", o.Amount, o.CurrencyCode)
			if err != nil {
				return nil, err
			}

			row := priceOfferingPayload{Header: header, Description: desc, Price: amount}
			if u := strings.TrimSpace(o.Unit); u != "" {
				if !enumShapeRE.MatchString(u) {
					return nil, fmt.Errorf("google-ads %s unit %q is not a unit name; Google spells these in upper case, such as PER_DAY", label, capForError(u))
				}
				row.Unit = u
			}

			finalURL, err := buildTaggedFinalURL(label+" destination URL", o.FinalURL, in.EventSlug, in.EventName, in.Project, in.NameSuffix)
			if err != nil {
				return nil, fmt.Errorf("google-ads %s is unservable: %w", label, err)
			}
			if n := len(finalURL); n > maxFinalURLBytes {
				return nil, fmt.Errorf("google-ads %s destination URL is %d bytes, exceeding the %d limit", label, n, maxFinalURLBytes)
			}
			row.FinalURLs = []string{finalURL}

			asset.PriceOfferings = append(asset.PriceOfferings, row)
		}
		out = append(out, assetCreate{PriceAsset: asset})
	}
	return out, nil
}

// validatePercentOff converts a human percentage into Google's percent_off
// units. The bound is the percentage, checked before the multiply, so the
// conversion cannot be reached with a value that would overflow it.
func validatePercentOff(target string, percent float64) (int64, error) {
	if math.IsNaN(percent) || math.IsInf(percent, 0) {
		return 0, fmt.Errorf("google-ads promotion extension %q discount percentage must be a finite number, got %v", target, percent)
	}
	if percent <= 0 || percent > 100 {
		return 0, fmt.Errorf("google-ads promotion extension %q discount percentage must be greater than 0 and at most 100, got %v", target, percent)
	}
	// Rounded rather than truncated, for the reason ValidateBudgetMicros gives:
	// 12.3 * 10000 is 122999.99… in float64.
	micros := int64(math.Round(percent * percentOffScale))
	if micros <= 0 {
		return 0, fmt.Errorf("google-ads promotion extension %q discount percentage %v is too small to express (rounds to 0)", target, percent)
	}
	return micros, nil
}

// validateMoney converts an amount plus a currency code into Google's Money
// message, applying the same four checks ValidateBudgetMicros applies to the
// budget — NaN/Inf explicitly, the bound before the multiply, rounding rather
// than truncation, and the <= 0 check AFTER the conversion so a sub-micro
// amount is caught.
func validateMoney(label string, amount float64, currencyCode string) (money, error) {
	currency := strings.ToUpper(strings.TrimSpace(currencyCode))
	if currency == "" {
		return money{}, fmt.Errorf("google-ads %s has an amount but no currency code; Google cannot interpret a bare number", label)
	}
	if !currencyCodeRE.MatchString(currency) {
		return money{}, fmt.Errorf("google-ads %s currency code %q must be a three-letter ISO 4217 code such as USD", label, currencyCode)
	}
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return money{}, fmt.Errorf("google-ads %s must be a finite number, got %v", label, amount)
	}
	if amount > maxBudget {
		return money{}, fmt.Errorf("google-ads %s %.2f exceeds the maximum %.0f", label, amount, maxBudget)
	}
	micros := int64(math.Round(amount * microsPerUnit))
	if micros <= 0 {
		return money{}, fmt.Errorf("google-ads %s must be > 0 (rounds to %d micros), got %.6f", label, micros, amount)
	}
	return money{CurrencyCode: currency, AmountMicros: micros}, nil
}

// validateAssetDateWindow validates an optional YYYY-MM-DD pair and returns it
// unchanged. Unlike the campaign's flight window these go to Google as bare
// dates, so there is no rendering step — only the format, calendar-validity and
// ordering checks, and the same-day case is accepted for the same reason.
func validateAssetDateWindow(label, startDate, endDate string) (string, string, error) {
	var start, end time.Time
	var err error
	startDate = strings.TrimSpace(startDate)
	endDate = strings.TrimSpace(endDate)
	if startDate != "" {
		if !campaignDateRE.MatchString(startDate) {
			return "", "", fmt.Errorf("google-ads %s start date %q is not in YYYY-MM-DD format", label, startDate)
		}
		if start, err = time.Parse(campaignDateOnlyLayout, startDate); err != nil {
			return "", "", fmt.Errorf("google-ads %s start date %q is not a valid calendar date: %w", label, startDate, err)
		}
	}
	if endDate != "" {
		if !campaignDateRE.MatchString(endDate) {
			return "", "", fmt.Errorf("google-ads %s end date %q is not in YYYY-MM-DD format", label, endDate)
		}
		if end, err = time.Parse(campaignDateOnlyLayout, endDate); err != nil {
			return "", "", fmt.Errorf("google-ads %s end date %q is not a valid calendar date: %w", label, endDate, err)
		}
	}
	if startDate != "" && endDate != "" && end.Before(start) {
		return "", "", fmt.Errorf("google-ads %s end date %s must not be before start date %s", label, endDate, startDate)
	}
	return startDate, endDate, nil
}

// isASCIILetters reports whether s is non-empty and every rune is an ASCII
// letter. Used for the country code, where a two-rune non-Latin string would
// otherwise pass a bare length check.
func isASCIILetters(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

func countDigits(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n
}

func stripNonDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

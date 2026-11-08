// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ---------------------------------------------------------------------------
// Create-time location and language targeting (#564, gaps 1 and 2)
//
// Before this, CreateCampaign created the line item with NO targeting criteria, and X documents
// that "if no targeting criteria is specified for a line item, the line item will target all
// users worldwide" (Campaign Management guide). twitterConfig.geoTargets and .languages now add
// LOCATION and LANGUAGE criteria to the line item THIS call creates.
//
// X Ads API v12 reference (https://docs.x.com/x-ads-api/campaign-management/reference, read
// 2026-11-07):
//   - GET targeting_criteria/locations takes location_type (COUNTRIES, REGIONS, METROS, CITIES,
//     POSTAL_CODES), country_code ("the 2 letter ISO country code"), count (max 1000) and cursor,
//     and answers entries carrying name, country_code, location_type, targeting_type LOCATION and
//     the targeting_value to send.
//   - GET targeting_criteria/languages takes count (max 1000), cursor and q, and answers entries
//     carrying name, targeting_type LANGUAGE and targeting_value (its example: "en").
//   - POST accounts/:account_id/targeting_criteria takes line_item_id, operator_type (EQ, NE, GTE,
//     LT), targeting_type and targeting_value as query parameters, like every other v12 create.
//
// Both lookups are NOT account-scoped ({base}/{version}/targeting_criteria/...), and both are
// reads, so they run in the pre-create block: a value X does not know is refused with nothing
// created. The criteria are POSTed one at a time, paced and sent once, after the line item and
// before the promoted tweet — the batch endpoint would need a JSON body this client does not send,
// and stopping before the tweet keeps the one irreversible artifact (a published tweet) out of a
// create whose targeting failed.
// ---------------------------------------------------------------------------

const (
	// FieldGeoTargets and FieldLanguages are the twitterConfig keys, named in refusals.
	FieldGeoTargets = "geoTargets"
	FieldLanguages  = "languages"

	// MaxGeoTargets caps CampaignInput.GeoTargets (X allows up to 2,000 locations; 50 countries is
	// far above any LF event and bounds the pre-create lookups, one per country).
	MaxGeoTargets = 50
	// MaxLanguages caps CampaignInput.Languages.
	MaxLanguages = 20

	// MaxCreateTargetingCriteria caps the targeting criteria ONE create posts, across geoTargets,
	// languages, keywords, interestIds, customAudienceIds and excludedCustomAudienceIds together
	// (after de-duplication).
	// Each criterion is its own POST, paced at one write per second (writeDelay) and sent once,
	// and the whole create runs inside the orchestrator's two-minute dispatch budget
	// (service.providerCallTimeout) alongside the account read, the lookups and the campaign,
	// line item, media and tweet writes. Sixty paced criteria leave that budget about a minute of
	// headroom; more could not finish, and would leave a retained PAUSED partial whose targeting
	// needs completing by hand. TestCreateTargetingCriteriaFitTheDispatchBudget pins the margin.
	MaxCreateTargetingCriteria = 60

	// TargetingLocation and TargetingLanguage are X's targeting_type values.
	TargetingLocation = "LOCATION"
	TargetingLanguage = "LANGUAGE"

	// FieldKeywords and FieldInterestIDs are the gap 4 twitterConfig keys (#564).
	FieldKeywords    = "keywords"
	FieldInterestIDs = "interestIds"
	// MaxCreateKeywords and MaxCreateInterests cap each gap 4 list; the per-create total
	// (MaxCreateTargetingCriteria) bounds them together with the others.
	MaxCreateKeywords  = MaxCreateTargetingCriteria
	MaxCreateInterests = MaxCreateTargetingCriteria
	// maxCreateKeywordRunes bounds one keyword.
	maxCreateKeywordRunes = 80
	// TargetingInterest is X's targeting_type for an interest criterion.
	TargetingInterest = "INTEREST"
)

var (
	countryCodeRe   = regexp.MustCompile(`^[A-Z]{2}$`)
	languageValueRe = regexp.MustCompile(`^[a-z][a-z-]{1,7}$`)
	interestIDRe    = regexp.MustCompile(`^[0-9]{1,19}$`)
)

// foldKey canonicalizes s under Unicode simple case folding: each rune becomes the smallest rune
// of its fold orbit. Two strings get the same key exactly when strings.EqualFold reports them
// equal, which is how the criterion echo and the reuse diff (createCriterion.sameValue) compare
// keyword values, so de-duplication and matching cannot disagree (strings.ToLower gives Σ and ς
// different keys although EqualFold treats them as equal).
func foldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < m {
				m = f
			}
		}
		b.WriteRune(m)
	}
	return b.String()
}

// NormalizeKeywordInterestTargeting applies the gap 4 rules (#564): keywords are trimmed, must be
// 1-80 characters with no control character, and duplicates under Unicode case folding
// (foldKey, the comparison strings.EqualFold makes) are dropped, first spelling kept; interest
// ids are trimmed, must be 1-19 decimal digits, and duplicates are dropped. Each list has a cap; a
// blank entry is refused. Pure, so the dispatcher's synchronous preflight and CreateCampaign apply
// the same rules. A nil or empty list stays nil.
func NormalizeKeywordInterestTargeting(keywords, interestIDs []string) (kws, interests []string, err error) {
	if len(keywords) > 0 {
		seen := make(map[string]struct{}, len(keywords))
		for i, raw := range keywords {
			k := strings.TrimSpace(raw)
			if k == "" {
				return nil, nil, &TargetingError{Field: FieldKeywords, Msg: fmt.Sprintf("entry %d is blank", i)}
			}
			if utf8.RuneCountInString(k) > maxCreateKeywordRunes || strings.IndexFunc(k, unicode.IsControl) >= 0 {
				return nil, nil, &TargetingError{Field: FieldKeywords, Msg: fmt.Sprintf("entry %d must be at most %d characters with no control character", i, maxCreateKeywordRunes)}
			}
			key := foldKey(k)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			kws = append(kws, k)
		}
		if len(kws) > MaxCreateKeywords {
			return nil, nil, &TargetingError{Field: FieldKeywords, Msg: fmt.Sprintf("at most %d distinct entries are accepted, got %d", MaxCreateKeywords, len(kws))}
		}
	}
	interests, err = normalizeTargetList(FieldInterestIDs, interestIDs, MaxCreateInterests, func(s string) string { return s }, interestIDRe,
		"must be an X interest id (1-19 digits, from GET targeting_criteria/interests)")
	if err != nil {
		return nil, nil, err
	}
	return kws, interests, nil
}

// TargetingError is a refusal of one create-time targeting list; Field is the twitterConfig key.
type TargetingError struct {
	Field string
	Msg   string
}

func (e *TargetingError) Error() string { return e.Field + ": " + e.Msg }

// NormalizeCreateTargeting applies the create-time targeting rules: country codes are trimmed,
// uppercased and must be two letters; language values are trimmed, lowercased and must be 2-8
// letters or hyphens; duplicates are dropped (first occurrence kept) and blanks refused; each
// list has a cap. Pure, so the dispatcher's synchronous preflight and CreateCampaign apply the
// same rules. A nil or empty list stays nil.
func NormalizeCreateTargeting(geoTargets, languages []string) (geo, langs []string, err error) {
	geo, err = normalizeTargetList(FieldGeoTargets, geoTargets, MaxGeoTargets, strings.ToUpper, countryCodeRe,
		"must be an ISO 3166-1 alpha-2 country code (two letters)")
	if err != nil {
		return nil, nil, err
	}
	langs, err = normalizeTargetList(FieldLanguages, languages, MaxLanguages, strings.ToLower, languageValueRe,
		"must be an X language value such as \"en\" (2-8 letters or hyphens)")
	if err != nil {
		return nil, nil, err
	}
	return geo, langs, nil
}

func normalizeTargetList(field string, in []string, maxLen int, fold func(string) string, shape *regexp.Regexp, shapeMsg string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for i, raw := range in {
		v := fold(strings.TrimSpace(raw))
		if v == "" {
			return nil, &TargetingError{Field: field, Msg: fmt.Sprintf("entry %d is blank", i)}
		}
		if !shape.MatchString(v) {
			return nil, &TargetingError{Field: field, Msg: fmt.Sprintf("entry %d (%s) %s", i, domain.CallerValue(raw), shapeMsg)}
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) > maxLen {
		return nil, &TargetingError{Field: field, Msg: fmt.Sprintf("at most %d distinct entries are accepted, got %d", maxLen, len(out))}
	}
	return out, nil
}

// CheckCreateCriteriaCount refuses a create whose lists name more than MaxCreateTargetingCriteria
// criteria in total. n is the count after each list's own normalization. Pure, so the
// dispatcher's synchronous preflight and CreateCampaign apply the same bound.
func CheckCreateCriteriaCount(n int) error {
	if n <= MaxCreateTargetingCriteria {
		return nil
	}
	return fmt.Errorf("%s, %s, %s, %s, %s and %s together name %d targeting criteria; at most %d are accepted per create, since each is a separate X write paced at one per second",
		FieldGeoTargets, FieldLanguages, FieldKeywords, FieldInterestIDs, FieldCustomAudienceIDs, FieldExcludedCustomAudienceIDs, n, MaxCreateTargetingCriteria)
}

// createCriterion is one criterion the create path will POST.
type createCriterion struct {
	targetingType string
	value         string
	// operator is X's operator_type; "" means EQ. Only a custom audience exclusion (#564 gap 3)
	// sends NE.
	operator string
	// label names it in steps and errors: the caller's country code or language, never X's
	// opaque location value.
	label string
}

// targetingOptionWire is one entry of a targeting_criteria/{locations,languages} lookup.
type targetingOptionWire struct {
	Name           string `json:"name"`
	CountryCode    string `json:"country_code"`
	LocationType   string `json:"location_type"`
	TargetingType  string `json:"targeting_type"`
	TargetingValue string `json:"targeting_value"`
}

// targetingOptionsURL is the NON-account-scoped lookup root.
func (c *Client) targetingOptionsURL(kind string) string {
	return fmt.Sprintf("%s/%s/targeting_criteria/%s", c.baseURL, c.apiVersion, kind)
}

// lookupTargetingOptions reads one page of a targeting_criteria lookup. The query rides on the
// wire URL; logPath stays the bare collection name.
func (c *Client) lookupTargetingOptions(ctx context.Context, kind string, query url.Values) ([]targetingOptionWire, *apiResponse, error) {
	reqURL := c.targetingOptionsURL(kind) + "?" + query.Encode()
	resp, err := c.doRequestAbs(ctx, http.MethodGet, reqURL, "targeting_criteria/"+kind, nil, true /* idempotent: a read */)
	if err != nil {
		return nil, nil, err
	}
	if resp == nil || resp.raw != nil && identityjson.Check(resp.raw) != nil {
		return nil, nil, fmt.Errorf("x targeting lookup %s: the response cannot be trusted (duplicated key or malformed text)", kind)
	}
	var opts []targetingOptionWire
	if len(resp.Data) == 0 || json.Unmarshal(resp.Data, &opts) != nil || opts == nil {
		return nil, nil, fmt.Errorf("x targeting lookup %s: the response carries no list of options", kind)
	}
	return opts, resp, nil
}

// isCountryLocationType accepts both spellings X's reference uses for a country-level entry: the
// lookup's request enum is plural (COUNTRIES, and its example answers CITIES for CITIES), while a
// criterion's echo spells the singular (CITY in the batch example).
func isCountryLocationType(t string) bool {
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "COUNTRIES", "COUNTRY":
		return true
	}
	return false
}

// resolveCountry returns X's LOCATION targeting_value for one ISO alpha-2 code. It requires
// exactly one country-level entry for that code, so a code X does not target and an ambiguous
// answer are both refused rather than guessed. A lookup that could not be read is an error.
func (c *Client) resolveCountry(ctx context.Context, code string) (string, error) {
	q := url.Values{}
	q.Set("location_type", "COUNTRIES")
	q.Set("country_code", code)
	q.Set("count", strconv.Itoa(listPageSize))
	opts, resp, err := c.lookupTargetingOptions(ctx, "locations", q)
	if err != nil {
		return "", fmt.Errorf("x location lookup for %s failed: %w", code, err)
	}
	switch v := cursorVerdict(resp); {
	case v == cursorMore:
		return "", domain.PreflightRefusalf("invalid twitterConfig.%s: X answered more than one page of country locations for %s, so which one to target cannot be told", FieldGeoTargets, code)
	case v == cursorUnknowable && len(opts) >= listPageSize:
		// A FULL page owes a cursor (the findByName / language-walk rule): without one the
		// answer cannot be confirmed complete, so "exactly one" cannot be concluded from it. An
		// unreadable lookup, not a value X refused.
		return "", fmt.Errorf("x location lookup for %s: a full page came back with no usable cursor, so the answer cannot be confirmed complete", code)
	}
	var values []string
	seen := map[string]struct{}{}
	for _, o := range opts {
		v := strings.TrimSpace(o.TargetingValue)
		if !isCountryLocationType(o.LocationType) || !strings.EqualFold(strings.TrimSpace(o.CountryCode), code) || v == "" {
			continue
		}
		if o.TargetingType != "" && o.TargetingType != TargetingLocation {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		values = append(values, v)
	}
	switch len(values) {
	case 0:
		return "", domain.PreflightRefusalf("invalid twitterConfig.%s: X offers no country location for %s", FieldGeoTargets, code)
	case 1:
		if len(values[0]) > maxTargetingValueLen {
			return "", fmt.Errorf("x location lookup for %s: the targeting value is implausibly long", code)
		}
		return values[0], nil
	default:
		return "", domain.PreflightRefusalf("invalid twitterConfig.%s: X answered %d different country locations for %s, so which one to target cannot be told", FieldGeoTargets, len(values), code)
	}
}

// maxTargetingValueLen bounds a value decoded from a lookup before it is put on a create URL.
const maxTargetingValueLen = 128

// listTargetingValues walks one non-account-scoped targeting_criteria lookup (languages,
// interests) to its end and returns the lower-cased targeting_values of entries of targetingType
// (an entry with no targeting_type is kept). It refuses to conclude from a list it could not read
// to the end: a full page with no usable cursor, the page cap, or a repeated cursor is an error.
func (c *Client) listTargetingValues(ctx context.Context, kind, targetingType string) (map[string]bool, error) {
	known := map[string]bool{}
	cursor := ""
	seenCursors := map[string]bool{}
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("x %s lookup: the list did not end within %d pages", kind, maxListPages)
		}
		q := url.Values{}
		q.Set("count", strconv.Itoa(listPageSize))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		opts, resp, err := c.lookupTargetingOptions(ctx, kind+"s", q)
		if err != nil {
			return nil, fmt.Errorf("x %s lookup failed: %w", kind, err)
		}
		for _, o := range opts {
			if o.TargetingType != "" && o.TargetingType != targetingType {
				continue
			}
			if v := strings.ToLower(strings.TrimSpace(o.TargetingValue)); v != "" {
				known[v] = true
			}
		}
		verdict := cursorVerdict(resp)
		if verdict == cursorExhausted || verdict == cursorUnknowable && len(opts) < listPageSize {
			return known, nil
		}
		if verdict == cursorUnknowable {
			return nil, fmt.Errorf("x %s lookup: a full page came back with no usable cursor, so the list cannot be confirmed complete", kind)
		}
		if seenCursors[resp.NextCursor] {
			return nil, fmt.Errorf("x %s lookup: the list repeated a page cursor", kind)
		}
		seenCursors[resp.NextCursor] = true
		cursor = resp.NextCursor
	}
}

// resolveLanguages checks every requested value against X's language list, walking its pages
// (listTargetingValues). It refuses a value X does not list.
func (c *Client) resolveLanguages(ctx context.Context, langs []string) error {
	if len(langs) == 0 {
		return nil
	}
	known, err := c.listTargetingValues(ctx, "language", TargetingLanguage)
	if err != nil {
		return err
	}
	for _, l := range langs {
		if !known[l] {
			return domain.PreflightRefusalf("invalid twitterConfig.%s: X offers no language %s", FieldLanguages, domain.CallerValue(l))
		}
	}
	return nil
}

// resolveInterests checks every requested interest id against X's GET targeting_criteria/interests
// list (#564 gap 4), walked to its end like the languages. It refuses an id X does not list.
func (c *Client) resolveInterests(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	known, err := c.listTargetingValues(ctx, "interest", TargetingInterest)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !known[strings.ToLower(id)] {
			return domain.PreflightRefusalf("invalid twitterConfig.%s: X offers no interest %s", FieldInterestIDs, domain.CallerValue(id))
		}
	}
	return nil
}

// resolveCreateTargeting turns the normalized lists into the criteria to POST, reading X's
// lookups. Called in the pre-create block, so every failure leaves nothing created.
func (c *Client) resolveCreateTargeting(ctx context.Context, geo, langs, keywords, interests []string) ([]createCriterion, error) {
	var out []createCriterion
	for _, code := range geo {
		v, err := c.resolveCountry(ctx, code)
		if err != nil {
			return nil, err
		}
		out = append(out, createCriterion{targetingType: TargetingLocation, value: v, label: "country " + code})
	}
	if err := c.resolveLanguages(ctx, langs); err != nil {
		return nil, err
	}
	for _, l := range langs {
		out = append(out, createCriterion{targetingType: TargetingLanguage, value: l, label: "language " + l})
	}
	// Keywords (#564 gap 4) need no lookup: a BROAD_KEYWORD criterion's value is the keyword text.
	// The label is a count, not the text: the steps and errors are persisted and a keyword is
	// caller free text.
	for i, k := range keywords {
		out = append(out, createCriterion{targetingType: TargetingBroadKeyword, value: k, label: fmt.Sprintf("keyword %d", i+1)})
	}
	if err := c.resolveInterests(ctx, interests); err != nil {
		return nil, err
	}
	for _, id := range interests {
		out = append(out, createCriterion{targetingType: TargetingInterest, value: id, label: "interest " + id})
	}
	return out, nil
}

// op is the criterion's operator_type, EQ unless it says NE.
func (cr createCriterion) op() string {
	if cr.operator == "" {
		return OperatorEQ
	}
	return cr.operator
}

// createTargetingCriterion POSTs one criterion to the line item and confirms the echo: a live
// criterion with a usable id, under THIS line item, of the requested type, value and operator (the
// single-create example omits operator_type, so an absent one reads as X's default EQ; an NE
// criterion must be echoed as NE, as the negative-keyword path requires). Paced and
// sent ONCE: X has no idempotency key for a criterion, so a 429 is not retried in-call and is
// classified UNCONFIRMED by createOutcomeAmbiguous, as a transport failure, 3xx or 5xx is; any
// other 4xx is a definite refusal. A 2xx that does not confirm is UNCONFIRMED.
func (c *Client) createTargetingCriterion(ctx context.Context, lineItemID string, cr createCriterion) error {
	if err := c.pace(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrWriteNotSent, err)
	}
	resp, err := c.doRequest(ctx, http.MethodPost, "targeting_criteria", map[string]string{
		"line_item_id":    lineItemID,
		"operator_type":   cr.op(),
		"targeting_type":  cr.targetingType,
		"targeting_value": cr.value,
	}, false /* sent once: a 429 is unconfirmed, never retried */)
	if err != nil {
		return err
	}
	unconfirmed := func(why string) error {
		return &transportError{Method: http.MethodPost, Path: "targeting criteria", err: fmt.Errorf("%s on line item %s: %s", cr.label, lineItemID, why)}
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return unconfirmed("acknowledged without the created criterion")
	}
	if resp.raw != nil && identityjson.Check(resp.raw) != nil {
		return unconfirmed("the 2xx response cannot be trusted: it carries a duplicated key, malformed UTF-8 or an unpaired surrogate escape")
	}
	var echo targetingCriterionWire
	if json.Unmarshal(resp.Data, &echo) != nil {
		return unconfirmed("the 2xx response is not a targeting criterion object")
	}
	tc := echo.criterion()
	switch {
	case tc.ID == "" || !accountIDRe.MatchString(tc.ID) || len(tc.ID) > maxAccountIDLen:
		return unconfirmed("acknowledged without a usable criterion id")
	case tc.LineItemID != lineItemID:
		return unconfirmed("acknowledged under another line item")
	case tc.TargetingType != cr.targetingType || tc.Deleted || !echoedOperator(tc.OperatorType, cr.op()):
		return unconfirmed("acknowledged as a criterion other than the one sent")
	case !cr.sameValue(tc.TargetingValue):
		return unconfirmed("acknowledged for another value")
	}
	return nil
}

// sameValue reports whether an echoed or listed targeting_value is this criterion's. A custom
// audience id is an opaque, case-sensitive id and must match exactly; the other values (X's
// location values and language codes) compare ignoring case.
func (cr createCriterion) sameValue(v string) bool {
	if cr.targetingType == TargetingCustomAudience {
		return v == cr.value
	}
	return strings.EqualFold(v, cr.value)
}

// echoedOperator reports whether an echoed or listed operator_type is the one wanted. An absent
// one reads as X's default EQ, so it confirms EQ and never NE.
func echoedOperator(got, want string) bool {
	if got == "" {
		return want == OperatorEQ
	}
	return got == want
}

// missingCriteria returns the wanted criteria the line item does not already hold live, as a
// criterion of the same type, value and operator. Extra criteria on the line item are left as
// they are: this create only adds.
func missingCriteria(want []createCriterion, live []TargetingCriterion) []createCriterion {
	var out []createCriterion
	for _, w := range want {
		held := false
		for _, l := range live {
			if !l.Deleted && l.TargetingType == w.targetingType && echoedOperator(l.OperatorType, w.op()) && w.sameValue(l.TargetingValue) {
				held = true
				break
			}
		}
		if !held {
			out = append(out, w)
		}
	}
	return out
}

// lineItemEntityStatus reads a line item's entity_status (GET accounts/:account_id/line_items/:id).
// Used before a reused line item's targeting is changed: only a PAUSED one may be re-targeted, since
// changing the audience of a line item that may be serving is not this create's to do.
func (c *Client) lineItemEntityStatus(ctx context.Context, lineItemID string) (string, error) {
	path, lineItemID, err := c.lineItemPath(lineItemID)
	if err != nil {
		return "", err
	}
	resp, err := c.request(ctx, http.MethodGet, path)
	if err != nil {
		return "", fmt.Errorf("read line item %s status: %w", lineItemID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" || resp.raw != nil && identityjson.Check(resp.raw) != nil {
		return "", fmt.Errorf("read line item %s status: the response carried no trustworthy line item", lineItemID)
	}
	var wire struct {
		ID           string `json:"id"`
		EntityStatus string `json:"entity_status"`
		Deleted      bool   `json:"deleted"`
	}
	if json.Unmarshal(resp.Data, &wire) != nil || strings.TrimSpace(wire.ID) != lineItemID {
		return "", fmt.Errorf("read line item %s status: the response is not this line item", lineItemID)
	}
	if wire.Deleted {
		return "DELETED", nil
	}
	return strings.ToUpper(strings.TrimSpace(wire.EntityStatus)), nil
}

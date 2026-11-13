// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// ---------------------------------------------------------------------------
// Remarketing-list audiences at create (#563, gap 4): observed (bid-only) lists and excluded
// lists on the ad group a create makes or reuses.
//
// Microsoft Advertising Campaign Management v13 reference (learn.microsoft.com, read
// 2026-11-07):
//   - AddAdGroupCriterions (POST AdGroupCriterions) takes AdGroupCriterions (up to 1,000) and
//     CriterionType, which "to add ... audience criterions i.e., custom audiences, in-market
//     audiences, and remarketing lists" must be "Audience". It answers AdGroupCriterionIds,
//     index-aligned with a null slot per criterion not added, and NestedPartialErrors, one
//     BatchErrorCollection per refused criterion carrying its Index.
//   - An AudienceCriterion carries AudienceId (Add: required); AudienceType is read-only. A
//     BiddableAdGroupCriterion with it observes the list, with a BidMultiplier whose Multiplier
//     is a whole percent from -90 through 900; a NegativeAdGroupCriterion excludes it.
//   - TargetSettingDetail: with no TargetSettingDetail for the Audience group, Microsoft "will
//     show ads to people searching for your ad, with the option to change the bid amount for
//     people included in the audience". This client never sends a TargetSetting, so an observed
//     list is BID ONLY: it never narrows who sees the ad.
//   - GetAudiencesByIds (POST Audiences/QueryByIds) takes at most 100 AudienceIds and a Type,
//     and answers Audiences index-aligned (null where not retrieved) plus PartialErrors.
//     System-generated lists (All Visitors, All Converters, Smart lists) are not supported by
//     it, so they cannot pass the pre-create check below.
//
// VERIFICATION LEVEL: none of this has been exercised against a live account, which is why the
// dispatcher keeps it behind MICROSOFT_AUDIENCES_ENABLED.
// ---------------------------------------------------------------------------

const (
	// FieldAudienceIDs, FieldExcludedAudienceIDs and FieldAudienceBidAdjustment are the
	// microsoftConfig keys, named in refusals.
	FieldAudienceIDs           = "audienceIds"
	FieldExcludedAudienceIDs   = "excludedAudienceIds"
	FieldAudienceBidAdjustment = "audienceBidAdjustment"

	// MaxAudienceIDs caps each list, so both together fit the one GetAudiencesByIds call
	// (100 ids) the pre-create check makes.
	MaxAudienceIDs = 50
	// MinAudienceBidAdjustment and MaxAudienceBidAdjustment are Microsoft's BidMultiplier range
	// for an audience criterion, in whole percent.
	MinAudienceBidAdjustment = -90
	MaxAudienceBidAdjustment = 900

	// audienceTypeRemarketingList is the only audience type accepted here, sent as
	// GetAudiencesByIds' Type and required of every audience it returns.
	audienceTypeRemarketingList = "RemarketingList"
	// adGroupCriterionTypeAudience is AddAdGroupCriterions' CriterionType for audiences.
	adGroupCriterionTypeAudience = "Audience"
	criterionTypeAudience        = "AudienceCriterion"
	adGroupCriterionBiddable     = "BiddableAdGroupCriterion"
	adGroupCriterionNegative     = "NegativeAdGroupCriterion"
	criterionBidMultiplier       = "BidMultiplier"
	// maxAudienceErrorItems bounds a decoded error array, as maxIndexedErrorItems does.
	maxAudienceErrorItems = 4 * MaxAudienceIDs

	// errCodeDuplicateAdGroupCriterion (4152, CampaignServiceDuplicateAdGroupCriterion,
	// "Duplicate ad group criterions are not allowed") is how Microsoft refuses a list the ad
	// group already carries. Kept as a defensive classification: the dispatcher sends no list to
	// a reused ad group, so a 4152 on an ad group this run created is unexpected, but it still
	// means the list is attached, so it is OutcomeAlreadyPresent.
	errCodeDuplicateAdGroupCriterion        = "CampaignServiceDuplicateAdGroupCriterion"
	errCodeDuplicateAdGroupCriterionNumeric = "4152"
)

// isDefaultCode reports whether one code field holds only a default: absent, null, 0 or "".
// BatchError.Code is a non-nullable int, so a slot with no error may carry 0.
func isDefaultCode(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", "0", `""`:
		return true
	}
	return false
}

// audienceErrorCode separates error PRESENCE from safe rendering: a pair of code fields is an
// error unless both are defaults (isDefaultCode). code is the rendered code (meaningfulCode), or
// withheldErrorCode when a present code is unreadable (a boolean, an object, an oversized
// string), so an error that cannot be shown is still counted as one.
func audienceErrorCode(errorCode, code json.RawMessage) (present bool, rendered string) {
	if isDefaultCode(errorCode) && isDefaultCode(code) {
		return false, ""
	}
	if c := meaningfulCode(errorCode, code); c != "" {
		return true, c
	}
	return true, withheldErrorCode
}

// isJSONNullRaw reports whether raw is the JSON literal null.
func isJSONNullRaw(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// isDuplicateAdGroupCriterion matches 4152 STRICTLY, as isNegativeKeywordAlreadyExists matches
// 4335: a symbolic ErrorCode must be that name, and the numeric code is consulted only when the
// symbolic one is absent.
func isDuplicateAdGroupCriterion(errorCode, code json.RawMessage) bool {
	if sym := codeString(errorCode); sym != "" {
		return sym == errCodeDuplicateAdGroupCriterion
	}
	// A symbolic code that is present but unreadable (a boolean, an object) is not "absent", so
	// the numeric code cannot stand in for it.
	if !isDefaultCode(errorCode) {
		return false
	}
	return codeString(code) == errCodeDuplicateAdGroupCriterionNumeric
}

// AudienceError is a refusal of one audience input; Field is the microsoftConfig key.
type AudienceError struct {
	Field string
	Msg   string
}

func (e *AudienceError) Error() string { return e.Field + ": " + e.Msg }

// ValidateAudiences checks the three create inputs together, without contacting Microsoft. Each
// id must be the canonical decimal spelling of a positive int64 (Microsoft's audience ids are
// longs), compared exactly and never trimmed; at most MaxAudienceIDs per list; no repeat and no
// id in both lists. adjustment, when set, must be a whole percent within Microsoft's range and
// is accepted only with audienceIds. A refusal names the field and a 1-based position, never a
// value.
func ValidateAudiences(include, exclude []string, adjustment *float64) error {
	check := func(field string, ids []string) (map[string]struct{}, error) {
		if len(ids) > MaxAudienceIDs {
			return nil, &AudienceError{Field: field, Msg: fmt.Sprintf("holds %d ids; at most %d are accepted", len(ids), MaxAudienceIDs)}
		}
		seen := make(map[string]struct{}, len(ids))
		for i, id := range ids {
			if !canonicalPositiveID(id) {
				return nil, &AudienceError{Field: field, Msg: fmt.Sprintf("entry %d is not a Microsoft audience id (a positive whole number, no sign or leading zero)", i+1)}
			}
			if _, dup := seen[id]; dup {
				return nil, &AudienceError{Field: field, Msg: fmt.Sprintf("entry %d repeats an earlier id", i+1)}
			}
			seen[id] = struct{}{}
		}
		return seen, nil
	}
	in, err := check(FieldAudienceIDs, include)
	if err != nil {
		return err
	}
	if _, err := check(FieldExcludedAudienceIDs, exclude); err != nil {
		return err
	}
	for i, id := range exclude {
		if _, both := in[id]; both {
			return &AudienceError{Field: FieldExcludedAudienceIDs, Msg: fmt.Sprintf("entry %d is also in %s; a list cannot be both observed and excluded", i+1, FieldAudienceIDs)}
		}
	}
	if adjustment != nil {
		a := *adjustment
		switch {
		case len(include) == 0:
			return &AudienceError{Field: FieldAudienceBidAdjustment, Msg: "applies to " + FieldAudienceIDs + ", so it requires at least one"}
		case math.IsNaN(a) || math.IsInf(a, 0) || a != math.Trunc(a):
			return &AudienceError{Field: FieldAudienceBidAdjustment, Msg: "must be a whole number of percent"}
		case a < MinAudienceBidAdjustment || a > MaxAudienceBidAdjustment:
			return &AudienceError{Field: FieldAudienceBidAdjustment, Msg: fmt.Sprintf("must be between %d and %d percent", MinAudienceBidAdjustment, MaxAudienceBidAdjustment)}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pre-create check: GetAudiencesByIds.
// ---------------------------------------------------------------------------

type getAudiencesByIDsRequest struct {
	AudienceIds []json.Number `json:"AudienceIds"`
	Type        string        `json:"Type"`
}

type msAudienceRef struct {
	Id   *json.Number `json:"Id"`
	Type string       `json:"Type"`
}

type getAudiencesByIDsResponse struct {
	Audiences     []*msAudienceRef `json:"Audiences"`
	PartialErrors indexedErrors    `json:"PartialErrors"`
}

// errAudienceLookupMalformed marks a GetAudiencesByIds answer this client cannot believe.
var errAudienceLookupMalformed = errors.New("microsoft-ads remarketing list lookup could not be confirmed")

// CheckRemarketingLists confirms, before anything is created, that every id (exclusions first,
// then observed lists) is a remarketing list on the client's account, with ONE GetAudiencesByIds
// call. An id Microsoft does not return, refuses with a partial error, or returns as another
// audience type is a domain.PreflightRefusal naming the field, the id and Microsoft's code or
// type. A call that fails, or an answer that cannot be believed (not index-aligned, an id that
// is not the one asked for, an error with no usable index), is a plain error. Either way
// nothing is created.
func (c *Client) CheckRemarketingLists(ctx context.Context, include, exclude []string) error {
	type want struct{ field, id string }
	var wants []want
	for _, id := range exclude {
		wants = append(wants, want{FieldExcludedAudienceIDs, id})
	}
	for _, id := range include {
		wants = append(wants, want{FieldAudienceIDs, id})
	}
	if len(wants) == 0 {
		return nil
	}
	ids := make([]json.Number, len(wants))
	for i, w := range wants {
		ids[i] = json.Number(w.id)
	}
	raw, err := c.doRequest(ctx, http.MethodPost, "Audiences/QueryByIds", getAudiencesByIDsRequest{AudienceIds: ids, Type: audienceTypeRemarketingList}, true /* a read */)
	if err != nil {
		return fmt.Errorf("microsoft-ads remarketing list lookup failed: %w", err)
	}
	// The Audiences array must be PRESENT: a 2xx without it (`{}`, `null`) describes nothing this
	// client asked for, and reading it as "no such list" would blame a possibly valid id.
	var probe map[string]json.RawMessage
	var resp getAudiencesByIDsResponse
	if json.Unmarshal(raw, &probe) != nil || json.Unmarshal(raw, &resp) != nil {
		return fmt.Errorf("%w: the response is not the documented shape", errAudienceLookupMalformed)
	}
	if aud, ok := probe["Audiences"]; !ok || isJSONNullRaw(aud) {
		return fmt.Errorf("%w: the response carries no Audiences array", errAudienceLookupMalformed)
	}
	if len(resp.Audiences) > len(wants) {
		return fmt.Errorf("%w: it returned more audiences than were asked for", errAudienceLookupMalformed)
	}
	// A zero or empty code is a default, not an error (audienceErrorCode); the rest are
	// attributed. A present code this client cannot read is evidence it cannot interpret, so
	// the answer is refused as unreadable rather than guessed at.
	var lookupErrs indexedErrors
	lookupErrs.Truncated = resp.PartialErrors.Truncated
	for _, it := range resp.PartialErrors.Items {
		present, rendered := audienceErrorCode(it.ErrorCode, it.Code)
		if !present {
			continue
		}
		if rendered == withheldErrorCode {
			return fmt.Errorf("%w: an error carries a code this client cannot read", errAudienceLookupMalformed)
		}
		lookupErrs.Items = append(lookupErrs.Items, it)
	}
	att := attribute(lookupErrs, len(wants))
	if att.unattributable {
		return fmt.Errorf("%w: an error could not be attributed to an id", errAudienceLookupMalformed)
	}
	for i, w := range wants {
		if att.errored[i] {
			code := att.byIndex[i]
			if code == "" {
				code = "unspecified"
			}
			return domain.PreflightRefusalf("invalid microsoftConfig.%s: Microsoft Advertising refused remarketing list %s (%s)", w.field, domain.CallerValue(w.id), code)
		}
		var a *msAudienceRef
		if i < len(resp.Audiences) {
			a = resp.Audiences[i]
		}
		if a == nil {
			return domain.PreflightRefusalf("invalid microsoftConfig.%s: Microsoft Advertising has no remarketing list %s on this account", w.field, domain.CallerValue(w.id))
		}
		if numberID(a.Id) != w.id {
			return fmt.Errorf("%w: position %d is not the audience asked for", errAudienceLookupMalformed, i+1)
		}
		if a.Type != audienceTypeRemarketingList {
			return domain.PreflightRefusalf("invalid microsoftConfig.%s: audience %s is a %s audience, not a remarketing list", w.field, domain.CallerValue(w.id), outputCode(a.Type))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// After create: AddAdGroupCriterions.
// ---------------------------------------------------------------------------

// AudienceOutcome is one list's positional outcome on the ad group: OutcomeApplied,
// OutcomeAlreadyPresent (Microsoft refused it only as a duplicate of a criterion the ad group
// already carries), OutcomeFailed or OutcomeUnconfirmed. CriterionID is set only for
// OutcomeApplied. The JSON tags are the persisted shape (CampaignResult.Audiences).
type AudienceOutcome struct {
	AudienceID  string `json:"audienceId"`
	Excluded    bool   `json:"excluded"`
	Outcome     string `json:"outcome"`
	CriterionID string `json:"criterionId,omitempty"`
	ErrorCode   string `json:"errorCode,omitempty"`
}

type msAudienceCriterion struct {
	Type       string      `json:"Type"`
	AudienceId json.Number `json:"AudienceId"`
}

type msBidMultiplier struct {
	Type       string  `json:"Type"`
	Multiplier float64 `json:"Multiplier"`
}

type msAdGroupCriterion struct {
	Type         string              `json:"Type"`
	AdGroupId    json.Number         `json:"AdGroupId"`
	Criterion    msAudienceCriterion `json:"Criterion"`
	CriterionBid *msBidMultiplier    `json:"CriterionBid,omitempty"`
}

type addAdGroupCriterionsRequest struct {
	AdGroupCriterions []msAdGroupCriterion `json:"AdGroupCriterions"`
	CriterionType     string               `json:"CriterionType"`
}

// adGroupCriterionErrorCollection is one NestedPartialErrors entry: Index is the refused
// criterion's position in the request; its own codes and its BatchErrors both count as a
// refusal of that criterion.
type adGroupCriterionErrorCollection struct {
	Index       *int            `json:"Index"`
	Code        json.RawMessage `json:"Code"`
	ErrorCode   json.RawMessage `json:"ErrorCode"`
	BatchErrors indexedErrors   `json:"BatchErrors"`
}

// adGroupCriterionErrors bounds the collection array, recording truncation.
type adGroupCriterionErrors struct {
	Items     []adGroupCriterionErrorCollection
	Truncated bool
}

func (b *adGroupCriterionErrors) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return errors.New("expected a JSON array of error collections")
	}
	for dec.More() {
		var it adGroupCriterionErrorCollection
		if err := dec.Decode(&it); err != nil {
			return err
		}
		if len(b.Items) < maxAudienceErrorItems {
			b.Items = append(b.Items, it)
			continue
		}
		b.Truncated = true
	}
	return nil
}

type addAdGroupCriterionsResponse struct {
	AdGroupCriterionIds []*json.Number         `json:"AdGroupCriterionIds"`
	NestedPartialErrors adGroupCriterionErrors `json:"NestedPartialErrors"`
}

// AttachAdGroupAudiences adds the excluded lists (NegativeAdGroupCriterion) and then the observed
// lists (BiddableAdGroupCriterion with a BidMultiplier of adjustment percent) to ONE ad group,
// one AddAdGroupCriterions call each, and reports one outcome per list: exclusions first, in
// request order, then observed lists. The lists are expected to have passed ValidateAudiences
// and CheckRemarketingLists.
//
// NOT RETRIED ON 429 (idempotent=false), the rule every add in this client follows: the add has
// no idempotency key. A call whose outcome is ambiguous (a transport failure, a 429 or 5xx, a
// 2xx this client cannot read) leaves each of its lists UNCONFIRMED; a definite refusal leaves
// them FAILED; a call never sent because the context ended is FAILED with ErrorCode NOT_SENT.
// The error, when non-nil, describes the whole-call failures in code-only text; the outcomes are
// always complete.
func (c *Client) AttachAdGroupAudiences(ctx context.Context, adGroupID string, include, exclude []string, adjustment float64) ([]AudienceOutcome, error) {
	if !canonicalPositiveID(adGroupID) {
		return nil, fmt.Errorf("microsoft-ads: ad group id %q is not a numeric id", truncate(adGroupID, maxErrorBodyChars))
	}
	var out []AudienceOutcome
	var errs []error
	for _, batch := range []struct {
		ids      []string
		excluded bool
	}{{exclude, true}, {include, false}} {
		if len(batch.ids) == 0 {
			continue
		}
		outcomes, err := c.addAudienceCriteria(ctx, adGroupID, batch.ids, batch.excluded, adjustment)
		out = append(out, outcomes...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

// addAudienceCriteria makes one AddAdGroupCriterions call for one kind of list.
func (c *Client) addAudienceCriteria(ctx context.Context, adGroupID string, ids []string, excluded bool, adjustment float64) ([]AudienceOutcome, error) {
	out := make([]AudienceOutcome, len(ids))
	for i, id := range ids {
		out[i] = AudienceOutcome{AudienceID: id, Excluded: excluded}
	}
	kind := "observed"
	if excluded {
		kind = "excluded"
	}
	all := func(outcome, code string) {
		for i := range out {
			out[i].Outcome, out[i].ErrorCode = outcome, code
		}
	}
	if ctx.Err() != nil {
		all(OutcomeFailed, errorCodeNotSent)
		return out, fmt.Errorf("microsoft-ads %s audience lists not sent: the context ended first", kind)
	}
	crits := make([]msAdGroupCriterion, len(ids))
	for i, id := range ids {
		crit := msAdGroupCriterion{
			Type:      adGroupCriterionNegative,
			AdGroupId: json.Number(adGroupID),
			Criterion: msAudienceCriterion{Type: criterionTypeAudience, AudienceId: json.Number(id)},
		}
		if !excluded {
			crit.Type = adGroupCriterionBiddable
			crit.CriterionBid = &msBidMultiplier{Type: criterionBidMultiplier, Multiplier: adjustment}
		}
		crits[i] = crit
	}
	raw, err := c.doRequest(ctx, http.MethodPost, "AdGroupCriterions", addAdGroupCriterionsRequest{AdGroupCriterions: crits, CriterionType: adGroupCriterionTypeAudience}, false)
	if err != nil {
		if IsOutcomeUnconfirmed(err) {
			all(OutcomeUnconfirmed, "")
			return out, fmt.Errorf("microsoft-ads %s audience lists UNCONFIRMED (they may be attached): %w", kind, err)
		}
		all(OutcomeFailed, "")
		return out, fmt.Errorf("microsoft-ads %s audience lists not attached: %w", kind, err)
	}
	// The id array must be PRESENT (it may hold nulls): a 2xx without it describes nothing this
	// client sent, so every list in the call is UNCONFIRMED.
	var probe map[string]json.RawMessage
	var resp addAdGroupCriterionsResponse
	sawIDs := false
	if json.Unmarshal(raw, &probe) == nil {
		_, sawIDs = probe["AdGroupCriterionIds"]
	}
	if !sawIDs || json.Unmarshal(raw, &resp) != nil || len(resp.AdGroupCriterionIds) > len(ids) {
		all(OutcomeUnconfirmed, "")
		return out, fmt.Errorf("microsoft-ads %s audience lists UNCONFIRMED: the response could not be read", kind)
	}
	// errored maps a request index to the first code refusing it; onlyDuplicate records whether
	// every error naming that index is 4152 (already on the ad group). Codes are read with
	// audienceErrorCode, so a zero or empty default (BatchError Code is a non-nullable int) is not
	// an error, while any other non-null code is, rendered as withheld when unreadable.
	//
	// incomplete is set when the evidence cannot cover every list: an error array (outer or
	// nested) was truncated, or an error carried no usable index. Then no list without an error
	// of its own can be proven attached, and it is UNCONFIRMED even with an id, the rule geo.go's
	// criterion add applies; a duplicate is not ALREADY_PRESENT either.
	errored := map[int]string{}
	onlyDuplicate := map[int]bool{}
	incomplete := resp.NestedPartialErrors.Truncated
	for _, coll := range resp.NestedPartialErrors.Items {
		collErr, code := audienceErrorCode(coll.ErrorCode, coll.Code)
		isErr := collErr
		dup := !collErr || isDuplicateAdGroupCriterion(coll.ErrorCode, coll.Code)
		if coll.BatchErrors.Truncated {
			incomplete = true
		}
		for _, it := range coll.BatchErrors.Items {
			itErr, itCode := audienceErrorCode(it.ErrorCode, it.Code)
			if !itErr {
				continue
			}
			isErr = true
			dup = dup && isDuplicateAdGroupCriterion(it.ErrorCode, it.Code)
			if code == "" {
				code = itCode
			}
		}
		if !isErr {
			continue
		}
		if coll.Index == nil || *coll.Index < 0 || *coll.Index >= len(ids) {
			incomplete = true
			continue
		}
		i := *coll.Index
		if _, seen := errored[i]; !seen {
			errored[i] = code
			onlyDuplicate[i] = dup
		} else {
			onlyDuplicate[i] = onlyDuplicate[i] && dup
		}
	}
	for i := range out {
		var id string
		if i < len(resp.AdGroupCriterionIds) {
			id = numberID(resp.AdGroupCriterionIds[i])
		}
		code, refused := errored[i]
		switch {
		case refused && onlyDuplicate[i] && !incomplete:
			out[i].Outcome = OutcomeAlreadyPresent
		case refused && onlyDuplicate[i]:
			// Only duplicates were retained, but a discarded or unattributable error may refuse
			// the same list for another reason: not proven attached.
			out[i].Outcome = OutcomeUnconfirmed
		case refused:
			out[i].Outcome, out[i].ErrorCode = OutcomeFailed, code
		case incomplete:
			out[i].Outcome = OutcomeUnconfirmed
		case id != "":
			out[i].Outcome, out[i].CriterionID = OutcomeApplied, id
		default:
			// Neither an id nor an error naming it: it may or may not exist.
			out[i].Outcome = OutcomeUnconfirmed
		}
	}
	return out, nil
}

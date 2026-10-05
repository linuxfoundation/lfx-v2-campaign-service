// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Keyword levers on a LIVE campaign (LFXV2-2665): pause/remove positive keywords, and add
// campaign-level negative keywords. Both are Campaign Management v13 REST operations on the
// same host and request layer every other call in this client uses:
//
//   - GetKeywordsByAdGroupId — POST Keywords/QueryByAdGroupId {AdGroupId}
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/getkeywordsbyadgroupid
//   - UpdateKeywords — PUT Keywords {AdGroupId, Keywords:[{Id, Status}]}; at most 1,000 keywords;
//     a Keyword field left unset on update "is not changed", so {Id, Status} is a status-only
//     update. https://learn.microsoft.com/en-us/advertising/campaign-management-service/updatekeywords
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/keyword
//   - DeleteKeywords — DELETE Keywords {AdGroupId, KeywordIds}; at most 1,000 ids.
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/deletekeywords
//   - AddNegativeKeywordsToEntities — POST EntityNegativeKeywords
//     {EntityNegativeKeywords:[{EntityId, EntityType:"Campaign", NegativeKeywords:[{Text, MatchType}]}]};
//     at most ONE EntityNegativeKeyword per call.
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/addnegativekeywordstoentities
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/entitynegativekeyword
//
// NONE OF THESE IS ATOMIC. Unlike Google's adGroupCriteria:mutate with partial failure off,
// Microsoft answers a batch with HTTP 200 and a PartialErrors (or NestedPartialErrors) array
// naming, by Index, the items it did NOT apply — every other item in the same call DID apply.
// So the outcome is reported PER ITEM, positionally, and never collapsed into one verdict:
// "applied" for an item Microsoft did not reject is a fact, and so is "failed" for one it did.
//
// What is collapsed into one verdict is a call Microsoft never answered per item — a non-2xx,
// a timeout, an undecodable 200. Those are whole-call outcomes, DEFINITE or UNCONFIRMED under
// the package's existing classification (IsOutcomeUnconfirmed), and every item the call carried
// takes that outcome.
// ---------------------------------------------------------------------------

// Keyword actions. Mirrors the service-level vocabulary (model.KeywordActionPause/Remove).
const (
	KeywordActionPause  = "PAUSE"
	KeywordActionRemove = "REMOVE"
)

// Per-item outcomes. A closed set the dispatcher maps 1:1 onto the API's outcome enum.
const (
	// OutcomeApplied: Microsoft answered the call and did not reject this item.
	OutcomeApplied = "APPLIED"
	// OutcomeAlreadyPresent: Microsoft rejected the add ONLY because the negative keyword is
	// already on the campaign (CampaignServiceNegativeKeywordAlreadyExists, 4335). The state the
	// caller asked for holds, so this is reported as success — distinctly, so it is not
	// mistaken for a newly-created negative.
	OutcomeAlreadyPresent = "ALREADY_PRESENT"
	// OutcomeFailed: DEFINITELY not applied — Microsoft named this item in its errors, or the
	// request carrying it was refused outright, or it was never sent.
	OutcomeFailed = "FAILED"
	// OutcomeUnconfirmed: MAY have been applied. Verify in Microsoft Advertising before retrying.
	OutcomeUnconfirmed = "UNCONFIRMED"
)

// errorCodeNotSent is the ErrorCode reported for an item whose call was never sent because
// the caller's context ended after an earlier call. Not a Microsoft code, and spelled so it
// cannot be mistaken for one.
const errorCodeNotSent = "NOT_SENT"

const (
	// maxKeywordActions bounds one batch. It matches the design's MaxLength(60) and the
	// google-ads sibling's cap; Microsoft's own per-call ceiling (1,000) is far higher.
	maxKeywordActions = 60
	// maxNegativeKeywords bounds one negative-keyword add, for the same reason and with the
	// same value as maxKeywords/the google-ads create path's negative cap. Microsoft allows up
	// to 20,000 negatives per campaign.
	maxNegativeKeywords = 60
	// maxNegativeKeywordRunes is Microsoft's documented NegativeKeyword.Text limit: "The text
	// can contain a maximum of 100 characters."
	// https://learn.microsoft.com/en-us/advertising/campaign-management-service/negativekeyword
	maxNegativeKeywordRunes = 100
	// maxIndexedErrorItems bounds how many PartialErrors entries a keyword-lever response may
	// carry before the array is treated as truncated. Each item can legitimately yield more
	// than one error, so it is a multiple of the request cap rather than equal to it.
	maxIndexedErrorItems = 4 * maxKeywordActions
	// keywordStatusDeleted is the KeywordStatus a removed keyword reports, if Microsoft
	// returns it at all.
	keywordStatusDeleted = "Deleted"
	// entityTypeCampaign is EntityNegativeKeyword.EntityType for a campaign-level negative
	// ("The possible values are AdGroup and Campaign").
	entityTypeCampaign = "Campaign"
)

// Negative-keyword error codes, both spellings. 4335 is
// CampaignServiceNegativeKeywordAlreadyExists, "Duplicate negative keyword already exists in
// this list." (https://learn.microsoft.com/en-us/advertising/guides/operation-error-codes).
const (
	errCodeNegativeKeywordExists        = "CampaignServiceNegativeKeywordAlreadyExists"
	errCodeNegativeKeywordExistsNumeric = "4335"
)

// KeywordAction is one requested keyword mutation. KeywordID is Microsoft's Keyword.Id, a
// positive int64 unique within its ad group — which is why AdGroupID travels with it.
type KeywordAction struct {
	AdGroupID string
	KeywordID string
	Action    string
}

// KeywordActionOutcome is one action's positional outcome. ErrorCode is Microsoft's
// machine-readable code for a FAILED/UNCONFIRMED item when one is known; never Message/Details.
type KeywordActionOutcome struct {
	AdGroupID string
	KeywordID string
	Action    string
	Outcome   string
	ErrorCode string
}

// AdGroupKeyword is one keyword as GetKeywordsByAdGroupId reports it — only the two fields the
// ownership check needs.
type AdGroupKeyword struct {
	ID     string
	Status string
}

// NegativeKeyword is one requested campaign-level negative keyword.
type NegativeKeyword struct {
	Text      string
	MatchType string
}

// NegativeKeywordOutcome is one negative keyword's positional outcome. NegativeKeywordID is
// set only for OutcomeApplied: an already-present negative's id is not returned by the add.
type NegativeKeywordOutcome struct {
	Text              string
	MatchType         string
	Outcome           string
	NegativeKeywordID string
	ErrorCode         string
}

// canonicalPositiveID reports whether s is the canonical base-10 spelling of a positive int64
// — no sign, no leading zero, no whitespace, in range. A digit/length check alone admits "0",
// "0123" and values above math.MaxInt64, none of which can name a Microsoft entity.
func canonicalPositiveID(s string) bool {
	if !idRE.MatchString(s) {
		return false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && strconv.FormatInt(n, 10) == s
}

// ValidateKeywordActions checks a keyword-action batch WITHOUT contacting Microsoft, so the
// dispatcher can refuse a malformed batch before any credential is decrypted.
//
// The rules are the google-ads sibling's, deliberately: the API publishes ONE input contract
// for this endpoint, and a request the design admits must be judged the same way whichever
// platform the campaign is on. Ids are NOT trimmed — the design's Pattern refuses padding for
// every HTTP caller, so trimming here would accept for direct callers what the transport refuses.
func ValidateKeywordActions(actions []KeywordAction) ([]KeywordAction, error) {
	if len(actions) == 0 {
		return nil, fmt.Errorf("microsoft-ads: at least one keyword action is required")
	}
	if len(actions) > maxKeywordActions {
		return nil, fmt.Errorf("microsoft-ads: at most %d keyword actions are supported, got %d", maxKeywordActions, len(actions))
	}
	out := make([]KeywordAction, 0, len(actions))
	seen := make(map[string]struct{}, len(actions))
	for i, a := range actions {
		if !canonicalPositiveID(a.AdGroupID) {
			return nil, fmt.Errorf("microsoft-ads: keyword action %d has an ad group id that is not the canonical base-10 spelling of a positive int64", i)
		}
		if !canonicalPositiveID(a.KeywordID) {
			return nil, fmt.Errorf("microsoft-ads: keyword action %d has a keyword id that is not the canonical base-10 spelling of a positive int64", i)
		}
		action := strings.ToUpper(strings.TrimSpace(a.Action))
		if action != KeywordActionPause && action != KeywordActionRemove {
			return nil, fmt.Errorf("microsoft-ads: keyword action %d has unsupported action %q (want %s or %s)",
				i, truncate(a.Action, maxErrorBodyChars), KeywordActionPause, KeywordActionRemove)
		}
		// Refused rather than de-duplicated: two entries may carry DIFFERENT actions, and
		// silently dropping either applies a mutation the caller did not ask for.
		key := a.AdGroupID + "~" + a.KeywordID
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("microsoft-ads: keyword action %d addresses keyword %s more than once in one batch", i, key)
		}
		seen[key] = struct{}{}
		out = append(out, KeywordAction{AdGroupID: a.AdGroupID, KeywordID: a.KeywordID, Action: action})
	}
	return out, nil
}

// isNegativeKeywordPunct reports the punctuation a negative keyword may carry. The set is
// deliberately narrow. Microsoft's text policy refuses "unnecessary symbols such as @, }{, \][,
// ¤, and §", mathematical symbols such as <, >, =, and "consecutive, non-alphanumeric
// characters" (https://about.ads.microsoft.com/en-us/policies/text-guidelines), and it permits
// "&" in place of "and". Refusing here, before the call, turns an editorial rejection Microsoft
// would report per item into a 400 the caller can fix — and keeps quote and bracket characters,
// which the Bing UI reads as match-type syntax, out of the text entirely.
func isNegativeKeywordPunct(r rune) bool {
	switch r {
	case '&', '\'', '-', '.':
		return true
	}
	return false
}

// ValidateNegativeKeywords checks a negative-keyword batch WITHOUT contacting Microsoft and
// returns the normalized batch, index-aligned with the input.
//
// Rules, each a permanent input fault:
//   - 1..maxNegativeKeywords entries;
//   - text trimmed, internal whitespace collapsed, non-empty, at most 100 characters (runes);
//   - text made only of letters, combining marks, digits, single spaces and & ' - . — with no
//     two punctuation characters adjacent (see isNegativeKeywordPunct for the sources);
//   - match type Exact or Phrase, any casing — "The supported values for a negative keyword are
//     Exact and Phrase" (NegativeKeyword reference). Broad is refused, never mapped;
//   - no (match type, case-folded text) pair twice. REFUSED rather than de-duplicated, because
//     the outcomes are positional: dropping an entry would shift every later result off the
//     request index the caller zips it with.
func ValidateNegativeKeywords(in []NegativeKeyword) ([]NegativeKeyword, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("microsoft-ads: at least one negative keyword is required")
	}
	if len(in) > maxNegativeKeywords {
		return nil, fmt.Errorf("microsoft-ads: at most %d negative keywords are supported per request, got %d", maxNegativeKeywords, len(in))
	}
	out := make([]NegativeKeyword, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, nk := range in {
		// Control characters are checked on the RAW text, so a leading/trailing one cannot
		// hide behind the trim below.
		if strings.IndexFunc(nk.Text, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("microsoft-ads: negative keyword %d contains a control character", i)
		}
		text := strings.Join(strings.Fields(nk.Text), " ")
		if text == "" {
			return nil, fmt.Errorf("microsoft-ads: negative keyword %d is empty", i)
		}
		if n := utf8.RuneCountInString(text); n > maxNegativeKeywordRunes {
			return nil, fmt.Errorf("microsoft-ads: negative keyword %d is %d characters, exceeding Microsoft's %d-character limit", i, n, maxNegativeKeywordRunes)
		}
		prevPunct := false
		for _, r := range text {
			switch {
			case unicode.IsLetter(r), unicode.IsMark(r), unicode.IsDigit(r), r == ' ':
				prevPunct = false
			case isNegativeKeywordPunct(r):
				if prevPunct {
					return nil, fmt.Errorf("microsoft-ads: negative keyword %d has consecutive punctuation, which Microsoft's text policy refuses", i)
				}
				prevPunct = true
			default:
				return nil, fmt.Errorf("microsoft-ads: negative keyword %d contains %q, which is not allowed (letters, digits, spaces and & ' - . only)", i, r)
			}
		}
		matchType, ok := canonicalMatchType(nk.MatchType)
		if !ok || matchType == MatchTypeBroad {
			return nil, fmt.Errorf("microsoft-ads: negative keyword %d has unsupported match type %q (want %s or %s)",
				i, truncate(nk.MatchType, maxErrorBodyChars), MatchTypeExact, MatchTypePhrase)
		}
		key := matchType + "\x00" + strings.ToLower(text)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("microsoft-ads: negative keyword %d repeats an earlier entry with the same text and match type", i)
		}
		seen[key] = struct{}{}
		out = append(out, NegativeKeyword{Text: text, MatchType: matchType})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Response decoding shared by the three mutations.
// ---------------------------------------------------------------------------

// indexedErrorItem is one BatchError with the Index the keyword levers attribute it by. Index
// is a pointer so an ABSENT index is distinguishable from index 0 — conflating them would pin
// an unattributable error onto the first item and report every other item as applied.
type indexedErrorItem struct {
	Index     *int            `json:"Index"`
	Code      json.RawMessage `json:"Code"`
	ErrorCode json.RawMessage `json:"ErrorCode"`
}

// code renders the item's machine-readable code, symbolic first.
func (it indexedErrorItem) code() string {
	if s := codeString(it.ErrorCode); s != "" && len(s) <= maxErrorCodeLen {
		return s
	}
	if s := codeString(it.Code); s != "" && len(s) <= maxErrorCodeLen {
		return s
	}
	return ""
}

// hasCode reports whether the item carries code in either spelling.
func (it indexedErrorItem) hasCode(codes ...string) bool {
	for _, c := range codes {
		if strings.EqualFold(codeString(it.ErrorCode), c) || strings.EqualFold(codeString(it.Code), c) {
			return true
		}
	}
	return false
}

// isError reports whether the entry carries an actual code (a null placeholder does not).
func (it indexedErrorItem) isError() bool {
	return rawCodePresent(it.ErrorCode) || rawCodePresent(it.Code)
}

// indexedErrors streams a BatchError array, retaining at most maxIndexedErrorItems entries and
// recording whether more were present. A truncated array cannot prove an item was NOT
// rejected, which is why the attribution below downgrades to UNCONFIRMED when it is set.
type indexedErrors struct {
	Items     []indexedErrorItem
	Truncated bool
}

func (b *indexedErrors) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("expected a JSON array of errors")
	}
	for dec.More() {
		var it indexedErrorItem
		if err := dec.Decode(&it); err != nil {
			return err
		}
		if len(b.Items) < maxIndexedErrorItems {
			b.Items = append(b.Items, it)
			continue
		}
		b.Truncated = true
	}
	return nil
}

// attribution maps a call's errors onto its n request items.
type attribution struct {
	// byIndex holds the first code reported for each request index (empty string for an
	// error whose code could not be rendered).
	byIndex map[int]string
	// errored records every index an error was attributed to.
	errored map[int]bool
	// unattributable is set when an error carried no usable index, an index outside the
	// request, or the array was truncated. Then NO un-named item can be proven applied.
	unattributable bool
}

func attribute(errs indexedErrors, n int) attribution {
	a := attribution{byIndex: map[int]string{}, errored: map[int]bool{}, unattributable: errs.Truncated}
	for _, it := range errs.Items {
		if !it.isError() {
			continue
		}
		if it.Index == nil || *it.Index < 0 || *it.Index >= n {
			a.unattributable = true
			continue
		}
		if !a.errored[*it.Index] {
			a.byIndex[*it.Index] = it.code()
		}
		a.errored[*it.Index] = true
	}
	return a
}

// keywordMutationUnconfirmedError marks a whole keyword-lever call whose outcome is ambiguous
// — or a call that was REFUSED only after an earlier attempt was answered with a 429, whose
// outcome this package already treats as ambiguous (see retriedUnconfirmedError).
type keywordMutationUnconfirmedError struct {
	what string
	err  error
}

func (e *keywordMutationUnconfirmedError) Error() string {
	return "microsoft-ads " + e.what + " outcome is unconfirmed (it may have been applied): " + e.err.Error()
}
func (e *keywordMutationUnconfirmedError) Unwrap() error { return e.err }

// Unconfirmed marks the outcome as ambiguous-applied for IsOutcomeUnconfirmed.
func (e *keywordMutationUnconfirmedError) Unconfirmed() bool { return true }

// errMalformedKeywordResponse marks a 2xx keyword-lever body this client could not read. The
// call was answered, so it may have applied: always wrapped as unconfirmed.
var errMalformedKeywordResponse = errors.New("microsoft-ads: the keyword response could not be read")

// ---------------------------------------------------------------------------
// GetKeywordsByAdGroupId
// ---------------------------------------------------------------------------

type queryKeywordsByAdGroupRequest struct {
	AdGroupId json.Number `json:"AdGroupId"`
}

type msKeywordRead struct {
	Id     *json.Number `json:"Id"`
	Status string       `json:"Status"`
}

// GetAdGroupKeywords reads the ad group's positive keywords: the ownership check the keyword
// actions make BEFORE any mutation. A pure read, retried on 429 like every read here.
//
// A body that omits the Keywords field is an ERROR, not an empty ad group: Microsoft documents
// "If no keywords exist, an empty array is returned", so absence is a body that never answered
// the question — and reading it as "no keywords" would refuse every action as foreign, while
// reading a malformed entry leniently could admit one that is not.
func (c *Client) GetAdGroupKeywords(ctx context.Context, adGroupID string) ([]AdGroupKeyword, error) {
	if !canonicalPositiveID(adGroupID) {
		return nil, fmt.Errorf("microsoft-ads: ad group id %q is not a numeric id", truncate(adGroupID, maxErrorBodyChars))
	}
	body, err := c.doRequest(ctx, http.MethodPost, "Keywords/QueryByAdGroupId", queryKeywordsByAdGroupRequest{AdGroupId: json.Number(adGroupID)}, true)
	if err != nil {
		return nil, err
	}
	var probe map[string]json.RawMessage
	if uerr := json.Unmarshal(body, &probe); uerr != nil {
		return nil, fmt.Errorf("microsoft-ads: decode keywords of ad group %s: %v", adGroupID, uerr)
	}
	raw, ok := probe["Keywords"]
	if !ok {
		return nil, fmt.Errorf("microsoft-ads: the keywords read for ad group %s omitted the Keywords field", adGroupID)
	}
	var rows []msKeywordRead
	if uerr := json.Unmarshal(raw, &rows); uerr != nil {
		return nil, fmt.Errorf("microsoft-ads: decode keywords of ad group %s: %v", adGroupID, uerr)
	}
	out := make([]AdGroupKeyword, 0, len(rows))
	for i, r := range rows {
		id := numberID(r.Id)
		if id == "" {
			return nil, fmt.Errorf("microsoft-ads: the keywords read for ad group %s returned an entry %d with no usable id", adGroupID, i)
		}
		out = append(out, AdGroupKeyword{ID: id, Status: strings.TrimSpace(r.Status)})
	}
	return out, nil
}

// IsDeleted reports whether the keyword is in the Deleted state.
func (k AdGroupKeyword) IsDeleted() bool { return strings.EqualFold(k.Status, keywordStatusDeleted) }

// ---------------------------------------------------------------------------
// UpdateKeywords (PAUSE) + DeleteKeywords (REMOVE)
// ---------------------------------------------------------------------------

type deleteKeywordsRequest struct {
	AdGroupId  json.Number   `json:"AdGroupId"`
	KeywordIds []json.Number `json:"KeywordIds"`
}

// keywordMutateResponse is the 200 body of UpdateKeywords and DeleteKeywords. PartialErrors'
// PRESENCE is tracked because absence and emptiness mean different things, exactly as
// updateStatusResponse documents: `[]`/null affirm no item failed, a missing field never spoke.
type keywordMutateResponse struct {
	PartialErrors    indexedErrors
	sawPartialErrors bool
}

func (r *keywordMutateResponse) UnmarshalJSON(data []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	raw, ok := probe["PartialErrors"]
	if !ok {
		return nil
	}
	r.sawPartialErrors = true
	return json.Unmarshal(raw, &r.PartialErrors)
}

// ApplyKeywordActions applies a validated batch on ONE ad group and reports one outcome per
// action, in request order.
//
// PAUSE actions go in one UpdateKeywords call and REMOVE actions in one DeleteKeywords call —
// two operations, because Microsoft has no single mutate covering both. PAUSE is sent FIRST:
// it is the reversible one, so a failure between the two calls leaves the caller with the
// recoverable half applied, never only the irreversible one.
//
// Both calls are sent as IDEMPOTENT, so a 429 is retried under the client's bounded backoff:
// re-pausing converges, and re-deleting cannot delete twice. What a retry cannot do is make a
// LATER refusal speak for the earlier attempt — a mutating 429 is ambiguous in this package —
// so once a call was retried, every item it reports FAILED (or every item, on a whole-call
// refusal) is reported UNCONFIRMED instead. Same rule putUpdate applies (PR #255).
//
// The error return is reserved for "no call was answered per item": then there is nothing
// positional to report, and the error is DEFINITE or UNCONFIRMED (IsOutcomeUnconfirmed) as the
// worst of the calls was. Otherwise the outcomes are returned with a nil error even when some
// — or all — items FAILED: that is Microsoft's per-item answer, and it is the caller's to read.
func (c *Client) ApplyKeywordActions(ctx context.Context, adGroupID string, actions []KeywordAction) ([]KeywordActionOutcome, error) {
	validated, err := ValidateKeywordActions(actions)
	if err != nil {
		return nil, err
	}
	for i, a := range validated {
		if a.AdGroupID != adGroupID {
			return nil, fmt.Errorf("microsoft-ads: keyword action %d names ad group %s, not %s", i, a.AdGroupID, adGroupID)
		}
	}
	out := make([]KeywordActionOutcome, len(validated))
	var pauseIdx, removeIdx []int
	for i, a := range validated {
		out[i] = KeywordActionOutcome{AdGroupID: a.AdGroupID, KeywordID: a.KeywordID, Action: a.Action}
		if a.Action == KeywordActionPause {
			pauseIdx = append(pauseIdx, i)
		} else {
			removeIdx = append(removeIdx, i)
		}
	}

	type group struct {
		idx    []int
		method string
		what   string
		body   func() any
	}
	groups := []group{
		{pauseIdx, http.MethodPut, "keyword pause", func() any {
			kws := make([]msKeywordStatus, 0, len(pauseIdx))
			for _, i := range pauseIdx {
				kws = append(kws, msKeywordStatus{Id: json.Number(validated[i].KeywordID), Status: StatusPaused})
			}
			return updateKeywordsRequest{AdGroupId: json.Number(adGroupID), Keywords: kws}
		}},
		{removeIdx, http.MethodDelete, "keyword delete", func() any {
			ids := make([]json.Number, 0, len(removeIdx))
			for _, i := range removeIdx {
				ids = append(ids, json.Number(validated[i].KeywordID))
			}
			return deleteKeywordsRequest{AdGroupId: json.Number(adGroupID), KeywordIds: ids}
		}},
	}

	answered := false
	var wholeErrs []error
	for _, g := range groups {
		if len(g.idx) == 0 {
			continue
		}
		if answered || len(wholeErrs) > 0 {
			// A later call is only sent while the caller still waits for it. Not sending is
			// DEFINITE: nothing in this group can have been applied.
			if ctx.Err() != nil {
				for _, i := range g.idx {
					out[i].Outcome, out[i].ErrorCode = OutcomeFailed, errorCodeNotSent
				}
				continue
			}
		}
		outcomes, werr := c.keywordMutation(ctx, g.method, g.what, g.body(), len(g.idx))
		if werr != nil {
			wholeErrs = append(wholeErrs, werr)
			outcome := OutcomeFailed
			if IsOutcomeUnconfirmed(werr) {
				outcome = OutcomeUnconfirmed
			}
			code := wholeCallCode(werr)
			for _, i := range g.idx {
				out[i].Outcome, out[i].ErrorCode = outcome, code
			}
			continue
		}
		answered = true
		for k, i := range g.idx {
			out[i].Outcome, out[i].ErrorCode = outcomes[k].outcome, outcomes[k].code
		}
	}
	if !answered {
		if len(wholeErrs) == 0 {
			// Unreachable: the first non-empty group is always sent. Refused rather than
			// returning (nil, nil), which a caller would read as success.
			return nil, fmt.Errorf("microsoft-ads: no keyword call was sent")
		}
		return nil, joinWholeCallErrors("keyword actions", wholeErrs)
	}
	return out, nil
}

// itemOutcome is one item's outcome inside a single call.
type itemOutcome struct {
	outcome string
	code    string
}

// keywordMutation sends one UpdateKeywords/DeleteKeywords call and attributes its PartialErrors
// to its n items. A non-nil error means the call was not answered per item.
func (c *Client) keywordMutation(ctx context.Context, method, what string, body any, n int) ([]itemOutcome, error) {
	raw, retries, err := c.doRequestCounted(ctx, method, "Keywords", body, true)
	if err != nil {
		if retries > 0 && !IsOutcomeUnconfirmed(err) {
			return nil, &retriedUnconfirmedError{what: what, retries: retries, err: err}
		}
		return nil, err
	}
	var resp keywordMutateResponse
	if uerr := json.Unmarshal(raw, &resp); uerr != nil || !resp.sawPartialErrors {
		return nil, &keywordMutationUnconfirmedError{what: what, err: errMalformedKeywordResponse}
	}
	att := attribute(resp.PartialErrors, n)
	out := make([]itemOutcome, n)
	for i := range out {
		switch {
		case att.errored[i] && retries > 0:
			// The final attempt refused it, but an earlier rate-limited attempt may not have.
			out[i] = itemOutcome{OutcomeUnconfirmed, att.byIndex[i]}
		case att.errored[i]:
			out[i] = itemOutcome{OutcomeFailed, att.byIndex[i]}
		case att.unattributable:
			// An error Microsoft did not pin to an index may be this item's.
			out[i] = itemOutcome{OutcomeUnconfirmed, ""}
		default:
			out[i] = itemOutcome{OutcomeApplied, ""}
		}
	}
	return out, nil
}

// wholeCallCode returns the first Microsoft code on a whole-call refusal, if any.
func wholeCallCode(err error) string {
	var ae *apiError
	if errors.As(err, &ae) && len(ae.ErrorCodes) > 0 {
		return ae.ErrorCodes[0]
	}
	return ""
}

// joinWholeCallErrors folds the whole-call failures of a request no call answered into one
// error, UNCONFIRMED if any of them was.
func joinWholeCallErrors(what string, errs []error) error {
	joined := errors.Join(errs...)
	for _, e := range errs {
		if IsOutcomeUnconfirmed(e) {
			return &keywordMutationUnconfirmedError{what: what, err: joined}
		}
	}
	return joined
}

// ---------------------------------------------------------------------------
// AddNegativeKeywordsToEntities (campaign level)
// ---------------------------------------------------------------------------

type msNegativeKeyword struct {
	MatchType string `json:"MatchType"`
	Text      string `json:"Text"`
}

type msEntityNegativeKeyword struct {
	EntityId         json.Number         `json:"EntityId"`
	EntityType       string              `json:"EntityType"`
	NegativeKeywords []msNegativeKeyword `json:"NegativeKeywords"`
}

type addNegativeKeywordsRequest struct {
	EntityNegativeKeywords []msEntityNegativeKeyword `json:"EntityNegativeKeywords"`
}

// msIDCollection is one IdCollection: Ids index-aligned with the request's negative keywords,
// null where one was not added.
type msIDCollection struct {
	Ids []*json.Number `json:"Ids"`
}

// msBatchErrorCollection is one NestedPartialErrors entry. Its OWN Code/ErrorCode is an
// entity-level (campaign) error; BatchErrors are the per-negative-keyword ones.
type msBatchErrorCollection struct {
	BatchErrors indexedErrors   `json:"BatchErrors"`
	Code        json.RawMessage `json:"Code"`
	ErrorCode   json.RawMessage `json:"ErrorCode"`
}

type addNegativeKeywordsResponse struct {
	NegativeKeywordIds  []msIDCollection         `json:"NegativeKeywordIds"`
	NestedPartialErrors []msBatchErrorCollection `json:"NestedPartialErrors"`
}

// AddCampaignNegativeKeywords adds validated negative keywords to ONE campaign and reports one
// outcome per keyword, in request order.
//
// NOT RETRIED ON 429 (idempotent=false), the create rule every add in this client follows: the
// add has no idempotency key, and although a re-add of the SAME negative is answered 4335
// (already exists) rather than duplicated, that is a per-item report, not a guarantee this
// client can lean on for a whole call it never saw answered. A 429 is therefore returned at
// once and is UNCONFIRMED under createOutcomeAmbiguous.
//
// Per item: an id → APPLIED; a 4335 → ALREADY_PRESENT (the requested state holds); any other
// attributed error → FAILED; no id and no attributed error → UNCONFIRMED (Microsoft answered,
// but not about this item). An ENTITY-level error — the campaign itself refused — is a
// whole-call DEFINITE failure: nothing was added to it.
func (c *Client) AddCampaignNegativeKeywords(ctx context.Context, campaignID string, keywords []NegativeKeyword) ([]NegativeKeywordOutcome, error) {
	if !canonicalPositiveID(campaignID) {
		return nil, fmt.Errorf("microsoft-ads: campaign id %q is not a numeric id", truncate(campaignID, maxErrorBodyChars))
	}
	validated, err := ValidateNegativeKeywords(keywords)
	if err != nil {
		return nil, err
	}
	nks := make([]msNegativeKeyword, 0, len(validated))
	for _, k := range validated {
		nks = append(nks, msNegativeKeyword{MatchType: k.MatchType, Text: k.Text})
	}
	raw, err := c.doRequest(ctx, http.MethodPost, "EntityNegativeKeywords", addNegativeKeywordsRequest{
		EntityNegativeKeywords: []msEntityNegativeKeyword{{
			EntityId:         json.Number(campaignID),
			EntityType:       entityTypeCampaign,
			NegativeKeywords: nks,
		}},
	}, false)
	if err != nil {
		return nil, err
	}
	var probe map[string]json.RawMessage
	if uerr := json.Unmarshal(raw, &probe); uerr != nil {
		return nil, &keywordMutationUnconfirmedError{what: "negative keyword add", err: errMalformedKeywordResponse}
	}
	_, sawIDs := probe["NegativeKeywordIds"]
	_, sawErrs := probe["NestedPartialErrors"]
	var resp addNegativeKeywordsResponse
	if uerr := json.Unmarshal(raw, &resp); uerr != nil || (!sawIDs && !sawErrs) {
		return nil, &keywordMutationUnconfirmedError{what: "negative keyword add", err: errMalformedKeywordResponse}
	}

	n := len(validated)
	var itemErrs indexedErrors
	for _, coll := range resp.NestedPartialErrors {
		if rawCodePresent(coll.ErrorCode) || rawCodePresent(coll.Code) {
			code := codeString(coll.ErrorCode)
			if code == "" {
				code = codeString(coll.Code)
			}
			// The campaign itself was refused, so no keyword was attached to it.
			return nil, &negativeKeywordEntityError{code: code}
		}
		itemErrs.Items = append(itemErrs.Items, coll.BatchErrors.Items...)
		itemErrs.Truncated = itemErrs.Truncated || coll.BatchErrors.Truncated
	}
	att := attribute(itemErrs, n)
	alreadyPresent := map[int]bool{}
	for _, it := range itemErrs.Items {
		if it.Index != nil && *it.Index >= 0 && *it.Index < n && it.hasCode(errCodeNegativeKeywordExists, errCodeNegativeKeywordExistsNumeric) {
			alreadyPresent[*it.Index] = true
		}
	}
	var ids []*json.Number
	if len(resp.NegativeKeywordIds) > 0 {
		ids = resp.NegativeKeywordIds[0].Ids
	}

	out := make([]NegativeKeywordOutcome, n)
	for i, k := range validated {
		out[i] = NegativeKeywordOutcome{Text: k.Text, MatchType: k.MatchType}
		var id string
		if i < len(ids) {
			id = numberID(ids[i])
		}
		switch {
		case att.errored[i] && alreadyPresent[i] && onlyCode(itemErrs.Items, i, errCodeNegativeKeywordExists, errCodeNegativeKeywordExistsNumeric):
			out[i].Outcome = OutcomeAlreadyPresent
		case att.errored[i]:
			out[i].Outcome, out[i].ErrorCode = OutcomeFailed, att.byIndex[i]
		case id != "":
			out[i].Outcome, out[i].NegativeKeywordID = OutcomeApplied, id
		default:
			out[i].Outcome = OutcomeUnconfirmed
		}
	}
	return out, nil
}

// onlyCode reports whether every error attributed to index i carries one of codes — so a
// genuine rejection travelling alongside an already-exists for the same item stays FAILED.
func onlyCode(items []indexedErrorItem, i int, codes ...string) bool {
	for _, it := range items {
		if it.Index == nil || *it.Index != i || !it.isError() {
			continue
		}
		if !it.hasCode(codes...) {
			return false
		}
	}
	return true
}

// negativeKeywordEntityError is a DEFINITE whole-call refusal: Microsoft answered 200 but
// rejected the campaign (the entity) itself, so no negative keyword was attached. It renders
// only the code, never Message/Details, matching the apiError contract.
type negativeKeywordEntityError struct{ code string }

func (e *negativeKeywordEntityError) Error() string {
	code := e.code
	if code == "" {
		code = "unspecified"
	}
	return "microsoft-ads rejected the campaign for the negative keyword add: " + code
}

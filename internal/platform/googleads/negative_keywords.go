// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"
)

// CampaignNegativeKeyword is one negative keyword criterion already attached to a campaign, as
// read back by ListCampaignNegativeKeywords. MatchType is Google's own spelling (EXACT, PHRASE,
// BROAD), not the endpoint's platform-neutral one — translating is the dispatcher's job, which is
// where the neutral vocabulary is defined.
type CampaignNegativeKeyword struct {
	CriterionID string
	Text        string
	MatchType   string
}

// criterionTypeKeyword is Google's CriterionType enum value for a keyword criterion. A campaign
// carries geo, language, device and schedule criteria from the same resource, so the type is both
// a query predicate and a re-check on every row.
const criterionTypeKeyword = "KEYWORD"

// gaqlCampaignNegativeRow is one googleAds:search row for the campaign-negatives query.
//
// CriterionID is a STRING because campaign_criterion.criterion_id is an int64 upstream and Google
// Ads REST emits int64 as a JSON string to avoid float64 precision loss (gaqlKeywordRow documents
// the same split). Negative and the type enum are not int64 and arrive as a bare bool and a
// string.
//
// Negative is a proto bool, so protobuf JSON OMITS it when false: a positive criterion arrives
// with the key absent and decodes to false. Absence therefore already means "not a negative" and
// is safe to treat as such — the opposite reading (absence as unknown) would be wrong.
type gaqlCampaignNegativeRow struct {
	CampaignCriterion struct {
		CriterionID string `json:"criterionId"`
		Negative    bool   `json:"negative"`
		Type        string `json:"type"`
		Keyword     struct {
			Text      string `json:"text"`
			MatchType string `json:"matchType"`
		} `json:"keyword"`
	} `json:"campaignCriterion"`
	Campaign struct {
		ID string `json:"id"`
	} `json:"campaign"`
}

// campaignNegativeQueryFields is the SELECT list for the campaign-negatives read. campaign.id is
// selected so each row can be checked against the campaign that was asked for rather than trusted
// because of the WHERE clause; campaign_criterion.negative and .type are selected for the same
// reason — the filters below are re-applied IN CODE.
const campaignNegativeQueryFields = "campaign.id, campaign_criterion.criterion_id, campaign_criterion.negative, campaign_criterion.type, campaign_criterion.keyword.text, campaign_criterion.keyword.match_type"

// ListCampaignNegativeKeywords returns the negative keyword criteria currently attached to one
// campaign in this account.
//
// It exists for the ADD path's already-present check, and that is what fixes its semantics.
// campaignCriteria:mutate is ATOMIC in this client (partialFailure is never set, here or anywhere
// else), so a single duplicate criterion fails the ENTIRE batch — including the keywords that
// would have been accepted. Sending a keyword the campaign already excludes is therefore not a
// harmless no-op but a batch-wide failure, and the only way to avoid it is to know what is there
// first.
//
// NO STATUS PREDICATE, deliberately. GAQL excludes REMOVED resources from campaign_criterion by
// default, so this returns exactly the LIVE negatives — which is precisely the set that would
// collide on a duplicate. That is the opposite of GetCampaignSettings, which names
// campaign.status explicitly to opt REMOVED back IN, because there the question is "does this
// campaign exist at all" rather than "what is in force". Adding a status filter here would make a
// removed criterion read as present and suppress a keyword the campaign no longer excludes.
//
// The contract:
//
//   - the campaign has negatives -> (them, nil)
//   - the campaign has none, or does not exist -> (nil, nil)
//   - anything unverifiable -> (nil, error)
//
// The second case folds "no criteria" together with "no such campaign" because this query cannot
// tell them apart — a campaign with no negatives and a campaign id that matches nothing both
// return zero rows. The caller must NOT read an empty result as proof the campaign exists, and
// NO CALLER DOES establish existence on this path: the add-negative-keywords lever runs no
// GetCampaignSettings read, as no platform's negative-keyword lever does — unlike the bid and
// budget levers, which do and answer 404. So a campaign id recorded in the row but absent
// upstream withholds nothing here and surfaces as a DEFINITE mutate failure, not as
// ErrPlatformCampaignAbsent. That is the sibling-consistent answer, and it costs the common path
// no extra round trip; it is written down here because the opposite is easy to assume.
func (c *Client) ListCampaignNegativeKeywords(ctx context.Context, campaignID string) ([]CampaignNegativeKeyword, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, err
	}
	// Validated before interpolation: the id goes straight into the GAQL text, so a non-numeric
	// value could change the shape of the query rather than merely fail to match.
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, err
	}

	query := "SELECT " + campaignNegativeQueryFields +
		" FROM campaign_criterion WHERE campaign.id = " + campaignID +
		" AND campaign_criterion.type = 'KEYWORD' AND campaign_criterion.negative = TRUE"
	rows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("google-ads: list negative keywords for campaign %s: %w", campaignID, err)
	}

	out := make([]CampaignNegativeKeyword, 0, len(rows))
	for i, raw := range rows {
		var row gaqlCampaignNegativeRow
		if uErr := json.Unmarshal(raw, &row); uErr != nil {
			return nil, &transportError{
				Method: http.MethodPost,
				Path:   c.customerPath("googleAds:search"),
				Err:    fmt.Errorf("decode campaign negative keyword row at index %d: %w", i, uErr),
			}
		}
		// EVERY filter in the WHERE clause is re-applied here, for GetKeywordPerformance's
		// reason: the predicate is a request, and the rows are the answer. A response carrying
		// a POSITIVE criterion, a non-keyword criterion, or another campaign's row would
		// otherwise be folded into the present-set and silently suppress a keyword the caller
		// asked to add — the one failure mode this read exists to prevent.
		if !row.CampaignCriterion.Negative || row.CampaignCriterion.Type != criterionTypeKeyword {
			return nil, fmt.Errorf("google-ads: campaign %s negative keyword row at index %d is not a negative keyword criterion (negative=%t type=%q); refusing to trust this response",
				campaignID, i, row.CampaignCriterion.Negative, row.CampaignCriterion.Type)
		}
		if row.Campaign.ID != campaignID {
			return nil, fmt.Errorf("google-ads: campaign %s negative keyword row at index %d reports a different campaign id %q; refusing to trust this response",
				campaignID, i, row.Campaign.ID)
		}
		if row.CampaignCriterion.CriterionID == "" {
			return nil, fmt.Errorf("google-ads: campaign %s negative keyword row at index %d carries no criterion id; refusing to trust this response", campaignID, i)
		}
		// The text is compared against caller input, so it must be well-formed UTF-8 before it
		// is folded into a comparison key. Invalid bytes would fold unpredictably and could make
		// an existing exclusion read as absent (sent again, failing the whole atomic batch) or a
		// new one read as present (silently dropped).
		if !utf8.ValidString(row.CampaignCriterion.Keyword.Text) {
			return nil, fmt.Errorf("google-ads: campaign %s negative keyword row at index %d carries a keyword text that is not valid UTF-8; refusing to trust this response", campaignID, i)
		}
		// The match type is HALF the comparison key, so it is checked like the text rather than
		// passed through. An unrecognised one (UNSPECIFIED, UNKNOWN, a new enum member) cannot
		// match any key the caller builds, so the exclusion it names would read as ABSENT, be
		// sent again, and fail the entire atomic batch — the exact outcome this read exists to
		// prevent, and one that would present as a platform rejection of keywords that were
		// perfectly acceptable. Refusing the response says what actually happened.
		switch row.CampaignCriterion.Keyword.MatchType {
		case MatchTypeExact, MatchTypePhrase, MatchTypeBroad:
		default:
			return nil, fmt.Errorf("google-ads: campaign %s negative keyword row at index %d reports the match type %q, which is not one of %s, %s, %s; refusing to trust this response",
				campaignID, i, row.CampaignCriterion.Keyword.MatchType, MatchTypeExact, MatchTypePhrase, MatchTypeBroad)
		}
		out = append(out, CampaignNegativeKeyword{
			CriterionID: row.CampaignCriterion.CriterionID,
			Text:        row.CampaignCriterion.Keyword.Text,
			MatchType:   row.CampaignCriterion.Keyword.MatchType,
		})
	}
	return out, nil
}

// AddCampaignNegativeKeywords attaches negative keyword criteria to an EXISTING campaign — the
// edit-path entry point, called by the dispatcher's add-negative-keywords lever.
//
// One atomic campaignCriteria:mutate, exactly as the create path sends: the whole list shares one
// outcome. The caller must have filtered out anything the campaign already excludes (see
// ListCampaignNegativeKeywords) — a duplicate is not dropped by Google, it fails the batch.
// Every guard below refuses BEFORE a request is built, so each is marked NotAttempted rather than
// returned plain. The dispatcher's DEFINITE arm stamps an unmarked error onto every keyword as
// REJECTED over HTTP 200 (internal/dispatch/googleads_negative_keywords.go), so a plain refusal
// here is a claim that Google refused keywords it was never sent — the same false sentence the
// pre-send transport guard below exists to prevent, arriving by a shorter route.
// validateAccountIDs is the one that is genuinely reachable: the dispatcher pre-checks the
// campaign id and the text/match-type rules, but nothing upstream validates the stored customer
// or login-customer id, which is the exact analogue of the malformed-ad-group-id defect the bid
// path already fixed.
func (c *Client) AddCampaignNegativeKeywords(ctx context.Context, campaignID string, negatives []Keyword) ([]string, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, &notAttemptedNegativeKeywordError{err: err}
	}
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, &notAttemptedNegativeKeywordError{err: err}
	}
	// Re-validated at the package boundary rather than trusted from the dispatcher: this is an
	// exported method, and the text/match-type rules are the ones the create path enforces, so a
	// keyword that could not have been CREATED must not become reachable by editing.
	validated, err := validateNegativeKeywords(negatives)
	if err != nil {
		return nil, &notAttemptedNegativeKeywordError{err: err}
	}
	if len(validated) == 0 {
		return nil, nil
	}
	campaignResource := "customers/" + c.account.CustomerID + "/campaigns/" + campaignID
	return c.mutateCampaignNegativeKeywords(ctx, campaignResource, campaignID, validated,
		// The definite-failure note the create path uses is FALSE here: this campaign was not
		// just created, and whatever exclusions it already had are untouched by a batch that
		// never applied. All that is lost is this request.
		"the campaign's existing negative keywords are unchanged and none of the requested ones were added")
}

// mutateCampaignNegativeKeywords is the shared mutate-and-verify core behind
// createCampaignNegativeKeywords (the create cascade) and AddCampaignNegativeKeywords (the edit
// lever). Both send the identical request and both verify the acknowledgement identically; the
// ONE thing that differs is what a definite failure MEANS, which is why definiteFailureNote is a
// parameter rather than a sentence baked in here. The create path's note names a brand-new
// campaign left with no exclusions at all; the edit path's names an unchanged campaign. Reusing
// either verbatim on the other path would tell the operator something untrue about the state the
// failure left behind.
//
// partialFailure is NOT set (as nowhere else in this client), so the batch is atomic: every
// keyword is applied or none is. That is deliberate for an exclusion list — a half-applied one is
// worse than none, because an operator looking at the campaign sees exclusions in place and has
// no reason to suspect the rest were dropped.
//
// The mutate is sent NON-idempotent (doRequest's last arg false), like every create in this
// client: campaignCriteria:mutate has no idempotency key, so a bounded retry after a 429 whose
// first attempt may already have committed would double-create the criteria.
func (c *Client) mutateCampaignNegativeKeywords(ctx context.Context, campaignResource, campaignID string, negatives []Keyword, definiteFailureNote string) ([]string, error) {
	if len(negatives) == 0 {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, &notAttemptedNegativeKeywordError{err: fmt.Errorf("google-ads negative keywords aborted before any request (context already done; campaign %s: %s): %w", campaignID, definiteFailureNote, ctxErr)}
	}

	ops := make([]mutateOperation, 0, len(negatives))
	for _, kw := range negatives {
		ops = append(ops, mutateOperation{Create: campaignNegativeKeywordCreate{
			Campaign: campaignResource,
			Negative: true,
			Keyword:  &keywordInfo{Text: kw.Text, MatchType: kw.MatchType},
		}})
	}

	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if err != nil {
		// ProbeNotSent runs BEFORE the ambiguity check because it proves the stronger fact. A
		// DNS failure or a connect-time refusal means this process never put the batch on the
		// wire, so the criteria cannot exist and "verify in Google Ads before retrying" would
		// send the operator to look at a campaign nothing was sent to. Without this, such a
		// failure carries no outcome marker at all and the dispatcher's DEFINITE arm reports
		// every keyword as a platform REJECTION over HTTP 200 — Google refusing keywords it
		// was never asked about, which is the same false claim the text-limit guard fixed.
		if ProbeNotSent(err) {
			return nil, &notAttemptedNegativeKeywordError{err: fmt.Errorf("google-ads negative keywords were not sent (campaign %s: %s): %w", campaignID, definiteFailureNote, err)}
		}
		if createOutcomeAmbiguous(err) {
			return nil, &unconfirmedNegativeKeywordError{err: fmt.Errorf("google-ads negative keywords UNCONFIRMED (campaign %s; negative keyword criteria may exist — verify in Google Ads before retrying): %w", campaignID, err)}
		}
		return nil, fmt.Errorf("google-ads negative keywords failed (campaign %s: %s): %w", campaignID, definiteFailureNote, err)
	}

	// Past this point Google returned 2xx, so nothing below may be reported as "not applied":
	// every remaining failure is about the ACKNOWLEDGEMENT being unusable, not about the mutate
	// not having happened.
	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, &unconfirmedNegativeKeywordError{err: fmt.Errorf("google-ads negative keywords UNCONFIRMED (campaign %s; 2xx with a malformed/short mutate response — negative keyword criteria may exist — verify in Google Ads before retrying)", campaignID)}
	}

	ids := make([]string, 0, len(ops))
	for i, r := range mr.Results {
		returnedCampaignID, critID := c.campaignCriterionID(r.ResourceName)
		if critID == "" || returnedCampaignID == "" {
			return nil, &unconfirmedNegativeKeywordError{err: fmt.Errorf("google-ads negative keywords UNCONFIRMED (campaign %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)}
		}
		// The campaignCriterion resourceName's campaign-id half must match the campaign these
		// negatives were created under — a mismatch means the response does not describe what
		// this call created, so the ids cannot be trusted enough to persist.
		if returnedCampaignID != campaignID {
			return nil, &unconfirmedNegativeKeywordError{err: fmt.Errorf("google-ads negative keywords UNCONFIRMED (campaign %s; campaignCriterion resource name %q reports a different campaign id %q — verify in Google Ads before retrying)", campaignID, r.ResourceName, returnedCampaignID)}
		}
		ids = append(ids, critID)
	}
	return ids, nil
}

// unconfirmedNegativeKeywordError marks a negative-keyword batch whose outcome is AMBIGUOUS — the
// request reached Google and the criteria may exist. Every message that already says UNCONFIRMED
// carries this type, so IsOutcomeUnconfirmed agrees with the sentence: without it the
// malformed-acknowledgement cases (a 2xx with an unusable body, which no error chain can be
// inferred from) would read as UNCONFIRMED to a human and as a definite failure to the caller
// classifying them.
type unconfirmedNegativeKeywordError struct{ err error }

func (e *unconfirmedNegativeKeywordError) Error() string     { return e.err.Error() }
func (e *unconfirmedNegativeKeywordError) Unwrap() error     { return e.err }
func (e *unconfirmedNegativeKeywordError) Unconfirmed() bool { return true }

// notAttemptedNegativeKeywordError marks a failure PROVEN to have left the batch unsent, so the
// criteria cannot exist. Three routes reach it: AddCampaignNegativeKeywords' own boundary guards,
// which refuse before a request is built; an already-cancelled context, caught at the same point;
// and anything ProbeNotSent proves after doRequest returns — a pre-send dial failure (DNS,
// connect-time refusal), or a context found already done at token acquisition, which is the one
// cancellation whose side of the wire is known. IsOutcomeUnconfirmed gives NotAttempted precedence
// over every shape-based inference precisely for these — they otherwise look exactly like a
// request cut off in flight, and would be reported as "the criteria may exist" when nothing was
// ever sent. Anything ProbeNotSent does not recognise stays unclaimed and falls through to the
// ambiguity check, which is the cheap direction: a cancellation landing after transmission is
// indistinguishable from one landing before it.
type notAttemptedNegativeKeywordError struct{ err error }

func (e *notAttemptedNegativeKeywordError) Error() string      { return e.err.Error() }
func (e *notAttemptedNegativeKeywordError) Unwrap() error      { return e.err }
func (e *notAttemptedNegativeKeywordError) NotAttempted() bool { return true }

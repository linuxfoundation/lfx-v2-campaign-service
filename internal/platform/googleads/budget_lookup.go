// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ambiguousBudgetResolveTimeout is a wall-clock cap on the whole of retryFindBudgetByName,
// and so on both of its callers. On the ambiguous-create path the re-reads run on a context
// detached from the caller's, so this is the ONLY thing bounding them, and the caller is
// still waiting on them; on the unreadable-2xx recovery path the caller's own context
// bounds them too, and this ceiling is what stops four 30s searches from being spent before
// a campaign mutate that still has to happen.
//
// It is deliberately NOT computed from the searches it contains: one gaqlSearch can cost
// several pages, each with its own googleAdsRequestTimeout (30s) attempt deadline, so a
// budget would have to cover minutes to bound the worst case — which would be a far worse
// answer than giving up. Four lightweight single-page searches plus 1750ms of sleep fit
// inside twenty seconds with room to spare whenever the account is answering at all.
//
// The trade-off it buys, and its direction: on an account slow enough to exhaust this, a
// budget that IS live may go unfound and the arm reports the failure as unsettled rather
// than as an absence. That is the fail-closed side — nothing is claimed either way — but
// it is a real loss of the reporting this arm exists for, so the ceiling is sized to be
// reached only by an account that is effectively not answering.
const ambiguousBudgetResolveTimeout = 20 * time.Second

// Retry schedule for the ONE outcome that is not self-evident: no live budget of this name
// visible yet. See resolveBudgetAfterAmbiguousCreate for why that outcome is ambiguous.
//
// The attempt cap, not the timeout, is what normally ends the loop; it bounds what this
// costs on the path where nothing is wrong. N attempts sleep N-1 times, so these four
// attempts span 250+500+1000 = 1750ms of sleep plus four round trips. Retrying until the
// ceiling instead would add twenty seconds to every genuinely-failed create, and each
// attempt is a real googleAds:search against the account's quota — so the schedule buys
// the replica-lag window without turning a clean failure into a slow one.
const (
	ambiguousBudgetResolveAttempts = 4
	ambiguousBudgetResolveMinDelay = 250 * time.Millisecond
	ambiguousBudgetResolveMaxDelay = 1 * time.Second
)

// ---------------------------------------------------------------------------
// Campaign budget reuse: resolving a DUPLICATE_NAME refusal at Step 1.
//
// Every channel creates its budget first and its campaign second, and the budget's
// composed name is this client's idempotency key. When the campaign mutate fails —
// which is exactly what the v23 Performance Max blockers did, twice, against the live
// account — the budget is already committed and the name is taken. Every later retry
// of the same brief then fails at Step 1 with CampaignBudgetError.DUPLICATE_NAME and
// never reaches the campaign again.
//
// That used to be terminal in practice. The orphan is created with
// explicitly_shared=false, and Google's Shared Library → Budgets page lists only
// explicitly-shared budgets; with no campaign attached there is no campaign row to open
// it from either. So the operator the old error message sent to "verify in Google Ads"
// had no surface on which to see or delete the thing blocking them.
//
// The way out is to ADOPT the budget the name refers to instead of failing. The whole
// risk of that is attaching a paid campaign to a budget this dispatch has no business
// using, so every check below is a precondition for reuse and every unverifiable answer
// is a refusal.
// ---------------------------------------------------------------------------

// reusableBudget is a campaign budget that already exists AND has been proved to be the
// one this dispatch was about to create. Only findReusableBudget constructs it.
type reusableBudget struct {
	// ResourceName is rebuilt from this client's customer id and the canonical budget
	// id — never echoed from the response — so the string handed to campaigns:mutate
	// cannot be a resource name the server named and we merely glanced at.
	ResourceName string
	ID           string
	// AmountMicros is the amount the budget currently carries, before any alignment.
	AmountMicros int64
}

// reuseBudgetAfterDuplicateName is what every channel's DUPLICATE_NAME arm calls. It
// proves the existing budget is this dispatch's own orphan, brings its amount into line
// with the brief if it has drifted, and returns the resource name to attach the campaign
// to, together with the steps that must appear in the operator-visible result.
//
// On ANY refusal it returns an error and the caller keeps failing exactly as it did
// before this existed — the dead end is only closed when every gate says yes.
//
// WHY IT ALIGNS THE AMOUNT RATHER THAN REFUSING ON A MISMATCH. The budget being adopted
// is, by the time it gets here, provably unattached and non-shared: nothing is spending
// from it and nothing else can. Refusing a reuse because the brief now says $400 and the
// orphan says $500 would re-impose precisely the dead end this function exists to close,
// on an operator who has no UI path to delete a non-shared budget with no campaign on it.
// Aligning instead reaches the SAME end state a clean first-time create would have
// produced, and the campaign is created PAUSED either way, so no spend can occur before a
// human enables it. The alignment is recorded in steps rather than done quietly, and a
// failed alignment fails the whole create — attaching a campaign to a budget carrying an
// amount nobody asked for is the one outcome worse than failing.
func (c *Client) reuseBudgetAfterDuplicateName(ctx context.Context, budgetName string, amountMicros int64, budgetDisplay float64) (resourceName, budgetID string, steps []string, err error) {
	budget, err := c.findReusableBudget(ctx, budgetName)
	if err != nil {
		return "", "", nil, err
	}

	steps = append(steps, fmt.Sprintf("Campaign budget reused: %s (existing unattached non-shared budget of the same name, left by a prior attempt)", budget.ID))

	if budget.AmountMicros != amountMicros {
		// UpdateCampaignBudget requires the caller to have established, from a readback
		// on this same connection, that the budget is not shared. findReusableBudget did
		// exactly that — and refused on a missing explicitly_shared rather than assuming.
		if err := c.UpdateCampaignBudget(ctx, budget.ID, amountMicros, budgetPeriodDaily); err != nil {
			return "", "", nil, fmt.Errorf("google-ads campaign budget %s named %q carries %d micros but this brief asks for %d, and aligning it failed; refusing to attach a campaign to a budget with an amount nobody asked for: %w", budget.ID, budgetName, budget.AmountMicros, amountMicros, err)
		}
		steps = append(steps, fmt.Sprintf("Campaign budget %s amount aligned to the brief: %.2f/day in account currency (was %.2f)", budget.ID, budgetDisplay, float64(budget.AmountMicros)/microsPerUnit))
	}

	return budget.ResourceName, budget.ID, steps, nil
}

// budgetLookupRow is one row of the findReusableBudget SELECT. Every field is here
// because a reuse decision reads it; nothing is selected "for logging".
type budgetLookupRow struct {
	CampaignBudget struct {
		ResourceName string `json:"resourceName"`
		ID           string `json:"id"`
		Name         string `json:"name"`
		Status       string `json:"status"`
		// ExplicitlyShared is the gate that separates "an orphan this service left
		// behind" from "a Shared Library budget somebody else manages". A pointer, so
		// an OMITTED field is distinguishable from a present false — absent means the
		// response never said, and a reuse may not be granted on a field that was
		// never answered.
		ExplicitlyShared *bool `json:"explicitlyShared"`
		// ReferenceCount is how many campaigns use this budget. int64 on the wire, so
		// JSON-encoded as a string.
		ReferenceCount string `json:"referenceCount"`
		AmountMicros   string `json:"amountMicros"`
		Period         string `json:"period"`
		DeliveryMethod string `json:"deliveryMethod"`
		Type           string `json:"type"`
	} `json:"campaignBudget"`
}

// budgetReferenceRow is one row of the "does any campaign use this budget" SELECT. It
// mirrors campaignLookupRow's identity fields so campaignRowIdentity can judge the row,
// plus campaign_budget so the server-side filter can be re-checked client-side.
type budgetReferenceRow struct {
	Campaign struct {
		ResourceName   string `json:"resourceName"`
		ID             string `json:"id"`
		Name           string `json:"name"`
		Status         string `json:"status"`
		CampaignBudget string `json:"campaignBudget"`
	} `json:"campaign"`
}

// Google's BudgetStatus enum has exactly two real values — there is no paused budget.
const (
	budgetStatusEnabled = "ENABLED"
	budgetStatusRemoved = "REMOVED"
)

// budgetDeliveryStandard and budgetTypeStandard are the two shapes campaignBudgetCreate
// produces: it sends deliveryMethod STANDARD explicitly, and sends no type at all, which
// Google defaults to STANDARD. A budget that is neither is not the budget this dispatch
// was about to create, whatever its name says.
const (
	budgetDeliveryStandard = "STANDARD"
	budgetTypeStandard     = "STANDARD"
)

// canonicalBudgetID is canonicalCampaignID applied to a campaign budget id.
//
// It delegates rather than duplicating because the check is about SPELLING, not about
// campaigns: Google exposes every resource id as an int64, so "0", "007" and a value past
// math.MaxInt64 are the same three non-ids here as there, and the leading-zero case is
// the same trap — "007" and "7" are one budget to the server and two strings to the
// two-fields-disagree check below. It is a named wrapper rather than a bare call so that
// anyone narrowing canonicalCampaignID for a campaign-specific reason sees that budgets
// depend on it too.
func canonicalBudgetID(s string) string {
	return canonicalCampaignID(s)
}

// budgetIDFromResourceName extracts a budget id from a resource name, accepting ONLY
//
//	customers/{this account's customer id}/campaignBudgets/{digits}
//
// and returning "" for everything else, which the caller turns into a fail-closed error.
// Deliberately stricter than the package's resourceID helper for the same reason
// campaignIDFromResourceName is: resourceID takes the trailing path segment, which is
// right for parsing back a mutate WE issued, and wrong here, where the resource name is
// identity evidence for a budget about to fund a campaign — "after the last slash"
// accepts "garbage/4242" and another customer's budget.
func (c *Client) budgetIDFromResourceName(resourceName string) string {
	parts := strings.Split(resourceName, "/")
	if len(parts) != 4 || parts[0] != "customers" || parts[2] != "campaignBudgets" {
		return ""
	}
	if parts[1] != c.account.CustomerID {
		return ""
	}
	return canonicalBudgetID(parts[3])
}

// findReusableBudget answers one question: may the campaign this dispatch is about to
// create be attached to the budget that already holds budgetName?
//
// It is called ONLY after Google has refused campaignBudgets:mutate with
// CampaignBudgetError.DUPLICATE_NAME, which is what makes "no live budget by that name"
// a contradiction rather than an absence — so unlike FindCampaignByName there is no
// clean-absence return. Every exit is either a budget that passed every gate, or an
// error:
//
//   - exactly one live, non-shared, unattached, same-shape budget -> (budget, nil)
//   - anything else, including no match at all                    -> (nil, error)
//
// That contract is the DUPLICATE_NAME caller's, not the lookup's: the search and the
// gates live in findBudgetByName, which reports a clean absence, and this function is the
// thin layer that turns that absence into the contradiction it is HERE. Callers for whom
// an absence is an ANSWER rather than a contradiction — the ambiguous-create arms, where
// Google refused nothing and "no budget exists" is the best possible outcome — call
// findBudgetByName instead. Bending this function to serve them would have it assert a
// duplicate refusal that never happened.
//
// A false NEGATIVE here costs a retry that was already failing. A false POSITIVE funds a
// paid campaign from a budget somebody else controls, so the asymmetry runs one way and
// every gate is a conjunction.
//
// WHY REUSE IS SOUND AT ALL. A non-shared budget's name SYNCHRONIZES to its campaign's
// name once a campaign attaches to it. An attempt that got past the campaign mutate
// therefore FREED this name, and this code path would not have been reached. Reaching it
// means the name is held by a budget with no campaign on it — the orphan shape. That is
// the ARGUMENT for reuse; it is not the evidence, because it rests on Google's behaviour
// as this package documents it rather than on anything this service observed. The gates
// in findBudgetByName are the evidence, and they are checked even though the argument
// predicts them.
func (c *Client) findReusableBudget(ctx context.Context, budgetName string) (*reusableBudget, error) {
	budget, found, err := c.findBudgetByName(ctx, budgetName)
	if err != nil {
		return nil, err
	}
	if !found {
		// Google refused the create as a duplicate name and the account says no live
		// budget carries that name. Both cannot be true. Something between the two calls
		// is wrong — a stale read replica, the wrong account, a name that does not
		// round-trip — and none of those is a safe footing for attaching a campaign.
		return nil, fmt.Errorf("google-ads campaign budget lookup: Google refused the create because %q already exists, but no live budget of that name is visible in account %s; refusing to proceed on two answers that contradict each other", budgetName, c.account.CustomerID)
	}
	return budget, nil
}

// ---------------------------------------------------------------------------
// Campaign budget recovery: resolving a 2xx whose resource name will not parse.
//
// The other dead end at Step 1, and the one with the least excuse for existing.
// campaignBudgets:mutate answers 2xx — Google committed the budget — but the response
// carries no `results[0].resourceName`, or one with no id segment, so firstResourceName
// cannot hand back anything to attach a campaign to. Before this, all five channels failed
// on the spot, leaving a budget that is non-shared and unattached: invisible in Shared
// Library → Budgets, invisible on any campaign row, and holding the composed name that
// blocks every later retry of the same brief. The same orphan class the DUPLICATE_NAME
// arms close, arrived at from the opposite direction.
//
// What makes this the EASIER of the two to close: Google said yes. There is no refusal to
// contradict and no question about whether the write landed — only the id is lost, and the
// name it was created under is this client's own idempotency key, so a lookup by that name
// is a lookup for a budget that provably exists.
// ---------------------------------------------------------------------------

// budgetFromCreateResponse is the WHOLE of every channel's "read the budget out of a 2xx"
// step, so the five cannot drift on a failure none of them can test against the live
// account. channel is the channel word the existing messages use — "performance max",
// "campaign", "demand gen", "display", "video".
//
// The ordinary path is unchanged and costs nothing: firstResourceName parses, and the
// returned recoveryStep is empty. Only an unparseable 2xx reaches the account.
//
// recoveryStep is returned separately rather than folded into the caller's "Campaign budget
// created" line because the five channels word that line differently (campaign.go names the
// delivery method and the sharing flag; the rest do not), and a recovery is worth saying in
// its own sentence anyway — it tells the operator the id in the result came from a readback
// rather than from the create, which is the difference between "Google told us" and "we went
// and looked".
func (c *Client) budgetFromCreateResponse(ctx context.Context, channel, budgetName string, body []byte) (resourceName, budgetID, recoveryStep string, err error) {
	resourceName, budgetID, parseErr := firstResourceName(body)
	if parseErr == nil {
		return resourceName, budgetID, "", nil
	}

	budget, recoverErr := c.recoverBudgetAfterUnreadableCreate(ctx, budgetName)
	if recoverErr != nil {
		// BOTH are wrapped and the parse failure goes FIRST, for the reason every other arm
		// in this file spells out: errors.As stops at the first *apiError in the chain, so an
		// apiError raised by the lookup READ must not be mistaken for an answer about the
		// create. The sentence does NOT say "UNCONFIRMED (may exist)" as this arm used to —
		// a 2xx is Google confirming the create, so the budget's EXISTENCE is settled and
		// only its identity is not. Telling an operator it "may exist" would send them
		// looking for something that is definitely there.
		return "", "", "", fmt.Errorf("google-ads %s budget %q WAS created (campaignBudgets:mutate answered success) but its response carried no usable resource name, and the account did not settle which budget it is, so the campaign was never attempted; find it by name in Google Ads change history before retrying: %w — and the account did not settle it: %w", channel, budgetName, parseErr, recoverErr)
	}

	// Never the resource name the server named and we merely glanced at: reusableBudget
	// rebuilds it from this client's customer id and the canonical budget id, which is the
	// same string a clean create would have produced and the same one the DUPLICATE_NAME
	// arms attach to.
	return budget.ResourceName, budget.ID, fmt.Sprintf("Campaign budget id %s recovered by lookup: the create succeeded but its response carried no usable resource name", budget.ID), nil
}

// recoverBudgetAfterUnreadableCreate finds the budget a 2xx campaignBudgets:mutate just
// created when the response would not say which one it is.
//
// It is deliberately findReusableBudget's shape and not ambiguousBudgetCreateOutcome's: an
// ABSENCE HERE IS A CONTRADICTION, not an answer. Google reported success, so a budget of
// this name exists; an account that then shows none has told us two things that cannot both
// be true — a stale read replica, the wrong account, a name that did not round-trip — and
// none of those is a safe footing for attaching a paid campaign. The ambiguous arm's
// "most likely nothing was created" default has no counterpart here, because the create
// was never in doubt.
//
// THE RE-READS MATTER MORE HERE THAN THERE, for the same reason the contradiction is
// sharper: the commit is CONFIRMED, so a first read that misses is pure replica lag and
// nothing else. In the ambiguous arm a zero-row answer can legitimately mean the write
// never landed; here it cannot mean anything but "not visible yet", right up until the
// schedule runs out.
//
// The gates in findBudgetByName are checked even though the argument predicts them, exactly
// as findReusableBudget checks them: the campaign about to be attached is billable, and
// "Google said 2xx moments ago" is an argument, not evidence about the row that comes back.
func (c *Client) recoverBudgetAfterUnreadableCreate(ctx context.Context, budgetName string) (*reusableBudget, error) {
	budget, found, err := c.retryFindBudgetByName(ctx, budgetName, "after a create whose response carried no usable resource name")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("google-ads campaign budget lookup: Google answered the create with success, so %q exists, but no live budget of that name is visible in account %s after %d reads; refusing to proceed on two answers that contradict each other", budgetName, c.account.CustomerID, ambiguousBudgetResolveAttempts)
	}
	return budget, nil
}

// findBudgetByName is the search and the identity gates, with no opinion about what an
// absence MEANS. It is the shared core of every "what is actually in the account under
// this name" question this client asks.
//
// Three exits, and the middle one is the whole point of the split:
//
//   - exactly one live, non-shared, unattached, same-shape budget -> (budget, true, nil)
//   - no live budget of that name at all                          -> (nil, false, nil)
//   - anything else                                               -> (nil, false, error)
//
// The third exit swallows nothing: a budget that EXISTS under this name but fails a gate
// — shared, attached, wrong pacing, unreadable — is an error and never a false. Only an
// empty result set is an absence. That keeps the fail-closed direction intact for both
// callers: whatever they decide an absence means, they only ever decide it about a name
// the account genuinely has nothing live under.
func (c *Client) findBudgetByName(ctx context.Context, budgetName string) (*reusableBudget, bool, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, false, err
	}
	// Bound the name with the SAME limit and the SAME UNIT the create path enforces —
	// maxBudgetNameBytes, UTF-8 BYTES, see preflightCampaignKind. Campaign names are
	// bounded in runes and budgets are not, so measuring this one in runes would be the
	// wrong ceiling in the lenient direction.
	if err := validateEntityName("budget", budgetName, len(budgetName), maxBudgetNameBytes, "UTF-8 bytes"); err != nil {
		return nil, false, err
	}

	literal, err := gaqlNameLiteral("campaign budget", budgetName)
	if err != nil {
		return nil, false, err
	}

	// The name and the tombstone filter are applied SERVER-side so a miss costs one page
	// rather than a walk of every budget in the account; both are re-checked on the rows
	// below, where a disagreement is an error.
	query := "SELECT campaign_budget.id, campaign_budget.name, campaign_budget.status, " +
		"campaign_budget.resource_name, campaign_budget.explicitly_shared, " +
		"campaign_budget.reference_count, campaign_budget.amount_micros, " +
		"campaign_budget.period, campaign_budget.delivery_method, campaign_budget.type " +
		"FROM campaign_budget WHERE campaign_budget.name = " + literal +
		" AND campaign_budget.status != '" + budgetStatusRemoved + "'"

	rows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: %w", err)
	}

	var (
		matches []*reusableBudget
		seen    = map[string]struct{}{}
	)
	for _, raw := range rows {
		// The same raw-bytes guard FindCampaignByName applies, for the same reason: the
		// echo-check below looks like it covers this, and it covers every name EXCEPT one
		// that already contains U+FFFD — which is a legal budget name. Ask for `bad<FFFD>`,
		// receive raw bytes `"bad\xff"`, and encoding/json substitutes the very rune that
		// was asked for, so the echo-check passes on a value it never saw intact.
		if !utf8.Valid(raw) || hasUnpairedSurrogateEscape(raw) {
			return nil, false, fmt.Errorf("google-ads campaign budget lookup: exact-match query for %q returned a row whose name cannot survive JSON decoding intact (malformed UTF-8 bytes, or an unpaired surrogate escape); decoding it would substitute U+FFFD and could echo back the requested name from a value that never matched it", budgetName)
		}
		if hasDuplicateKeys(raw) {
			return nil, false, fmt.Errorf("google-ads campaign budget lookup: exact-match query for %q returned a row that declares the same JSON key twice; the row identifies more than one budget and no reading of it can be trusted", budgetName)
		}
		var row budgetLookupRow
		if err := json.Unmarshal(raw, &row); err != nil {
			// A 2xx row we cannot decode is NOT a non-match. Both callers read a
			// non-match as a fact about the account — one as a contradiction, one as
			// proof a write never landed — and an undecodable row is evidence for
			// neither.
			return nil, false, fmt.Errorf("google-ads campaign budget lookup: decoding result row: %w", err)
		}

		// Re-checking the name client-side is what makes an escaping regression LOUD: if
		// gaqlNameLiteral ever stopped neutralising a quote, the injected query would
		// still return 2xx, with rows for OTHER budgets. A mismatch is an ERROR, not a
		// skip — a row that is not an exact match means the filter did not take effect,
		// which invalidates the WHOLE response, and skipping would quietly reduce an
		// unhonoured query to "no rows matched". Byte-exact, matching ComposeName.
		if row.CampaignBudget.Name != budgetName {
			return nil, false, fmt.Errorf("google-ads campaign budget lookup: exact-match query for %q returned a budget named %q; the name filter was not honoured, refusing to trust this response", budgetName, row.CampaignBudget.Name)
		}

		b, live, err := c.budgetRowIdentity(row, budgetName)
		if err != nil {
			return nil, false, err
		}
		if !live {
			continue
		}
		if _, dup := seen[b.ID]; dup {
			// GAQL can return one resource on several rows. Counting rows rather than
			// distinct budgets would report a single budget as ambiguous and refuse a
			// reuse that is in fact unambiguous.
			continue
		}
		seen[b.ID] = struct{}{}
		matches = append(matches, b)
	}

	switch len(matches) {
	case 0:
		// The ONLY clean absence: the query was honoured, every row it returned was
		// readable, and nothing live carries this name. What that absence means is the
		// caller's to decide.
		return nil, false, nil
	case 1:
		// Fall through to the reuse gates.
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: %d live budgets in account %s are named %q (ids %v) — refusing to choose one", len(matches), c.account.CustomerID, budgetName, ids)
	}
	budget := matches[0]

	// The budget must be unattached, and this is the check that earns the reuse. A
	// budget with a campaign on it is a budget that campaign is spending from; pointing a
	// second campaign at it would split one daily amount across two campaigns nobody
	// agreed to fund together.
	//
	// reference_count already answered this on the row above. It is asked AGAIN from the
	// campaign side, because reference_count is documented as the number of campaigns
	// ACTIVELY using the budget, and this service creates every campaign PAUSED — so the
	// one shape most likely to exist in the account is also the shape that phrase is
	// least clear about. Two independent answers that must agree.
	if err := c.assertBudgetUnattached(ctx, budget, budgetName); err != nil {
		return nil, false, err
	}

	return budget, true, nil
}

// ambiguousBudgetCreateOutcome is the WHOLE of every channel's ambiguous budget-create
// arm: it resolves the ambiguity, picks the partial, and composes the sentence, so the
// five channels cannot drift from each other on a failure none of them can test against
// the live account. channel is the channel word the existing messages use — "performance
// max", "campaign", "demand gen", "display", "video" — and namePartial is the caller's own
// pre-budget partial builder.
//
// Every arm returns a partial and keeps the brief's claim. What changes is what the
// partial and the sentence SAY: a budget that is found puts its id on the partial, which
// is what reclassifies the failure for CreateOutcome, and an absence that survives the
// re-reads is reported as such in words. No arm returns a nil result — an ambiguous
// mutating create is not allowed to resolve into a pre-create failure on the strength of
// a read (see the default arm).
func (c *Client) ambiguousBudgetCreateOutcome(ctx context.Context, channel, budgetName string, namePartial func() *CampaignResult, createErr error) (*CampaignResult, error) {
	budgetID, budgetExists, resolveErr := c.resolveBudgetAfterAmbiguousCreate(ctx, budgetName)
	switch {
	case resolveErr != nil:
		// Nothing was settled, so nothing is claimed: this is the sentence the arm
		// returned before any of this existed. The create error is wrapped FIRST for the
		// same load-bearing reason the DUPLICATE_NAME arms spell out — errors.As stops at
		// the first *apiError in the chain, so an apiError from the lookup READ must not
		// shadow the create's and cost this failure its create-outcome token.
		//
		// "did not settle it" rather than "could not be read": this arm also fires when
		// the account WAS read and a gate refused the row it returned — two live budgets
		// of the name, an explicitly shared one, an attached one, a wrong-shaped or
		// undecodable one. Naming only the read failure would send the operator to check
		// connectivity when the actual answer is in the wrapped resolveErr.
		//
		// A gate refusal is where this arm is weakest, and deliberately so. Every gate
		// error names the budget's ID, and resolveErr is wrapped here, so the operator IS
		// told which budget to go and look at — including in the case most likely to
		// arise here, a row whose explicitly_shared arrives omitted (the gate refuses
		// rather than read absent as false). What the refusal costs is the machine-
		// readable CampaignBudgetID, and that is the right trade: the gates are what
		// establish the row is THIS dispatch's budget at all — the name-synchronises
		// argument holds for non-shared budgets only — so putting an id on the partial
		// for a row that failed them would claim a creation the account has not
		// confirmed. Reported in words, never claimed in the result.
		return namePartial(), fmt.Errorf("google-ads %s budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w — and the account did not settle it: %w", channel, budgetName, createErr, resolveErr)
	case budgetExists:
		// The budget landed. Carrying its id is what turns the generic unconfirmed
		// message into CreateOutcomeBudgetCreatedCampaignUnconfirmed (see CreateOutcome) —
		// a budget the operator can act on instead of one they have to go looking for in
		// change history.
		r := namePartial()
		r.CampaignBudgetID = budgetID
		return r, fmt.Errorf("google-ads %s budget %s (%q) WAS created despite an unconfirmed create response, and the campaign was never attempted; the budget is unattached so it accrues no spend, and a retry of this brief will reuse it: %w", channel, budgetID, budgetName, createErr)
	default:
		// The account shows no live budget of this name, after the corroborating re-reads
		// resolveBudgetAfterAmbiguousCreate makes. That is worth SAYING — it is a much
		// better starting point than "it may exist" — but it is not worth CLAIMING.
		//
		// An ambiguous mutating create must produce a partial result and retain the claim
		// (docs/reviews/knowledge-base/context-and-cancellation.md,
		// presend-clean-inflight-unconfirmed), and the reason survives the re-reads: an
		// absence is a property of the READ, and this same file refuses to act on a
		// zero-row answer from this same query where it can be contradicted, naming a
		// stale read replica, the wrong account, and a name that does not round-trip. A
		// nil result here would release the brief's claim on the strength of that read,
		// so a budget that did commit and was simply not visible would be reported as
		// never created — the exact orphan shape this arm exists to close. The partial
		// therefore still carries the names, and the outcome stays UNCONFIRMED.
		return namePartial(), fmt.Errorf("google-ads %s budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w — no budget of that name is live in the account, checked %d times, so most likely nothing was created", channel, budgetName, createErr, ambiguousBudgetResolveAttempts)
	}
}

// resolveBudgetAfterAmbiguousCreate answers the question the ambiguous budget-create arm
// has never been able to answer: did the budget actually get created?
//
// The arm is reached when campaignBudgets:mutate failed in a way that does not say
// whether the write landed — a 5xx, an exhausted 429, a transport error after the request
// was on the wire. Before this existed, every such failure returned a partial carrying no
// budget id, so a budget that HAD been committed became an orphan nobody was told about,
// and the retry it blocked failed differently one call later.
//
// WHY THE ACCOUNT MAY BE READ FOR AN ANSWER HERE. This is Step 1: campaigns:mutate has not
// been issued, on this attempt or on any attempt that reached this arm. A non-shared
// budget's name synchronises to its campaign's name on attach, so no campaign can have
// taken this name and renamed its budget — a live budget found under budgetName is this
// client's own, and an absence is this client's own too. The same reasoning licenses
// reuseBudgetAfterDuplicateName, and it is STRONGER here, where Google refused nothing and
// so cannot be contradicted.
//
// Three outcomes, and every one of them is better than the single outcome it replaces:
//
//   - (id, true, nil)  — the budget exists; the caller puts its id on the partial so the
//     operator is told what to reconcile instead of being sent to hunt for it
//   - ("", false, nil) — no budget of this name is visible after the re-reads below; the
//     caller may SAY so, which is a far better starting point than "it may exist", but it
//     may not treat it as proof that nothing was written
//   - ("", false, err) — the account could not be read, or a budget of this name exists
//     that fails a gate. Nothing was settled, so the caller keeps saying exactly what it
//     said before this existed
//
// THE READ RUNS ON A DETACHED, BOUNDED CONTEXT, for the same reason
// ambiguousCommitReconcileTimeout is detached in audience_repo.go: one of the three shapes
// that reach this arm is a transport error from httpClient.Do, and a cancelled or expired
// caller context produces exactly that. Reusing ctx would make the feature inert in one of
// the shapes most likely to leave an orphan — the search would fail immediately and the
// arm would fall through to "did not settle it" — while still costing a doomed round trip.
// The caller is already on its way out with an error; a client disconnect must not be the
// reason an orphan goes unreported.
//
// ONLY THE NO-ROW OUTCOME IS RETRIED, and the schedule that does it lives in
// retryFindBudgetByName, which both post-create lookups share. A zero-row search does not
// prove the write rolled back: this arm is reached when campaignBudgets:mutate failed
// without saying whether the write landed, including while Google was still committing it.
// Treating the first zero-row answer as final would leave this blind to a subset of its own
// reason for existing: a budget committed moments before is exactly the one a single
// immediate read can miss, and it is the one the caller most needs to hear about, since the
// id it would carry is what makes the orphan actionable.
func (c *Client) resolveBudgetAfterAmbiguousCreate(ctx context.Context, budgetName string) (budgetID string, created bool, err error) {
	// The detachment is this caller's, not the schedule's: only the ambiguous arm is reached
	// by a shape that can BE the caller's cancelled context. retryFindBudgetByName owns the
	// ceiling and applies it to whatever context it is handed.
	budget, found, err := c.retryFindBudgetByName(context.WithoutCancel(ctx), budgetName, "after an unconfirmed create")
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}
	return budget.ID, true, nil
}

// retryFindBudgetByName is findBudgetByName on the schedule above: the ONLY outcome it
// retries is the no-row one, and it is bounded by ambiguousBudgetResolveTimeout so that a
// slow account ends the re-reads rather than the caller waiting out four 30s searches.
//
// WHY A ZERO-ROW ANSWER IS THE ONLY THING WORTH ASKING TWICE. Both callers reach the
// account moments after a campaignBudgets:mutate that may have committed, and the Google
// Ads search surface is not guaranteed read-after-write consistent — findReusableBudget's
// own contradiction message names a stale read replica as one explanation it refuses to
// proceed on. A budget committed moments before is exactly the one a single immediate read
// can miss. A found budget, or any gate error, settles the question and is never retried.
//
// occasion names what the caller was doing, and appears only in the ceiling error, because
// that is the one message that has to send an operator somewhere: the two callers are
// looking at different account states and "we never finished re-reading" means a different
// thing to each.
//
// It takes the context it is given and does NOT detach: whether the caller's context can
// itself be the reason the account is unreadable is the caller's question, not the
// schedule's. resolveBudgetAfterAmbiguousCreate detaches before calling in;
// recoverBudgetAfterUnreadableCreate deliberately does not, because its create was answered
// 2xx on a live context that the campaign mutate is about to use anyway.
func (c *Client) retryFindBudgetByName(ctx context.Context, budgetName, occasion string) (*reusableBudget, bool, error) {
	rctx, cancel := context.WithTimeout(ctx, ambiguousBudgetResolveTimeout)
	defer cancel()

	delay := ambiguousBudgetResolveMinDelay
	for attempt := 1; ; attempt++ {
		budget, found, err := c.findBudgetByName(rctx, budgetName)
		if err != nil {
			return nil, false, err
		}
		if found {
			return budget, true, nil
		}
		if attempt >= ambiguousBudgetResolveAttempts {
			return nil, false, nil
		}

		timer := time.NewTimer(delay)
		select {
		case <-rctx.Done():
			timer.Stop()
			// The ceiling was reached before the attempts were, so the re-reads never
			// finished and the absence is reported as unsettled rather than as an absence.
			return nil, false, fmt.Errorf("resolving budget %q %s: %w", budgetName, occasion, rctx.Err())
		case <-timer.C:
		}
		if delay *= 2; delay > ambiguousBudgetResolveMaxDelay {
			delay = ambiguousBudgetResolveMaxDelay
		}
	}
}

// budgetRowIdentity answers "which budget is this row, and may it be reused" for ONE row,
// and is the single place that question is answered.
//
// IDENTITY IS ESTABLISHED BEFORE EVERYTHING ELSE, exactly as campaignRowIdentity does and
// for the same reason: "a removed budget is unusable however it arrived" is a claim ABOUT
// a budget, so it means nothing until the row has said which budget it is. A malformed or
// cross-customer row is rejected before its status, its sharing flag or its amount is ever
// read.
//
// live is false ONLY for a REMOVED budget — the one state that is safe to drop silently,
// because a tombstone can never be attached to anything. Every other unrecognised status
// is an ERROR rather than a skip: Google can return UNKNOWN or UNSPECIFIED, and an omitted
// field decodes to "", so treating one as live would fund a campaign from a budget whose
// state was never established, and treating it as a skip would reduce an unverifiable
// response to the "no live budget" contradiction above and blame the wrong thing.
func (c *Client) budgetRowIdentity(row budgetLookupRow, budgetName string) (b *reusableBudget, live bool, err error) {
	// NOT trimmed, and the resource name is validated WHENEVER it is present rather than
	// only as a fallback for a missing id: both fields were selected, so both are evidence
	// of what this row IS, and a row carrying id "4242" beside a resource name that is
	// blank-but-present or points at another customer identifies no single budget in this
	// account. Absent and present-but-garbage are exactly the distinction this keeps.
	id := row.CampaignBudget.ID
	fromName := c.budgetIDFromResourceName(row.CampaignBudget.ResourceName)
	switch {
	case row.CampaignBudget.ResourceName != "" && fromName == "":
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget named %q was returned with resource name %q, which is not customers/%s/campaignBudgets/{id}; the row identifies no budget in this account", budgetName, row.CampaignBudget.ResourceName, c.account.CustomerID)
	case id == "" && fromName == "":
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget named %q was returned with neither a usable id nor a usable resource name", budgetName)
	case id == "":
		id = fromName
	case canonicalBudgetID(id) == "":
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget named %q was returned with id %q, which is not the canonical base-10 spelling of a positive int64", budgetName, row.CampaignBudget.ID)
	case fromName != "" && fromName != id:
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget named %q was returned with id %q but resource name %q; the row disagrees with itself about which budget it is", budgetName, id, row.CampaignBudget.ResourceName)
	}

	switch row.CampaignBudget.Status {
	case budgetStatusRemoved:
		return nil, false, nil
	case budgetStatusEnabled:
		// Live. Fall through to the reuse gates.
	default:
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget %s named %q has unrecognised status %q (want %s or %s); refusing to treat it as reusable", id, budgetName, row.CampaignBudget.Status, budgetStatusEnabled, budgetStatusRemoved)
	}

	// THE SHARING GATE. A shared budget is a Shared Library object the account manages and
	// funds other campaigns from; attaching this dispatch's campaign to one would spend
	// somebody else's pot. It is also the gate that makes the name mean what this code
	// assumes it means — the name-synchronises-to-the-campaign behaviour the reuse
	// argument rests on is a property of NON-shared budgets only, so on a shared budget
	// the name is not evidence of anything and nothing below would be load-bearing.
	//
	// A MISSING field is refused separately from a true one. Google omits false booleans
	// from REST responses in some shapes, so "absent" cannot be read as "not shared"
	// without granting exactly the permission this gate exists to withhold.
	switch {
	case row.CampaignBudget.ExplicitlyShared == nil:
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget %s named %q was returned without campaign_budget.explicitly_shared; refusing to reuse a budget whose sharing is unknown", id, budgetName)
	case *row.CampaignBudget.ExplicitlyShared:
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget %s named %q is explicitly shared; this dispatch creates non-shared budgets and must not attach a campaign to a shared-library budget", id, budgetName)
	}

	// SHAPE GATES. campaignBudgetCreate sends deliveryMethod STANDARD and no type (which
	// Google defaults to STANDARD), and every channel's budget is DAILY. A budget that
	// differs on any of these is not the budget this dispatch was about to create, however
	// its name reads — and period in particular decides WHICH amount field holds the
	// money, so getting it wrong would compare a daily amount against a whole-flight cap.
	switch {
	case row.CampaignBudget.Period != budgetPeriodDaily:
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget %s named %q has period %q, but this dispatch creates %s budgets; refusing to reuse a budget with a different pacing model", id, budgetName, row.CampaignBudget.Period, budgetPeriodDaily)
	case row.CampaignBudget.DeliveryMethod != budgetDeliveryStandard:
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget %s named %q has delivery method %q, but this dispatch creates %s budgets", id, budgetName, row.CampaignBudget.DeliveryMethod, budgetDeliveryStandard)
	case row.CampaignBudget.Type != budgetTypeStandard:
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget %s named %q has type %q, but this dispatch creates %s budgets", id, budgetName, row.CampaignBudget.Type, budgetTypeStandard)
	}

	// reference_count is the row's own answer to "is anything using this budget". It is
	// REQUIRED to be present and zero: an omitted count is not a zero count, and reading
	// it as one would grant the unattached verdict on a field the response never sent.
	refs, err := strconv.ParseInt(row.CampaignBudget.ReferenceCount, 10, 64)
	if err != nil {
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget %s named %q was returned with campaign_budget.reference_count %q, which is not an integer; refusing to reuse a budget whose attachment count is unreadable", id, budgetName, row.CampaignBudget.ReferenceCount)
	}
	if refs != 0 {
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget %s named %q reports %d campaign(s) using it; refusing to attach another campaign to a budget already being spent from", id, budgetName, refs)
	}

	// The amount is read for the caller to compare against the brief, and it must parse —
	// a budget whose amount cannot be read is one whose daily spend cannot be checked.
	amount, err := strconv.ParseInt(row.CampaignBudget.AmountMicros, 10, 64)
	if err != nil || amount <= 0 {
		return nil, false, fmt.Errorf("google-ads campaign budget lookup: budget %s named %q was returned with campaign_budget.amount_micros %q, which is not a positive integer", id, budgetName, row.CampaignBudget.AmountMicros)
	}

	return &reusableBudget{
		// Rebuilt, not echoed — see reusableBudget.ResourceName.
		ResourceName: "customers/" + c.account.CustomerID + "/campaignBudgets/" + id,
		ID:           id,
		AmountMicros: amount,
	}, true, nil
}

// assertBudgetUnattached asks the CAMPAIGN side whether anything references this budget,
// and returns an error if anything live does.
//
// This is the second of two independent answers to the same question — see the call site
// for why reference_count alone is not taken as settled. It is also the check that
// produces a readable failure instead of Google's: attaching a campaign to a non-shared
// budget that is already in use is refused upstream with a CampaignBudgetError the
// operator would receive as an opaque enum, after the campaign mutate had been fired.
func (c *Client) assertBudgetUnattached(ctx context.Context, budget *reusableBudget, budgetName string) error {
	// The resource name is interpolated rather than escaped because it is OURS: both
	// segments were validated as digits (customerIDRE on the account, canonicalBudgetID on
	// the id), so it carries no quote or backslash to neutralise. Nothing from the
	// response reaches this query.
	query := "SELECT campaign.id, campaign.name, campaign.status, campaign.resource_name, " +
		"campaign.campaign_budget FROM campaign " +
		"WHERE campaign.campaign_budget = '" + budget.ResourceName + "'"

	rows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return fmt.Errorf("google-ads campaign budget %s attachment check: %w", budget.ID, err)
	}

	for _, raw := range rows {
		if !utf8.Valid(raw) || hasUnpairedSurrogateEscape(raw) {
			return fmt.Errorf("google-ads campaign budget %s attachment check: a row cannot survive JSON decoding intact (malformed UTF-8 bytes, or an unpaired surrogate escape); refusing to call the budget unattached on a response that cannot be read", budget.ID)
		}
		if hasDuplicateKeys(raw) {
			return fmt.Errorf("google-ads campaign budget %s attachment check: a row declares the same JSON key twice and identifies more than one campaign", budget.ID)
		}
		var row budgetReferenceRow
		if err := json.Unmarshal(raw, &row); err != nil {
			// A row we cannot decode is NOT "no campaign uses this budget".
			return fmt.Errorf("google-ads campaign budget %s attachment check: decoding result row: %w", budget.ID, err)
		}
		// Re-check the filter client-side, same tripwire as the name filter above: a row
		// for a DIFFERENT budget means the predicate did not take effect, which
		// invalidates the whole response — including its silence about ours.
		if row.Campaign.CampaignBudget != budget.ResourceName {
			return fmt.Errorf("google-ads campaign budget %s attachment check: the query for %s returned a campaign whose budget is %q; the filter was not honoured, refusing to trust this response", budget.ID, budget.ResourceName, row.Campaign.CampaignBudget)
		}

		// Judged by the same identity-then-status rules the campaign lookup uses, so the
		// two paths cannot drift about what a trustworthy campaign row is.
		var ident campaignLookupRow
		ident.Campaign.ResourceName = row.Campaign.ResourceName
		ident.Campaign.ID = row.Campaign.ID
		ident.Campaign.Name = row.Campaign.Name
		ident.Campaign.Status = row.Campaign.Status
		id, live, err := c.campaignRowIdentity(ident, fmt.Sprintf("campaign using budget %s", budget.ID))
		if err != nil {
			return err
		}
		if !live {
			// A REMOVED campaign cannot serve and cannot spend, so its tombstone does not
			// make the budget in use.
			continue
		}
		return fmt.Errorf("google-ads campaign budget %s named %q is already used by campaign %s (%q); refusing to attach a second campaign to it", budget.ID, budgetName, id, row.Campaign.Name)
	}

	return nil
}

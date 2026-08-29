// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// HubSpot models marketing campaigns as the CRM object type `0-35`, which is why these calls go
// through /crm/v3/objects rather than the /marketing/v3 surface the email endpoints use. The
// object carries `hs_utm`: the campaign's UTM token, which is what makes a send attributable to
// a campaign in HubSpot's own reporting rather than only in ours.
const (
	campaignObjectType = "0-35"
	campaignSearchPath = "/crm/v3/objects/" + campaignObjectType + "/search"
	campaignCreatePath = "/crm/v3/objects/" + campaignObjectType

	// campaignSearchLimit bounds the search. HubSpot's CRM search caps `limit` at 200 (raised
	// from 100 in September 2024 — https://developers.hubspot.com/changelog/increasing-our-api-limits),
	// and this asks for the maximum rather than a smaller "human-readable" number.
	//
	// IT IS A CAP, NOT A PAGE. This method does not follow `paging.next.after`, so a campaign
	// ranked below the cap is not returned — and the caller reads an absent campaign as licence
	// to create one, in a namespace shared by everyone on that HubSpot portal. Every row the cap
	// omits is a duplicate this service can cause, which is why the ceiling is the API's own
	// maximum rather than a display-sized number: the results are not a page to read, they are
	// the evidence for an absence claim.
	//
	// `capped` reports whether the gap is actually open, so a caller is never left inferring
	// completeness from the row count.
	campaignSearchLimit = 200
)

// campaignProps are the properties requested on every campaign read.
//
// Requested EXPLICITLY rather than relying on defaults: the CRM search endpoint returns only
// `hs_object_id` and a few system fields unless properties are named, so a consumer promised a
// utm token would otherwise receive an empty string from every row.
var campaignProps = []string{"hs_name", "hs_utm", "hs_start_date"}

// Campaign is one HubSpot marketing campaign.
//
// THE NAMESPACE IS PORTAL-WIDE, NOT PROJECT-SCOPED. HubSpot campaigns are not scoped to a
// project or any sub-account this service partitions by: every campaign in the portal is
// visible to every caller holding that portal's token, and one created here is visible to
// everyone else working in it. That is a property of HubSpot's data model, not a gap in the
// scoping here, and it is why the create path is documented as requiring a UI warning.
//
// WHICH portal is the connection's, not necessarily the LF's. A HubSpot connection is stored
// per project and carries its own token and portal_id, and credsSource refuses the LF system
// fallback for HubSpot (internal/dispatch/creds.go — that fallback is ad-account-only, because
// writing one tenant's contacts into another's portal is not the trade it makes). So two
// projects share this namespace only when they are configured against the SAME portal, which
// is the common case for foundations under the LF umbrella but is not guaranteed. Do not read
// "portal-wide" as "every foundation, always".
type Campaign struct {
	// ID is HubSpot's own object id (`hs_object_id`).
	ID string
	// Name is the campaign's display name (`hs_name`).
	Name string
	// UTM is the campaign's UTM token (`hs_utm`). EMPTY is a real state: a campaign can exist
	// with no token configured, and that is different from the campaign not existing. A caller
	// must not treat an empty token as "not found" — see SearchCampaigns.
	UTM string
	// StartDate is `hs_start_date`, carried through as HubSpot's own string rather than parsed:
	// it is used for display and disambiguation between same-named campaigns, never for
	// arithmetic here.
	StartDate string
}

// campaignSearchHit is one row of the CRM search response.
type campaignSearchHit struct {
	ID         string `json:"id"`
	Properties struct {
		Name      string `json:"hs_name"`
		UTM       string `json:"hs_utm"`
		StartDate string `json:"hs_start_date"`
	} `json:"properties"`
}

// SearchCampaigns finds LF HubSpot campaigns whose name matches query.
//
// The match is HubSpot's own `query` search over the default searchable properties. It is NOT
// an exact-name lookup — it can return campaigns whose names merely share a token — and it is
// NOT relevance-ranked: the CRM v3 search API has no relevance sort, and with no `sorts` in the
// request the rows come back in HubSpot's default order, which is by object creation. Do not
// read the first row as the best match; nothing here has scored them.
//
// Every hit is returned rather than filtered to a single "best" one, because picking one would
// hide the ambiguity from the only party able to resolve it: a human looking at the names. A
// caller wanting an exact match, or a ranked one, must do that itself — the UI does exactly
// that, scoring locally before it offers a best match.
//
// An EMPTY result is not an error. "No campaign is named that" is the answer the caller acts on
// by offering to create one, and it must be distinguishable from a failed search — which is why
// a malformed 2xx is an error rather than an empty slice.
func (c *Client) SearchCampaigns(ctx context.Context, query string) (SearchCampaignsPage, error) {
	// Trimmed before sending, for the reason SearchEmails and SearchLists trim: a padded term
	// would otherwise fail to match names it should, returning a clean empty answer that reads
	// as "no such campaign".
	q := strings.TrimSpace(query)
	if q == "" {
		// Refused rather than sent. An empty query is not a search for everything — HubSpot
		// would return the whole portal's campaigns ranked arbitrarily, and a caller looking
		// for one event would act on whichever happened to sort first.
		return SearchCampaignsPage{}, fmt.Errorf("hubspot: campaign search requires a non-empty query")
	}

	body := map[string]any{
		"query":      q,
		"limit":      campaignSearchLimit,
		"properties": campaignProps,
	}
	raw, err := c.doRequest(ctx, http.MethodPost, campaignSearchPath, body, true)
	if err != nil {
		return SearchCampaignsPage{}, err
	}

	var resp struct {
		Results []campaignSearchHit `json:"results"`
		// Total is HubSpot's count of ALL matches, not just the returned page. It is what makes
		// "capped" a fact rather than an inference: len(results)==limit is also what an exactly-
		// full last page looks like, and guessing from it would warn on a complete result set.
		//
		// A POINTER, because an ABSENT total and a total of zero are different answers and a
		// plain int cannot tell them apart. Absent means the response did not say how many
		// matched — so capping is UNKNOWN, and the fail-closed reading is that it may have been
		// capped. Decoded as zero, an absent total would read as "0 matched, nothing hidden",
		// which is the authoritative absence a caller acts on by creating a duplicate campaign.
		Total *int `json:"total"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return SearchCampaignsPage{}, fmt.Errorf("hubspot: decode campaign search: %w", err)
	}
	// A malformed 2xx body (`{}` or `null`) decodes with Results==nil, while a genuinely empty
	// search returns `{"results":[]}` — non-nil. Erroring on nil keeps "the search failed"
	// distinguishable from "nothing matched", which is the distinction the caller branches on.
	if resp.Results == nil {
		return SearchCampaignsPage{}, fmt.Errorf("hubspot: campaign search returned a 2xx with no results array (malformed response)")
	}

	out := make([]Campaign, 0, len(resp.Results))
	for _, hit := range resp.Results {
		// An id-less hit is a MALFORMED response, not a droppable row. Dropping it would turn
		// `{"results":[{"id":""}]}` into a clean empty answer — and empty is precisely what the
		// caller acts on by creating a campaign, in a namespace shared by everyone on that HubSpot portal. That
		// is the same fail-closed rule the nil-results check above applies, and silently
		// discarding a row would breach it one layer down. Every other field may legitimately
		// be empty.
		if strings.TrimSpace(hit.ID) == "" {
			return SearchCampaignsPage{}, fmt.Errorf("hubspot: campaign search returned a hit with no id (malformed response)")
		}
		out = append(out, Campaign{
			ID:        hit.ID,
			Name:      hit.Properties.Name,
			UTM:       hit.Properties.UTM,
			StartDate: hit.Properties.StartDate,
		})
	}
	// Capped is derived from HubSpot's own total, not from len(out)==limit — an exactly-full
	// final page is indistinguishable from a truncated one by length alone.
	//
	// An ABSENT total is treated as capped: the response did not tell us how many matched, so
	// completeness is unknown, and "unknown" must not be reported as the proven absence that
	// licenses a create in a namespace shared by everyone on that HubSpot portal.
	// A CONTRADICTORY total is treated as capped too. A negative total, or one smaller than the
	// rows actually returned, means the response cannot be trusted to describe its own
	// completeness — and "cannot be trusted" must not resolve to the proven absence that
	// licenses a create. Only a total that is >= the returned count can say the set is whole.
	capped := resp.Total == nil || *resp.Total != len(out)
	return SearchCampaignsPage{Campaigns: out, Capped: capped}, nil
}

// SearchCampaignsPage is one page of campaign search results plus the fact a caller needs to
// know whether absence is meaningful.
type SearchCampaignsPage struct {
	// Campaigns are the matches, in the order HubSpot returned them — by object creation, NOT
	// by relevance. See SearchCampaigns.
	Campaigns []Campaign
	// Capped reports that this search could NOT be shown to be complete — HubSpot reported more
	// matches than it returned, OR completeness is simply unknown (an absent `total`, or one
	// that contradicts the rows). All of those fail CLOSED: a campaign absent from Campaigns may
	// still exist, so a caller must not read absence as licence to create one in a namespace
	// shared by everyone on that HubSpot portal.
	Capped bool
}

// CreateCampaign creates a portal-wide HubSpot campaign named name.
//
// IT IS VISIBLE PORTAL-WIDE. The campaign namespace is the whole HubSpot portal this client's
// credential authenticates against, so this is not a project-scoped write however the calling
// route is scoped — the created campaign appears for everyone working in that portal. Which
// portal that is depends on the connection (see Campaign), so this is not necessarily every
// foundation. Callers must warn before invoking it.
//
// It does NOT check for an existing campaign first. A search-then-create here would be a race
// with any concurrent caller and would still not prevent a duplicate, so the check belongs with
// the human who can read the candidate names: the caller searches, shows the matches, and only
// then creates. Making that ordering the caller's job is what keeps this method honest about
// what it does — it always creates.
//
// HubSpot assigns `hs_utm` itself on creation; it is not settable here. The created campaign is
// read back from the create response rather than re-fetched, so any token returned is the one
// HubSpot actually assigned rather than one this service guessed.
//
// The token may be ABSENT from a successful create, and that is not an error: property
// selection on a create is undocumented (see the request below), and HubSpot may not have
// assigned one yet. `UTM == ""` is a real state every caller already handles — what is refused
// is a response with no ID, which cannot be addressed at all.
func (c *Client) CreateCampaign(ctx context.Context, name string) (*Campaign, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return nil, fmt.Errorf("hubspot: campaign creation requires a non-empty name")
	}

	body := map[string]any{
		"properties": map[string]string{"hs_name": n},
	}
	// The properties are requested on the way back, as the search does. Without asking, the CRM
	// create returns only system fields, so the response would carry no `hs_utm` at all.
	//
	// BEST EFFORT, NOT A GUARANTEE. `?properties=` is documented on the READ endpoints; HubSpot
	// does not document it on the create, and their generated SDK builds this POST with no query
	// parameters. So it may be honoured or ignored, and this code must not depend on it — which
	// it does not: a create whose response carries no token returns `UTM == ""`, and an ABSENT
	// token is a real state every caller already handles (see Campaign.UTM). What the code does
	// depend on is the id, and an id-less or undecodable response is refused as UNCONFIRMED
	// rather than reported as success.
	//
	// The names are compile-time constants, never caller input.
	createPath := campaignCreatePath + "?properties=" + strings.Join(campaignProps, ",")
	// NOT idempotent: this creates a row, and a retried create makes a second campaign in a
	// namespace shared by everyone on that HubSpot portal. The transport must not replay it.
	raw, err := c.doRequest(ctx, http.MethodPost, createPath, body, false)
	if err != nil {
		return nil, err
	}

	var hit campaignSearchHit
	if err := json.Unmarshal(raw, &hit); err != nil {
		// UNCONFIRMED, not a plain decode error. The POST already returned 2xx, so HubSpot has
		// very likely created the campaign — only our reading of the body failed. A plain error
		// loses the structural signal (see CloneEmail and CreateList, which mark exactly this
		// arm) and a caller that treats it as a clean failure retries into a duplicate in a
		// namespace shared by everyone on that HubSpot portal.
		return nil, unconfirmed("hubspot: campaign create UNCONFIRMED (2xx with an undecodable body — a campaign may have been created; verify before retrying)", err)
	}
	// An id-less 2xx means the campaign may or may not have been created and cannot be
	// addressed either way. Reporting success would hand the caller a campaign reference that
	// does not work; reporting the ambiguity lets them check HubSpot rather than retry blindly
	// into a duplicate.
	if strings.TrimSpace(hit.ID) == "" {
		// Same reasoning as the decode arm above, and marked the same way: the message already
		// said the outcome was unknown, but only the STRUCTURAL signal survives being wrapped
		// by callers, and it is what a classifier reads.
		return nil, unconfirmed("hubspot: campaign create returned a 2xx with no id — the campaign may or may not exist, check HubSpot before retrying", nil)
	}
	return &Campaign{
		ID:        hit.ID,
		Name:      hit.Properties.Name,
		UTM:       hit.Properties.UTM,
		StartDate: hit.Properties.StartDate,
	}, nil
}

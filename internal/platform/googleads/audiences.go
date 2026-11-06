// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Strictly read-only. The one call in this file is a GAQL search (googleAds:search, POST but
// idempotent) and nothing here mutates anything.
//
// This file exists because the Google create's audience inputs (customAudiences and
// userInterests on Display, demandGenAudience, performanceMaxAudienceSignal) take resource
// names the caller had to copy out of the Google Ads UI (#496). Listing them lets a picker
// offer what the account actually has. User lists have their own read (ListUserLists).

// AudienceKind names one of the five Google Ads resources ListAudiences reads.
type AudienceKind string

// The kinds ListAudiences reads, spelled as the GAQL resource names they select FROM.
const (
	AudienceKindCustomAudience   AudienceKind = "custom_audience"
	AudienceKindCombinedAudience AudienceKind = "combined_audience"
	AudienceKindAudience         AudienceKind = "audience"
	AudienceKindUserInterest     AudienceKind = "user_interest"
	// AudienceKindCustomInterest is Google's legacy custom affinity / custom intent segment,
	// the resource Display's customAffinities input takes (#517).
	AudienceKindCustomInterest AudienceKind = "custom_interest"
)

// ParseAudienceKind returns the kind s names, exactly as spelled (no case folding: the
// values are an API contract, and a near miss is a caller bug worth refusing).
func ParseAudienceKind(s string) (AudienceKind, bool) {
	switch k := AudienceKind(s); k {
	case AudienceKindCustomAudience, AudienceKindCombinedAudience, AudienceKindAudience, AudienceKindUserInterest,
		AudienceKindCustomInterest:
		return k, true
	}
	return "", false
}

// MaxAudienceScanRows bounds what one ListAudiences reads. The service filters the answer by
// name and returns at most a picker's worth, so it needs the whole set; the bound only stops
// a runaway response. Over it, the read fails rather than silently filtering a partial set,
// because a name search over a partial set reports "no match" for an entry that exists.
const MaxAudienceScanRows = 10000

// audienceListQueries holds, per kind, the GAQL search (v23 field names). Each keeps only entries
// a create can use now:
//
//   - custom_audience, combined_audience, audience, custom_interest: status ENABLED (REMOVED
//     cannot be attached).
//   - custom_interest additionally type CUSTOM_AFFINITY. The LF customer holds 28
//     CUSTOM_AFFINITY and 29 CUSTOM_INTENT rows (read-only search, 2026-11-06), but the one
//     input this kind feeds, Display's customAffinities, sends a customAffinity criterion, and a
//     validateOnly probe that day (paused LFX Display ad group 210268047788) accepted a
//     CUSTOM_AFFINITY resource there and refused both CUSTOM_INTENT ones tried with
//     RESOURCE_NOT_FOUND. Offering them would fail the create after the budget.
//   - audience additionally scope CUSTOMER: an ASSET_GROUP-scoped Audience belongs to the one
//     asset group it was made for and cannot be reused by another ad group or asset group.
//   - user_interest has no status field, and is restricted to the AFFINITY and IN_MARKET
//     taxonomies, the two the Display userInterests input is for; VERTICAL_GEO,
//     MOBILE_APP_INSTALL_USER and NEW_SMART_PHONE_USER are not audience segments a picker
//     offers. user_interest.launched_to_all is NOT filtered on: a read-only search of the LF
//     customer on 2026-11-05 returned it false for all 1008 AFFINITY and IN_MARKET rows (190
//     and 818), so filtering on it would empty the list.
//
// ORDER BY id makes the LIMIT-bounded set deterministic; rows are sorted by name locally,
// case-insensitively, as ListUserLists sorts.
var audienceListQueries = map[AudienceKind]string{
	AudienceKindCustomAudience: "SELECT custom_audience.resource_name, custom_audience.id, custom_audience.name, " +
		"custom_audience.type, custom_audience.status, custom_audience.description FROM custom_audience " +
		"WHERE custom_audience.status = 'ENABLED' ORDER BY custom_audience.id LIMIT %d",
	AudienceKindCombinedAudience: "SELECT combined_audience.resource_name, combined_audience.id, combined_audience.name, " +
		"combined_audience.status, combined_audience.description FROM combined_audience " +
		"WHERE combined_audience.status = 'ENABLED' ORDER BY combined_audience.id LIMIT %d",
	AudienceKindAudience: "SELECT audience.resource_name, audience.id, audience.name, audience.status, " +
		"audience.description FROM audience WHERE audience.status = 'ENABLED' AND audience.scope = 'CUSTOMER' " +
		"ORDER BY audience.id LIMIT %d",
	AudienceKindUserInterest: "SELECT user_interest.resource_name, user_interest.user_interest_id, user_interest.name, " +
		"user_interest.taxonomy_type FROM user_interest WHERE user_interest.taxonomy_type IN ('AFFINITY', 'IN_MARKET') " +
		"ORDER BY user_interest.user_interest_id LIMIT %d",
	AudienceKindCustomInterest: "SELECT custom_interest.resource_name, custom_interest.id, custom_interest.name, " +
		"custom_interest.type, custom_interest.status, custom_interest.description FROM custom_interest " +
		"WHERE custom_interest.status = 'ENABLED' AND custom_interest.type = 'CUSTOM_AFFINITY' " +
		"ORDER BY custom_interest.id LIMIT %d",
}

// audienceCollections is each kind's resource collection in a resource name.
var audienceCollections = map[AudienceKind]string{
	AudienceKindCustomAudience:   "customAudiences",
	AudienceKindCombinedAudience: "combinedAudiences",
	AudienceKindAudience:         "audiences",
	AudienceKindUserInterest:     "userInterests",
	AudienceKindCustomInterest:   "customInterests",
}

// Audience is one entry ListAudiences returns.
type Audience struct {
	// ResourceName is customers/{customer}/{collection}/{id}, the form the create inputs take.
	ResourceName string
	ID           string
	Name         string
	// Subtype is Google's own classification, verbatim: custom_audience.type (AUTO, INTEREST,
	// PURCHASE_INTENT, SEARCH), custom_interest.type (always CUSTOM_AFFINITY: see the query) or
	// user_interest.taxonomy_type (AFFINITY, IN_MARKET). Empty
	// for combined_audience and audience, which have none.
	Subtype string
	// Status is ENABLED for the four account resources (the query filters on it) and empty
	// for user_interest, which has no status field.
	Status      string
	Description string
}

// audienceGaqlRow decodes any of the five kinds; each response row carries exactly one of the
// five objects, keyed by the resource's camelCase name.
type audienceGaqlRow struct {
	CustomAudience   *audienceGaqlFields `json:"customAudience"`
	CombinedAudience *audienceGaqlFields `json:"combinedAudience"`
	Audience         *audienceGaqlFields `json:"audience"`
	UserInterest     *audienceGaqlFields `json:"userInterest"`
	CustomInterest   *audienceGaqlFields `json:"customInterest"`
}

type audienceGaqlFields struct {
	ResourceName string `json:"resourceName"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	TaxonomyType string `json:"taxonomyType"`
	Status       string `json:"status"`
	Description  string `json:"description"`
}

func (r audienceGaqlRow) fields(kind AudienceKind) *audienceGaqlFields {
	switch kind {
	case AudienceKindCustomAudience:
		return r.CustomAudience
	case AudienceKindCombinedAudience:
		return r.CombinedAudience
	case AudienceKindAudience:
		return r.Audience
	case AudienceKindUserInterest:
		return r.UserInterest
	case AudienceKindCustomInterest:
		return r.CustomInterest
	}
	return nil
}

// ListAudiences reads every usable entry of one kind on this client's customer, sorted by name
// (case-insensitively, then by resource name).
//
// A row whose resource name is not a {collection} resource with a positive canonical id under
// this customer is dropped rather than offered, as ListUserLists does: the create would refuse
// it. More than MaxAudienceScanRows rows fails the read (see MaxAudienceScanRows).
func (c *Client) ListAudiences(ctx context.Context, kind AudienceKind) ([]Audience, error) {
	query, ok := audienceListQueries[kind]
	if !ok {
		return nil, fmt.Errorf("google-ads: unknown audience kind %q", kind)
	}
	customerID := c.account.CustomerID
	raw, err := c.gaqlSearch(ctx, fmt.Sprintf(query, MaxAudienceScanRows+1))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	if len(raw) > MaxAudienceScanRows {
		return nil, fmt.Errorf("list %s: account %s returned more than the %d rows this read scans", kind, customerID, MaxAudienceScanRows)
	}
	prefix := "customers/" + customerID + "/" + audienceCollections[kind] + "/"
	out := make([]Audience, 0, len(raw))
	for i, r := range raw {
		var row audienceGaqlRow
		if uErr := json.Unmarshal(r, &row); uErr != nil {
			return nil, &transportError{Method: http.MethodPost, Path: c.customerPath("googleAds:search"), Err: fmt.Errorf("decode %s row at index %d: %w", kind, i, uErr)}
		}
		f := row.fields(kind)
		if f == nil {
			continue
		}
		rn := strings.TrimSpace(f.ResourceName)
		id := canonicalCampaignID(strings.TrimPrefix(rn, prefix))
		if !strings.HasPrefix(rn, prefix) || id == "" {
			continue
		}
		a := Audience{ResourceName: rn, ID: id, Name: f.Name, Status: f.Status, Description: f.Description}
		switch kind {
		case AudienceKindCustomAudience, AudienceKindCustomInterest:
			a.Subtype = f.Type
		case AudienceKindUserInterest:
			a.Subtype = f.TaxonomyType
			a.Status = ""
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].ResourceName < out[j].ResourceName
	})
	return out, nil
}

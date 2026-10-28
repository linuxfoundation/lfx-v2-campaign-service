// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Targeting lookups: the two read-only picker reads behind googleAdsConfig.geoTargets and
// googleAdsConfig.audienceSegments. Both exist so an operator chooses an id from a list rather
// than typing one; neither creates, changes or deletes anything.

// GeoTargetSuggestion is one geo target constant Google suggested for a search.
type GeoTargetSuggestion struct {
	// ID is the numeric geo target constant id, in the canonical base-10 spelling
	// resolveGeoList accepts for googleAdsConfig.geoTargets. NOT the
	// "geoTargetConstants/{id}" resource name: the config validator refuses that form.
	ID            string
	Name          string
	CanonicalName string
	CountryCode   string
	// TargetType is Google's own label, verbatim (e.g. "City", "Postal Code").
	TargetType string
	// Status is ENABLED or REMOVAL_PLANNED, verbatim.
	Status string
	// Reach is Google's approximate audience estimate, nil when Google sent none.
	Reach *int64
}

// geoSuggestRequest is the body of geoTargetConstants:suggest.
type geoSuggestRequest struct {
	Locale        string            `json:"locale,omitempty"`
	CountryCode   string            `json:"countryCode,omitempty"`
	LocationNames geoSuggestNameSet `json:"locationNames"`
}

type geoSuggestNameSet struct {
	Names []string `json:"names"`
}

type geoSuggestResponse struct {
	Suggestions []struct {
		GeoTargetConstant *struct {
			ResourceName  string `json:"resourceName"`
			ID            string `json:"id"`
			Name          string `json:"name"`
			CountryCode   string `json:"countryCode"`
			TargetType    string `json:"targetType"`
			Status        string `json:"status"`
			CanonicalName string `json:"canonicalName"`
		} `json:"geoTargetConstant"`
		Reach string `json:"reach"`
	} `json:"geoTargetConstantSuggestions"`
}

// SuggestGeoTargetConstants asks Google's GeoTargetConstantService which geo target constants
// match query (a place name or a postal code), optionally within one country.
//
// Account-AGNOSTIC: the endpoint has no customer segment, so like ListAccessibleCustomers it
// runs without a configured CustomerID and validates only the login-customer-id it sends as a
// header. The answer is Google's public location table, not anything an ad account holds.
//
// Rows are returned in Google's order. A row whose id is not a canonical positive integer (the
// only form googleAdsConfig.geoTargets accepts) is dropped rather than offered: a picker that
// lists it would hand the caller an id the campaign create then refuses.
func (c *Client) SuggestGeoTargetConstants(ctx context.Context, query, countryCode string) ([]GeoTargetSuggestion, error) {
	if err := c.validateLoginCustomerID(); err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("google-ads: geo target search query must not be empty")
	}
	body := geoSuggestRequest{
		Locale:        "en",
		CountryCode:   strings.ToUpper(strings.TrimSpace(countryCode)),
		LocationNames: geoSuggestNameSet{Names: []string{query}},
	}
	// A suggest is a read (POST-but-idempotent, like GAQL :search), so a 429 is safe to retry.
	raw, err := c.doRequestValidated(ctx, http.MethodPost, "geoTargetConstants:suggest", body, true /* idempotent */)
	if err != nil {
		return nil, fmt.Errorf("suggest geo target constants: %w", err)
	}
	var resp geoSuggestResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &transportError{Method: http.MethodPost, Path: "geoTargetConstants:suggest", Err: fmt.Errorf("decode suggest response: %w", err)}
	}
	out := make([]GeoTargetSuggestion, 0, len(resp.Suggestions))
	seen := map[string]struct{}{}
	for _, s := range resp.Suggestions {
		g := s.GeoTargetConstant
		if g == nil {
			continue
		}
		id := strings.TrimSpace(g.ID)
		if id == "" {
			id = strings.TrimPrefix(strings.TrimSpace(g.ResourceName), "geoTargetConstants/")
		}
		if canonicalCampaignID(id) == "" {
			continue
		}
		// The same location can come back once per matched search term; one row per id.
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		var reach *int64
		if v, perr := strconv.ParseInt(strings.TrimSpace(s.Reach), 10, 64); perr == nil && v >= 0 {
			reach = &v
		}
		out = append(out, GeoTargetSuggestion{
			ID:            id,
			Name:          g.Name,
			CanonicalName: g.CanonicalName,
			CountryCode:   g.CountryCode,
			TargetType:    g.TargetType,
			Status:        g.Status,
			Reach:         reach,
		})
	}
	return out, nil
}

// NormalizeGeoTargetType folds a target type for comparison, so a caller's "postal_code",
// "POSTAL_CODE" or "Postal Code" all match Google's "Postal Code" label.
func NormalizeGeoTargetType(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r == ' ' || r == '_' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// UserList is one Google Ads user list (audience) on a customer.
type UserList struct {
	// ResourceName is "customers/{customerId}/userLists/{id}", exactly the form
	// googleAdsConfig.audienceSegments accepts.
	ResourceName string
	ID           string
	Name         string
	// Type is Google's UserListType, verbatim (e.g. CRM_BASED, RULE_BASED, LOGICAL).
	Type string
	// MembershipStatus is OPEN or CLOSED, verbatim.
	MembershipStatus string
	// AccountStatus is the list's account_user_list_status (ENABLED or DISABLED), verbatim.
	AccountStatus string
	// SizeForSearch is Google's estimated Search-network size, nil when Google sent none.
	SizeForSearch *int64
	// SizeRangeForSearch is Google's bucketed Search-network size, verbatim.
	SizeRangeForSearch string
	// EligibleForSearch reports whether the list can target Search campaigns. An absent
	// field decodes as false: protobuf JSON omits a false bool.
	EligibleForSearch bool
}

type userListGaqlRow struct {
	UserList struct {
		ResourceName          string `json:"resourceName"`
		ID                    string `json:"id"`
		Name                  string `json:"name"`
		Type                  string `json:"type"`
		MembershipStatus      string `json:"membershipStatus"`
		AccountUserListStatus string `json:"accountUserListStatus"`
		SizeForSearch         string `json:"sizeForSearch"`
		SizeRangeForSearch    string `json:"sizeRangeForSearch"`
		EligibleForSearch     bool   `json:"eligibleForSearch"`
	} `json:"userList"`
}

// MaxUserListRows is the most user lists a picker is offered. A caller asks ListUserLists for
// one more than it will show, so it can tell a full page from a truncated one.
const MaxUserListRows = 500

// userListQuery selects user lists on the customer, bounded by LIMIT so a large account is
// not downloaded whole for a page that shows MaxUserListRows. ORDER BY user_list.id makes the
// bounded subset deterministic; the rows are then sorted by name locally.
const userListQuery = "SELECT user_list.resource_name, user_list.id, user_list.name, user_list.type, " +
	"user_list.membership_status, user_list.account_user_list_status, user_list.size_for_search, " +
	"user_list.size_range_for_search, user_list.eligible_for_search FROM user_list " +
	"ORDER BY user_list.id LIMIT %d"

// ListUserLists reads at most limit user lists on customerID (lowest ids first), sorted by
// name (then resource name). limit must be positive.
//
// customerID is validated by gaqlSearchForCustomer (digits only) because it is interpolated
// into the request path. A row whose resource name is not a userLists resource under
// customerID is dropped rather than offered: audienceSegments would refuse it, and a row
// claiming another customer means the response cannot be trusted for this one.
func (c *Client) ListUserLists(ctx context.Context, customerID string, limit int) ([]UserList, error) {
	if limit < 1 {
		return nil, fmt.Errorf("google-ads: user list limit must be positive, got %d", limit)
	}
	raw, err := c.gaqlSearchForCustomer(ctx, customerID, fmt.Sprintf(userListQuery, limit))
	if err != nil {
		return nil, fmt.Errorf("list user lists: %w", err)
	}
	prefix := "customers/" + customerID + "/userLists/"
	out := make([]UserList, 0, len(raw))
	for _, r := range raw {
		var row userListGaqlRow
		if err := json.Unmarshal(r, &row); err != nil {
			return nil, &transportError{Method: http.MethodPost, Path: "customers/" + customerID + "/googleAds:search", Err: fmt.Errorf("decode user list row: %w", err)}
		}
		u := row.UserList
		rn := strings.TrimSpace(u.ResourceName)
		if !strings.HasPrefix(rn, prefix) || canonicalCampaignID(strings.TrimPrefix(rn, prefix)) == "" {
			continue
		}
		var size *int64
		if v, perr := strconv.ParseInt(strings.TrimSpace(u.SizeForSearch), 10, 64); perr == nil && v >= 0 {
			size = &v
		}
		out = append(out, UserList{
			ResourceName:       rn,
			ID:                 strings.TrimPrefix(rn, prefix),
			Name:               u.Name,
			Type:               u.Type,
			MembershipStatus:   u.MembershipStatus,
			AccountStatus:      u.AccountUserListStatus,
			SizeForSearch:      size,
			SizeRangeForSearch: u.SizeRangeForSearch,
			EligibleForSearch:  u.EligibleForSearch,
		})
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

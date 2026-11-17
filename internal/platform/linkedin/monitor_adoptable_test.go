// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"testing"
)

// TestListAccountCampaignRefs_RefusesUntrustworthyCandidates pins the adoption candidate list's
// own checks (#581): the monitor's shared reader tolerates missing fields and repeated keys, but
// a candidate the caller can pick must have a valid id, a name and an ACTIVE/PAUSED status, no id
// may repeat, and the page must pass identityjson. Each violation is ErrUntrustworthyCampaignList,
// which the service answers as 503.
func TestListAccountCampaignRefs_RefusesUntrustworthyCandidates(t *testing.T) {
	cases := map[string]string{
		"empty element":     `{"elements":[{}],"metadata":{}}`,
		"missing id":        `{"elements":[{"name":"Spring","status":"ACTIVE"}],"metadata":{}}`,
		"zero id":           `{"elements":[{"id":0,"name":"Spring","status":"ACTIVE"}],"metadata":{}}`,
		"non-numeric id":    `{"elements":[{"id":"abc","name":"Spring","status":"ACTIVE"}],"metadata":{}}`,
		"urn id":            `{"elements":[{"id":"urn:li:sponsoredCampaign:111","name":"Spring","status":"ACTIVE"}],"metadata":{}}`,
		"empty name":        `{"elements":[{"id":111,"name":"","status":"ACTIVE"}],"metadata":{}}`,
		"blank name":        `{"elements":[{"id":111,"name":"   ","status":"PAUSED"}],"metadata":{}}`,
		"missing status":    `{"elements":[{"id":111,"name":"Spring"}],"metadata":{}}`,
		"unknown status":    `{"elements":[{"id":111,"name":"Spring","status":"WHATEVER"}],"metadata":{}}`,
		"archived status":   `{"elements":[{"id":111,"name":"Spring","status":"ARCHIVED"}],"metadata":{}}`,
		"lower-case status": `{"elements":[{"id":111,"name":"Spring","status":"active"}],"metadata":{}}`,
		"duplicate id":      `{"elements":[{"id":111,"name":"Spring","status":"ACTIVE"},{"id":"111","name":"Spring copy","status":"PAUSED"}],"metadata":{}}`,
		"duplicate key":     `{"elements":[{"id":111,"name":"Spring","status":"ACTIVE","status":"PAUSED"}],"metadata":{}}`,
		"case-folded key":   `{"elements":[{"id":111,"name":"Spring","Name":"Other","status":"ACTIVE"}],"metadata":{}}`,
		"duplicate top key": `{"elements":[{"id":111,"name":"Spring","status":"ACTIVE"}],"elements":[{"id":222,"name":"X","status":"ACTIVE"}],"metadata":{}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := adAccountsServer(t, body)
			rows, err := newAccountsClient(t, srv.URL).ListAccountCampaignRefs(context.Background(), "512345678")
			if !errors.Is(err, ErrUntrustworthyCampaignList) {
				t.Fatalf("err = %v, rows = %+v; want ErrUntrustworthyCampaignList", err, rows)
			}
			if rows != nil {
				t.Errorf("rows = %+v, want none alongside the refusal", rows)
			}
		})
	}
}

// TestListAccountCampaignRefs_DuplicateAcrossPages_IsRefused: the id check spans the whole walk,
// not one page.
func TestListAccountCampaignRefs_DuplicateAcrossPages_IsRefused(t *testing.T) {
	srv, _ := adAccountsServer(t,
		`{"elements":[{"id":111,"name":"Spring","status":"ACTIVE"}],"metadata":{"nextPageToken":"p2"}}`,
		`{"elements":[{"id":111,"name":"Spring","status":"ACTIVE"}],"metadata":{}}`,
	)
	_, err := newAccountsClient(t, srv.URL).ListAccountCampaignRefs(context.Background(), "512345678")
	if !errors.Is(err, ErrUntrustworthyCampaignList) {
		t.Fatalf("err = %v, want ErrUntrustworthyCampaignList", err)
	}
}

// TestListAccountCampaignRefs_ValidPage_IsReturned: a well-formed page passes unchanged, and an
// empty one is an honest empty list.
func TestListAccountCampaignRefs_ValidPage_IsReturned(t *testing.T) {
	srv, _ := adAccountsServer(t,
		`{"elements":[{"id":111,"name":"Spring","status":"ACTIVE"},{"id":"222","name":"Fall","status":"PAUSED","dailyBudget":{"amount":"10","currencyCode":"USD"}}],"metadata":{"nextPageToken":"p2"}}`,
		`{"elements":[],"metadata":{}}`,
	)
	rows, err := newAccountsClient(t, srv.URL).ListAccountCampaignRefs(context.Background(), "512345678")
	if err != nil {
		t.Fatalf("ListAccountCampaignRefs: %v", err)
	}
	if len(rows) != 2 || rows[0].CampaignID != "111" || rows[0].Name != "Spring" || rows[0].Status != "ACTIVE" ||
		rows[1].CampaignID != "222" || rows[1].Name != "Fall" || rows[1].Status != "PAUSED" {
		t.Fatalf("rows = %+v, want 111/Spring/ACTIVE and 222/Fall/PAUSED", rows)
	}

	srv, _ = adAccountsServer(t, `{"elements":[],"metadata":{}}`)
	rows, err = newAccountsClient(t, srv.URL).ListAccountCampaignRefs(context.Background(), "512345678")
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty page: rows = %+v, err = %v; want an empty list", rows, err)
	}
}

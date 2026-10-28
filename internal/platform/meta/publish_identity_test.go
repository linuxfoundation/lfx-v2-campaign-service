// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func TestCheckPublishIdentity(t *testing.T) {
	cases := []struct {
		name string
		in   PublishIdentity
		want string // "" = accepted
	}{
		{name: "US only, nothing supplied", in: PublishIdentity{GeoTargets: []string{"US"}}},
		{name: "empty geo defaults to US", in: PublishIdentity{}},
		{name: "UK and Switzerland need no DSA", in: PublishIdentity{GeoTargets: []string{"GB", "CH", "US"}}},
		{name: "non-EU EEA without disclosures", in: PublishIdentity{GeoTargets: []string{"GB", "NO", "IS", "LI"}}, want: "EU countries (NO, IS, LI)"},
		{name: "EU with both disclosures", in: PublishIdentity{GeoTargets: []string{"US", "DE"}, DSABeneficiary: "CNCF", DSAPayor: "The Linux Foundation"}},
		{name: "EU without disclosures", in: PublishIdentity{GeoTargets: []string{"US", "de", "FR"}}, want: "EU DSA disclosures are required: geoTargets include EU countries (DE, FR)"},
		{name: "EU outermost region without disclosures", in: PublishIdentity{GeoTargets: []string{"RE"}}, want: "EU countries (RE)"},
		{name: "EU with whitespace-only disclosures", in: PublishIdentity{GeoTargets: []string{"IE"}, DSABeneficiary: "  ", DSAPayor: "\t"}, want: "EU DSA disclosures are required"},
		{name: "dropped bogus code is not a reason", in: PublishIdentity{GeoTargets: []string{"US", "XX"}}},
		{name: "one-sided beneficiary", in: PublishIdentity{GeoTargets: []string{"US"}, DSABeneficiary: "CNCF"}, want: "dsaBeneficiary was supplied without dsaPayor"},
		{name: "one-sided payor", in: PublishIdentity{GeoTargets: []string{"DE"}, DSAPayor: "LF"}, want: "dsaPayor was supplied without dsaBeneficiary"},
		{name: "control character", in: PublishIdentity{DSABeneficiary: "CNCF\nEvil", DSAPayor: "LF"}, want: "dsaBeneficiary contains a control"},
		{name: "line separator", in: PublishIdentity{DSABeneficiary: "CNCF\u2028Evil", DSAPayor: "LF"}, want: "dsaBeneficiary contains a control"},
		{name: "paragraph separator", in: PublishIdentity{DSABeneficiary: "CNCF", DSAPayor: "L\u2029F"}, want: "dsaPayor contains a control"},
		{name: "bidi override", in: PublishIdentity{DSABeneficiary: "\u202eFNCC", DSAPayor: "LF"}, want: "dsaBeneficiary contains a control"},
		{name: "bidi isolate", in: PublishIdentity{DSABeneficiary: "CNCF", DSAPayor: "L\u2067F"}, want: "dsaPayor contains a control"},
		{name: "non-Latin name is fine", in: PublishIdentity{DSABeneficiary: "Fondation Linux — Zürich", DSAPayor: "リナックス財団"}},
		{name: "too long", in: PublishIdentity{DSABeneficiary: "LF", DSAPayor: strings.Repeat("a", maxDSANameRunes+1)}, want: "dsaPayor is 257 characters"},
		{name: "numeric instagram id", in: PublishIdentity{InstagramUserID: " 17841400000000000 "}},
		{name: "malformed instagram id", in: PublishIdentity{InstagramUserID: "@lfprojects"}, want: "instagramUserId"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			err := CheckPublishIdentity(&in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("CheckPublishIdentity = %v, want nil", err)
				}
				return
			}
			var pr *domain.PreflightRefusal
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !errors.As(err, &pr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckPublishIdentity = %v, want an ErrCreateConfigInvalid PreflightRefusal containing %q", err, tc.want)
			}
		})
	}
}

func TestCheckPublishIdentityOneSidedAdviceFollowsTheGeoTargets(t *testing.T) {
	// "Omit both" alone is only an option when no EU country is targeted: with one, the next
	// guard refuses a create carrying neither disclosure, and removing the EU countries alone
	// still leaves a one-sided pair. So with an EU target the advice is "supply both, or remove
	// the EU countries and omit both disclosures" (#429). Both directions of the pair are covered,
	// so a refactor that names the wrong side on the EU branch is caught.
	for _, tc := range []struct {
		name          string
		id            PublishIdentity
		supplied      string
		want, notWant string
	}{
		{"EU targeted, beneficiary only", PublishIdentity{GeoTargets: []string{"US", "DE"}, DSABeneficiary: "CNCF"},
			"dsaBeneficiary was supplied without dsaPayor", "geoTargets include EU countries (DE), so Meta requires both", "supply both or omit both"},
		{"EU targeted, payor only", PublishIdentity{GeoTargets: []string{"FR"}, DSAPayor: "LF"},
			"dsaPayor was supplied without dsaBeneficiary", "remove the EU countries and omit both disclosures", "supply both or omit both"},
		{"no EU targeted", PublishIdentity{GeoTargets: []string{"US", "GB"}, DSABeneficiary: "CNCF"},
			"dsaBeneficiary was supplied without dsaPayor", "supply both or omit both", "EU countries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.id
			err := CheckPublishIdentity(&id)
			if err == nil || !strings.Contains(err.Error(), tc.supplied) {
				t.Fatalf("CheckPublishIdentity = %v, want the one-sided refusal %q", err, tc.supplied)
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("CheckPublishIdentity = %v, want it to contain %q and not %q", err, tc.want, tc.notWant)
			}
		})
	}
}

func TestCheckPublishIdentityTrimsInPlace(t *testing.T) {
	in := PublishIdentity{InstagramUserID: " 123 ", DSABeneficiary: " CNCF ", DSAPayor: " LF "}
	if err := CheckPublishIdentity(&in); err != nil {
		t.Fatal(err)
	}
	if in.InstagramUserID != "123" || in.DSABeneficiary != "CNCF" || in.DSAPayor != "LF" {
		t.Fatalf("not trimmed: %+v", in)
	}
}

func TestUsesInstagram(t *testing.T) {
	f, tr := false, true
	cases := []struct {
		name string
		p    Placement
		want bool
	}{
		{"defaults include Instagram Feed", Placement{}, true},
		{"Facebook only", Placement{InstagramFeed: &f}, false},
		{"Stories run on Instagram", Placement{InstagramFeed: &f, Stories: &tr}, true},
		{"Reels run on Instagram", Placement{InstagramFeed: &f, Reels: &tr}, true},
		{"Facebook only, Stories and Reels explicitly off", Placement{FacebookFeed: &tr, InstagramFeed: &f, Stories: &f, Reels: &f}, false},
		{"Audience Network only", Placement{FacebookFeed: &f, InstagramFeed: &f, AudienceNetwork: &tr}, false},
	}
	for _, tc := range cases {
		if got := UsesInstagram(tc.p); got != tc.want {
			t.Errorf("%s: UsesInstagram = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCreateCampaignBindsInstagramOnlyForAnInstagramPlacement: the IGSID is bound to the creative
// only when the ad set runs on Instagram, so a Facebook-only ad set's creative is unchanged.
func TestCreateCampaignBindsInstagramOnlyForAnInstagramPlacement(t *testing.T) {
	f := false
	for name, tc := range map[string]struct {
		placements Placement
		wantSent   bool
	}{
		"default placements (Instagram Feed on)": {Placement{}, true},
		"Facebook only":                          {Placement{InstagramFeed: &f}, false},
	} {
		t.Run(name, func(t *testing.T) {
			adsetCap, creativeCap := newBodyCapture(), newBodyCapture()
			srv := metaFieldServer(t, adsetCap, creativeCap)
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "tok"},
				AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
				WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
			in := metaFieldInput()
			in.InstagramUserID = "17841400000000000"
			in.Placements = tc.placements
			if _, err := c.CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign error: %v", err)
			}
			_, sent := creativeCap.get()["instagram_user_id"]
			if sent != tc.wantSent {
				t.Fatalf("instagram_user_id sent = %v, want %v", sent, tc.wantSent)
			}
		})
	}
}

// TestCreateCampaignRefusesEUWithoutDSABeforeAnyRequest: the client's own backstop, for a path
// that skipped the dispatcher's synchronous check — no Meta request is made.
func TestCreateCampaignRefusesEUWithoutDSABeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: the EU/DSA refusal must precede every call", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "tok"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	in := metaFieldInput()
	in.GeoTargets = []string{"US", "NL"}
	_, err := c.CreateCampaign(context.Background(), in)
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), "EU countries (NL)") {
		t.Fatalf("CreateCampaign = %v, want the EU DSA refusal", err)
	}
}

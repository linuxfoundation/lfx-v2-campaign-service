// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const (
	adListKey = "GET /888/ads"
)

func adRow(id, adSet, campaign, account, extra string) string {
	return fmt.Sprintf(`{"id":%q,"adset_id":%q,"campaign_id":%q,"account_id":%q%s}`, id, adSet, campaign, account, extra)
}

func adInsRow(ad, adSet, campaign, extra string) string {
	return fmt.Sprintf(`{"ad_id":%q,"adset_id":%q,"campaign_id":%q,"account_currency":"USD"%s}`, ad, adSet, campaign, extra)
}

func happyAdReplies() map[string][]adSetReply {
	return map[string][]adSetReply{
		adListKey: {
			{200, page("c1", adRow("901", "888", "555", "777", `,"name":"Image A","status":"ACTIVE","effective_status":"ACTIVE","creative":{"id":"701","name":"Creative A"}`))},
			{200, page("", adRow("902", "888", "555", "act_777", `,"name":"Image B","status":"PAUSED","effective_status":"PAUSED"`))},
		},
		adSetAcctKey: ok(`{"currency":"USD","id":"act_777"}`),
		adSetInsKey: ok(page("",
			adInsRow("901", "888", "555", `,"impressions":"1000","clicks":"25","spend":"12.34"`),
			adInsRow("903", "888", "555", `,"impressions":"10","clicks":"1","spend":"0.50"`),
		)),
	}
}

func TestListAdSetAds_HappyPathPagesAndMerges(t *testing.T) {
	s := newAdSetServer(t, happyAdReplies())
	got, err := s.client().ListAdSetAds(context.Background(), "888", "555", "act_777", WindowLast7Days)
	if err != nil {
		t.Fatalf("ListAdSetAds: %v", err)
	}
	if got.AdSetID != "888" || got.CampaignID != "555" || got.Currency != "USD" || got.Window != WindowLast7Days || got.Truncated {
		t.Fatalf("header = %+v", got)
	}
	if len(got.Ads) != 3 {
		t.Fatalf("ads = %+v", got.Ads)
	}
	a, b, c := got.Ads[0], got.Ads[1], got.Ads[2]
	if a.ID != "901" || !a.Listed || *a.Name != "Image A" || *a.CreativeID != "701" || *a.CreativeName != "Creative A" ||
		a.Impressions != 1000 || a.Clicks != 25 || a.CostMicros != 12_340_000 || a.Ctr != 0.025 {
		t.Errorf("901 = %+v", a)
	}
	if b.ID != "902" || !b.Listed || b.CreativeID != nil || b.Impressions != 0 || b.Ctr != 0 {
		t.Errorf("902 = %+v", b)
	}
	if c.ID != "903" || c.Listed || c.Name != nil || c.Impressions != 10 || c.CostMicros != 500_000 {
		t.Errorf("903 = %+v", c)
	}
	calls := s.calls()
	if len(calls) != 4 {
		t.Fatalf("want 2 listing pages, the currency and one insights page, got %+v", calls)
	}
	for _, c := range calls {
		if c.method != http.MethodGet {
			t.Fatalf("a read issued %s %s", c.method, c.uri)
		}
		if strings.Contains(c.uri, "SHOULD-NOT-BE-FOLLOWED") || strings.Contains(c.uri, "access_token") {
			t.Fatalf("the paging.next URL (and its token) was followed: %s", c.uri)
		}
	}
	first, _ := url.QueryUnescape(calls[0].uri)
	if !strings.Contains(first, "fields="+adListFields) || !strings.Contains(calls[1].uri, "after=c1") {
		t.Errorf("listing requests = %q / %q", calls[0].uri, calls[1].uri)
	}
	ins := calls[3].uri
	for _, want := range []string{"level=ad&", "date_preset=last_7d", "fields=" + adInsightsFields,
		"filtering=%5B%7B%22field%22%3A%22adset.id%22%2C%22operator%22%3A%22EQUAL%22%2C%22value%22%3A%22888%22%7D%5D"} {
		if !strings.Contains(ins, want) {
			t.Errorf("insights request %q lacks %q", ins, want)
		}
	}
}

// Past the listing cap the read answers what it has, flagged truncated — never a silent partial.
func TestListAdSetAds_ListingCapSetsTruncated(t *testing.T) {
	r := happyAdReplies()
	var pages []adSetReply
	for i := 0; i < adListMaxPages+5; i++ {
		pages = append(pages, adSetReply{200, page(fmt.Sprintf("c%d", i), adRow(fmt.Sprintf("%d", 1000+i), "888", "555", "777", ""))})
	}
	r[adListKey] = pages
	r[adSetInsKey] = ok(page(""))
	s := newAdSetServer(t, r)
	got, err := s.client().ListAdSetAds(context.Background(), "888", "555", "act_777", "")
	if err != nil {
		t.Fatalf("ListAdSetAds: %v", err)
	}
	if !got.Truncated || len(got.Ads) != adListMaxPages {
		t.Fatalf("truncated = %v, ads = %d; want true and %d", got.Truncated, len(got.Ads), adListMaxPages)
	}
	n := 0
	for _, c := range s.calls() {
		if strings.HasPrefix(c.uri, "/888/ads") {
			n++
		}
	}
	if n != adListMaxPages {
		t.Fatalf("sent %d listing requests, want exactly %d", n, adListMaxPages)
	}
	// Exactly at the cap with no further page is NOT truncated.
	r = happyAdReplies()
	pages = nil
	for i := 0; i < adListMaxPages; i++ {
		next := fmt.Sprintf("c%d", i)
		if i == adListMaxPages-1 {
			next = ""
		}
		pages = append(pages, adSetReply{200, page(next, adRow(fmt.Sprintf("%d", 1000+i), "888", "555", "777", ""))})
	}
	r[adListKey] = pages
	s = newAdSetServer(t, r)
	got, err = s.client().ListAdSetAds(context.Background(), "888", "555", "act_777", "")
	if err != nil || got.Truncated {
		t.Fatalf("got %+v, %v; want a complete, untruncated read", got, err)
	}
}

// Delivery is never truncated: an Insights walk past its bound fails the whole read.
func TestListAdSetAds_InsightsPagingIsBounded(t *testing.T) {
	r := happyAdReplies()
	var pages []adSetReply
	for i := 0; i < adInsightsMaxPages+5; i++ {
		pages = append(pages, adSetReply{200, page(fmt.Sprintf("i%d", i), adInsRow(fmt.Sprintf("%d", 2000+i), "888", "555", ""))})
	}
	r[adSetInsKey] = pages
	s := newAdSetServer(t, r)
	got, err := s.client().ListAdSetAds(context.Background(), "888", "555", "act_777", "")
	if err == nil || got != nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("got %+v, %v; want the page bound", got, err)
	}
}

func TestListAdSetAds_RefusesUntrustworthyAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		body string
	}{
		{"repeated cursor", adListKey, page("c1", adRow("901", "888", "555", "777", ""))},
		{"next without cursor", adListKey, `{"data":[],"paging":{"next":"https://x"}}`},
		{"no data field", adListKey, `{"paging":{}}`},
		{"duplicated key on a listing page", adListKey, `{"data":[{"id":"901","adset_id":"889","adset_id":"888","campaign_id":"555","account_id":"777"}],"paging":{}}`},
		{"case-folded duplicate key", adListKey, `{"data":[{"id":"901","Adset_ID":"889","adset_id":"888","campaign_id":"555","account_id":"777"}],"paging":{}}`},
		{"duplicated key inside the creative", adListKey, page("", adRow("901", "888", "555", "777", `,"creative":{"id":"701","id":"702"}`))},
		{"malformed UTF-8", adListKey, "{\"data\":[{\"id\":\"901\",\"name\":\"a\xffb\",\"adset_id\":\"888\",\"campaign_id\":\"555\",\"account_id\":\"777\"}],\"paging\":{}}"},
		{"unpaired surrogate", adListKey, `{"data":[{"id":"901","name":"\ud800","adset_id":"888","campaign_id":"555","account_id":"777"}],"paging":{}}`},
		{"an ad of another ad set", adListKey, page("", adRow("901", "889", "555", "777", ""))},
		{"an ad with no ad set", adListKey, page("", `{"id":"901","campaign_id":"555","account_id":"777"}`)},
		{"an ad of another campaign", adListKey, page("", adRow("901", "888", "556", "777", ""))},
		{"an ad listed twice", adListKey, page("", adRow("901", "888", "555", "777", ""), adRow("901", "888", "555", "777", ""))},
		{"a non-canonical ad id", adListKey, page("", adRow("0901", "888", "555", "777", ""))},
		{"a non-canonical creative id", adListKey, page("", adRow("901", "888", "555", "777", `,"creative":{"id":"x7"}`))},
		{"a status outside the charset", adListKey, page("", adRow("901", "888", "555", "777", `,"status":"<b>"`))},
		{"no account currency", adSetAcctKey, `{"id":"act_777"}`},
		{"duplicated key on an insights page", adSetInsKey, `{"data":[{"ad_id":"901","adset_id":"888","campaign_id":"555","account_currency":"USD","spend":"1","spend":"2"}],"paging":{}}`},
		{"an insights row of another ad set", adSetInsKey, page("", adInsRow("901", "889", "555", ""))},
		{"an insights row of another campaign", adSetInsKey, page("", adInsRow("901", "888", "556", ""))},
		{"a duplicated insights row", adSetInsKey, page("", adInsRow("901", "888", "555", ""), adInsRow("901", "888", "555", ""))},
		{"a different currency", adSetInsKey, page("", `{"ad_id":"901","adset_id":"888","campaign_id":"555","account_currency":"EUR"}`)},
		{"an explicit null impressions", adSetInsKey, page("", adInsRow("901", "888", "555", `,"impressions":null`))},
		{"a numeric (non-string) clicks", adSetInsKey, page("", adInsRow("901", "888", "555", `,"clicks":5`))},
		{"a malformed spend", adSetInsKey, page("", adInsRow("901", "888", "555", `,"spend":"lots"`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := happyAdReplies()
			r[tc.key] = ok(tc.body)
			if tc.key == adListKey {
				r[tc.key] = []adSetReply{{200, tc.body}, {200, tc.body}}
			}
			s := newAdSetServer(t, r)
			got, err := s.client().ListAdSetAds(context.Background(), "888", "555", "act_777", "")
			if err == nil || got != nil {
				t.Fatalf("got %+v, %v; want a refusal", got, err)
			}
			if errors.Is(err, ErrAdAccountMismatch) {
				t.Fatalf("an untrustworthy answer was classified as an account mismatch: %v", err)
			}
			for _, leak := range []string{"<b>", "lots", "\xff"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error echoes upstream content %q: %v", leak, err)
				}
			}
		})
	}
}

func TestListAdSetAds_AnotherAccountIsAMismatch(t *testing.T) {
	r := happyAdReplies()
	r[adListKey] = ok(page("", adRow("901", "888", "555", "999", "")))
	s := newAdSetServer(t, r)
	_, err := s.client().ListAdSetAds(context.Background(), "888", "555", "act_777", "")
	if !errors.Is(err, ErrAdAccountMismatch) {
		t.Fatalf("err = %v, want ErrAdAccountMismatch", err)
	}
}

func TestListAdSetAds_ObjectMissingIsAnError(t *testing.T) {
	for _, status := range []int{400, 403, 404} {
		r := happyAdReplies()
		r[adListKey] = []adSetReply{{status, `{"error":{"message":"Unsupported get request","type":"GraphMethodException","code":100,"error_subcode":33}}`}}
		s := newAdSetServer(t, r)
		got, err := s.client().ListAdSetAds(context.Background(), "888", "555", "act_777", "")
		var ae *APIError
		if got != nil || !errors.As(err, &ae) || ae.Code != 100 {
			t.Fatalf("status %d: got %+v, %v; want the Graph error", status, got, err)
		}
	}
}

func TestListAdSetAds_RefusesBadInputBeforeAnyRequest(t *testing.T) {
	s := newAdSetServer(t, happyAdReplies())
	c := s.client()
	for _, args := range [][4]string{
		{"88/8", "555", "act_777", ""}, {"888", "0555", "act_777", ""}, {"888", "555", "777", ""}, {"888", "555", "act_777", "LAST_90_DAYS"},
	} {
		if _, err := c.ListAdSetAds(context.Background(), args[0], args[1], args[2], MetricsWindow(args[3])); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if n := len(s.calls()); n != 0 {
		t.Fatalf("sent %d requests for refused input", n)
	}
}

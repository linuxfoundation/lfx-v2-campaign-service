// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

// Response bodies follow Reddit's published v3 OpenAPI document (GetCustomAudienceResponse:
// {"data":[components_schema_custom_audience_base...],"pagination":{...}}). They prove the walk's
// handling of that shape, not that Reddit sends it; custom_audiences.go records that.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

const customAudiencesPath = "/api/v3/ad_accounts/t2_acct/custom_audiences"

func newCustomAudienceClient(t *testing.T, stub *discoveryStub) *Client {
	t.Helper()
	apiSrv := httptest.NewServer(stub)
	t.Cleanup(apiSrv.Close)
	stub.mu.Lock()
	stub.srvURL = apiSrv.URL
	stub.mu.Unlock()
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	t.Cleanup(tokenSrv.Close)
	return NewClient(testCreds, AccountConfig{AccountID: "t2_acct"},
		WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()), withRetryBaseDelay(tinyBackoff))
}

func TestListCustomAudiences_WalksPagesAndMapsFields(t *testing.T) {
	stub := &discoveryStub{respond: func(s *discoveryStub, key string, _ int) (int, string) {
		switch key {
		case customAudiencesPath:
			return 200, `{"data":[{"id":"67547345732543","name":" Site visitors ","type":"PIXEL_RETARGETING","status":"VALID","size_range_lower":1000,"size_range_upper":5000}],"pagination":` + s.next(customAudiencesPath+"?page.token=p2") + `}`
		case customAudiencesPath + "?page.token=p2":
			return 200, `{"data":[{"id":"t2_list","name":"Registrants","type":"CUSTOMER_LIST","status":"PENDING"}],"pagination":{"next_url":null}}`
		}
		return 404, `{}`
	}}
	c := newCustomAudienceClient(t, stub)
	got, truncated, err := c.ListCustomAudiences(context.Background())
	if err != nil || truncated {
		t.Fatalf("ListCustomAudiences: truncated=%v err=%v", truncated, err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d audiences, want 2: %+v", len(got), got)
	}
	if a := got[0]; a.ID != "67547345732543" || a.Name != "Site visitors" || a.Type != "PIXEL_RETARGETING" || a.Status != "VALID" ||
		a.SizeLower == nil || *a.SizeLower != 1000 || a.SizeUpper == nil || *a.SizeUpper != 5000 {
		t.Errorf("first = %+v", a)
	}
	if b := got[1]; b.ID != "t2_list" || b.SizeLower != nil || b.SizeUpper != nil {
		t.Errorf("second = %+v (absent sizes must stay nil)", b)
	}
	if n := len(stub.seen()); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestListCustomAudiences_EmptyAccountIsEmptyNotNil(t *testing.T) {
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) {
		return 200, `{"data":[],"pagination":{}}`
	}}
	got, truncated, err := newCustomAudienceClient(t, stub).ListCustomAudiences(context.Background())
	if err != nil || truncated || got == nil || len(got) != 0 {
		t.Fatalf("got (%v, %v, %v), want an empty non-nil list", got, truncated, err)
	}
}

// TestListCustomAudiences_StopsAtTheCapAndSaysSo: an account with more pages than the cap is
// read for exactly customAudienceMaxPages pages and reported truncated, never as complete.
func TestListCustomAudiences_StopsAtTheCapAndSaysSo(t *testing.T) {
	stub := &discoveryStub{respond: func(s *discoveryStub, _ string, n int) (int, string) {
		return 200, fmt.Sprintf(`{"data":[{"id":"a%d"}],"pagination":%s}`, n, s.next(fmt.Sprintf("%s?page.token=p%d", customAudiencesPath, n+1)))
	}}
	got, truncated, err := newCustomAudienceClient(t, stub).ListCustomAudiences(context.Background())
	if err != nil || !truncated {
		t.Fatalf("truncated=%v err=%v, want truncated", truncated, err)
	}
	if len(got) != customAudienceMaxPages || len(stub.seen()) != customAudienceMaxPages {
		t.Errorf("got %d audiences over %d requests, want %d of each", len(got), len(stub.seen()), customAudienceMaxPages)
	}
}

func TestListCustomAudiences_RefusesWhatItCannotBelieve(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"data absent", `{"pagination":{}}`},
		{"data null", `{"data":null}`},
		{"data not a list", `{"data":{"id":"1"}}`},
		{"blank id", `{"data":[{"id":""}]}`},
		{"path in id", `{"data":[{"id":"1/../2"}]}`},
		{"duplicate", `{"data":[{"id":"1"},{"id":"1"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) { return 200, tc.body }}
			got, _, err := newCustomAudienceClient(t, stub).ListCustomAudiences(context.Background())
			if got != nil || !errors.Is(err, ErrCustomAudiencesMalformed) {
				t.Fatalf("got (%v, %v), want nil and ErrCustomAudiencesMalformed", got, err)
			}
		})
	}
}

func TestListCustomAudiences_UpstreamErrorIsAnError(t *testing.T) {
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) { return http.StatusForbidden, `{}` }}
	got, truncated, err := newCustomAudienceClient(t, stub).ListCustomAudiences(context.Background())
	if got != nil || truncated || err == nil {
		t.Fatalf("got (%v, %v, %v), want an error and nothing partial", got, truncated, err)
	}
}

func TestListCustomAudiences_RefusesAnUnusableAccountWithoutCalling(t *testing.T) {
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) { return 200, `{"data":[]}` }}
	c := newCustomAudienceClient(t, stub)
	c.account.AccountID = "t2/../x"
	if _, _, err := c.ListCustomAudiences(context.Background()); err == nil {
		t.Fatal("an unusable account id must be refused")
	}
	if n := len(stub.seen()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

// TestCreateCampaign_RefusesBadCustomAudiencesBeforeAnyRequest: the client's own check, for a
// caller that skipped the dispatcher's, refuses before the account verify GET, so nothing is
// created and the job reports the sentence.
func TestCreateCampaign_RefusesBadCustomAudiencesBeforeAnyRequest(t *testing.T) {
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) { return 200, `{"data":{"id":"x"}}` }}
	c := newCustomAudienceClient(t, stub)
	c.account.ConversionPixelID = "t2_pixel"
	in := validRedditInput()
	in.CustomAudienceIDs = []string{"1"}
	in.ExcludedCustomAudienceIDs = []string{"1"}
	res, err := c.CreateCampaign(context.Background(), in)
	var refusal *domain.PreflightRefusal
	if res != nil || !errors.As(err, &refusal) || !strings.Contains(err.Error(), "also in customAudienceIds") {
		t.Fatalf("CreateCampaign = (%v, %v), want a pre-flight refusal", res, err)
	}
	if n := len(stub.seen()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

func TestValidateCustomAudienceIDs(t *testing.T) {
	many := make([]string, MaxCustomAudienceIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf("a%d", i)
	}
	for _, tc := range []struct {
		name             string
		include, exclude []string
		want             string // substring; "" = accepted
	}{
		{"none", nil, nil, ""},
		{"valid", []string{"67547345732543", "t2_x"}, []string{"9"}, ""},
		{"at the cap", many[:MaxCustomAudienceIDs], nil, ""},
		{"over the cap", many, nil, "customAudienceIds holds 101 ids"},
		{"excluded over the cap", nil, many, "excludedCustomAudienceIds holds 101 ids"},
		{"blank", []string{"1", ""}, nil, "customAudienceIds entry 2 is not"},
		{"padded", nil, []string{" 1"}, "excludedCustomAudienceIds entry 1 is not"},
		{"too long", []string{strings.Repeat("1", 65)}, nil, "entry 1 is not"},
		{"64 is fine", []string{strings.Repeat("1", 64)}, nil, ""},
		{"duplicate", []string{"1", "2", "1"}, nil, "customAudienceIds entry 3 repeats"},
		{"overlap", []string{"1", "2"}, []string{"3", "2"}, "excludedCustomAudienceIds entry 2 is also in customAudienceIds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateCustomAudienceIDs(tc.include, tc.exclude)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("ValidateCustomAudienceIDs = %q, want %q", got, tc.want)
			}
		})
	}
}

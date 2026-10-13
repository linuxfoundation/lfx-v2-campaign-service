// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// leverStub is a fake LinkedIn answering the campaign read, the confirming account read and the
// unitCost PARTIAL_UPDATE, recording every request (method, path, headers, body) under a mutex.
type leverStub struct {
	mu           sync.Mutex
	reqs         []leverReq
	campaignCode int
	campaignBody string
	accountCode  int
	accountBody  string
	writeCode    int
	writeBody    string
}

type leverReq struct {
	Method, Path, Body, RestliMethod string
}

func (s *leverStub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, leverReq{Method: r.Method, Path: r.URL.Path, Body: string(body), RestliMethod: r.Header.Get("X-Restli-Method")})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost:
		w.WriteHeader(s.writeCode)
		_, _ = io.WriteString(w, s.writeBody)
	case r.URL.Path == "/adAccounts/509430019":
		w.WriteHeader(s.accountCode)
		_, _ = io.WriteString(w, s.accountBody)
	default:
		w.WriteHeader(s.campaignCode)
		_, _ = io.WriteString(w, s.campaignBody)
	}
}

func (s *leverStub) requests() []leverReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]leverReq(nil), s.reqs...)
}

func newLeverStub(t *testing.T) (*leverStub, *Client) {
	t.Helper()
	s := &leverStub{campaignCode: 200, accountCode: 200, writeCode: 204, accountBody: `{"id":509430019}`}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return s, NewClient(Credentials{AccessToken: "test-token"}, RuntimeConfig{DefaultAccountID: "509430019"},
		WithBaseURL(srv.URL), withRetryBaseDelay(time.Millisecond))
}

const fullCampaignJSON = `{"id":777,"account":"urn:li:sponsoredAccount:509430019",
 "campaignGroup":"urn:li:sponsoredCampaignGroup:42","name":"KubeCon EU","status":"PAUSED",
 "objectiveType":"WEBSITE_CONVERSION","optimizationTargetType":"NONE","costType":"CPC",
 "unitCost":{"amount":"5.25","currencyCode":"USD"},
 "dailyBudget":{"amount":"100.00","currencyCode":"USD"},
 "runSchedule":{"start":1785542400000,"end":1786924799999}}`

func TestValidateCampaignID(t *testing.T) {
	for id, ok := range map[string]bool{
		"777": true, "1": true, "9223372036854775807": true, // MaxInt64 accepted
		"": false, "0": false, "0777": false, "-1": false, "12a": false, " 1": false, "1 ": false,
		"9223372036854775808": false, "9999999999999999999": false, "12345678901234567890": false, "urn:li:sponsoredCampaign:1": false,
	} {
		if got := ValidateCampaignID(id) == nil; got != ok {
			t.Errorf("ValidateCampaignID(%q) accepted=%v, want %v", id, got, ok)
		}
		if err := ValidateCampaignID(id); err != nil && !errors.Is(err, ErrInvalidCampaignID) {
			t.Errorf("ValidateCampaignID(%q) = %v, does not wrap ErrInvalidCampaignID", id, err)
		}
	}
}

func TestParseDecimalMicros_IsExactOrAbsent(t *testing.T) {
	cases := []struct {
		raw     string
		want    int64
		present bool
		bad     bool
	}{
		{`"5.25"`, 5_250_000, true, false},
		{`"30.0"`, 30_000_000, true, false},
		{`"0"`, 0, true, false},
		{`"10.123456"`, 10_123_456, true, false},
		{`"10.1234560"`, 10_123_456, true, false},
		{`12.5`, 12_500_000, true, false},
		{`"10.1234567"`, 0, false, true}, // beyond micro precision: refused, never rounded
		{`"-1"`, 0, false, true},
		{`"9223372036854.775807"`, 9_223_372_036_854_775_807, true, false}, // the int64 ceiling, exactly
		{`"9223372036854.775808"`, 0, false, true},                         // one micro past it
		{`"9999999999999.999999"`, 0, false, true},                         // would wrap negative
		{`"9999999999999.5"`, 0, false, true},
		{`"1e3"`, 0, false, true},
		{`" 5"`, 0, false, true},
		{`true`, 0, false, true},
		{`null`, 0, false, false},
		{``, 0, false, false},
	}
	for _, tc := range cases {
		got, bad := parseDecimalMicros(json.RawMessage(tc.raw))
		if bad != tc.bad || (got != nil) != tc.present || (got != nil && *got != tc.want) {
			t.Errorf("parseDecimalMicros(%s) = %v,%v; want present=%v %d bad=%v", tc.raw, deref(got), bad, tc.present, tc.want, tc.bad)
		}
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestGetCampaignSettings_ReadsEveryReportedField(t *testing.T) {
	s, c := newLeverStub(t)
	s.campaignBody = fullCampaignJSON
	got, err := c.GetCampaignSettings(context.Background(), "777")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got.CampaignID != "777" || got.AccountID != "509430019" || got.CampaignGroupID != "42" {
		t.Errorf("ids = %+v", got)
	}
	if *got.Name != "KubeCon EU" || *got.Status != "PAUSED" || *got.CostType != "CPC" ||
		*got.OptimizationTargetType != "NONE" || *got.ObjectiveType != "WEBSITE_CONVERSION" {
		t.Errorf("strings = %+v", got)
	}
	if *got.UnitCostMicros != 5_250_000 || got.UnitCostCurrency != "USD" || *got.DailyBudgetMicros != 100_000_000 ||
		got.TotalBudgetMicros != nil || got.BudgetCurrency != "USD" {
		t.Errorf("amounts = %+v", got)
	}
	if got.RunStart.Format(time.RFC3339) != "2026-08-01T00:00:00Z" || got.RunEnd.Format("2006-01-02") != "2026-08-16" {
		t.Errorf("schedule = %v .. %v", got.RunStart, got.RunEnd)
	}
	reqs := s.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/adAccounts/509430019/adCampaigns/777" {
		t.Fatalf("requests = %+v, want one account-scoped GET", reqs)
	}
	if reqs[0].RestliMethod != "" {
		t.Errorf("a single-entity GET must not claim a Rest.li method, sent %q", reqs[0].RestliMethod)
	}
}

func TestGetCampaignSettings_AbsenceIsProvenOnlyByAReadableAccount(t *testing.T) {
	cases := []struct {
		name        string
		accountCode int
		accountBody string
		wantAbsent  bool
	}{
		{"account confirmed", 200, `{"id":509430019}`, true},
		{"account also 404", 404, `{}`, false},
		{"account 403", 403, `{}`, false},
		{"account names another id", 200, `{"id":1}`, false},
		{"account body duplicated key", 200, `{"id":509430019,"id":1}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newLeverStub(t)
			s.campaignCode, s.campaignBody = 404, `{"status":404}`
			s.accountCode, s.accountBody = tc.accountCode, tc.accountBody
			got, err := c.GetCampaignSettings(context.Background(), "777")
			if tc.wantAbsent {
				if err != nil || got != nil {
					t.Fatalf("want (nil, nil), got %+v, %v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("an unproven absence must be an error, got %+v", got)
			}
		})
	}
}

func TestGetCampaignSettings_UntrustworthyBodiesAreErrors(t *testing.T) {
	for name, body := range map[string]string{
		"duplicated id":          `{"id":777,"id":778,"name":"x"}`,
		"another campaign":       `{"id":778,"name":"x"}`,
		"no id":                  `{"name":"x"}`,
		"not an object":          `[1,2]`,
		"unknown status":         `{"id":777,"status":"SOMETHING_NEW"}`,
		"account is not a URN":   `{"id":777,"account":"509430019"}`,
		"group is not a URN":     `{"id":777,"campaignGroup":"urn:li:sponsoredAccount:1"}`,
		"wrong kind for a field": `{"id":777,"name":7}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, c := newLeverStub(t)
			s.campaignBody = body
			if got, err := c.GetCampaignSettings(context.Background(), "777"); err == nil {
				t.Fatalf("want an error, got %+v", got)
			}
		})
	}
}

func TestGetCampaignSettings_InvalidIDIsRefusedBeforeAnyRequest(t *testing.T) {
	s, c := newLeverStub(t)
	if _, err := c.GetCampaignSettings(context.Background(), "../777"); !errors.Is(err, ErrInvalidCampaignID) {
		t.Fatalf("err = %v, want ErrInvalidCampaignID", err)
	}
	if n := len(s.requests()); n != 0 {
		t.Fatalf("%d request(s) sent for an invalid id", n)
	}
}

func TestCampaignSettings_Terminal(t *testing.T) {
	for status, want := range map[string]bool{"ACTIVE": false, "PAUSED": false, "DRAFT": false, "ARCHIVED": false,
		"COMPLETED": false, "CANCELED": true, "PENDING_DELETION": true, "REMOVED": true} {
		st := status
		if got := (&CampaignSettings{Status: &st}).Terminal(); got != want {
			t.Errorf("Terminal(%s) = %v, want %v", status, got, want)
		}
	}
}

func TestAdoptedCampaignResult_RecordsProvenanceOnly(t *testing.T) {
	raw, err := json.Marshal(AdoptedCampaignResult("509430019", "777", "KubeCon", "Campaign adopted: 777"))
	if err != nil {
		t.Fatal(err)
	}
	var blob map[string]any
	_ = json.Unmarshal(raw, &blob)
	if blob["accountId"] != "509430019" || blob["adopted"] != true || blob["campaignGroupId"] != "" {
		t.Errorf("blob = %s", raw)
	}
	if !strings.Contains(blob["linkedInUrl"].(string), "/accounts/509430019/campaigns/777") {
		t.Errorf("linkedInUrl = %v", blob["linkedInUrl"])
	}
}

// A budget object that is PRESENT without an amount is unreadable, never absent: read as absent, a
// campaign reporting both budgets would pass as an unambiguous single-budget one.
func TestGetCampaignSettings_PresentBudgetWithoutAmountIsUnreadable(t *testing.T) {
	for name, total := range map[string]string{
		"null amount":    `{"amount":null,"currencyCode":"USD"}`,
		"missing amount": `{"currencyCode":"USD"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, c := newLeverStub(t)
			s.campaignBody = `{"id":777,"dailyBudget":{"amount":"5.00","currencyCode":"USD"},"totalBudget":` + total + `}`
			got, err := c.GetCampaignSettings(context.Background(), "777")
			if err != nil {
				t.Fatalf("GetCampaignSettings: %v", err)
			}
			if !got.BudgetUnparseable {
				t.Errorf("a present totalBudget without an amount must mark the budget unreadable: %+v", got)
			}
		})
	}
}

// ValidAccountID is the campaign id's rule: a positive int64 without a leading zero.
func TestValidAccountID_IsAPositiveLong(t *testing.T) {
	for id, ok := range map[string]bool{
		"509430019": true, "9223372036854775807": true,
		"0": false, "0509": false, "": false, "9223372036854775808": false, "-1": false, "12a": false,
	} {
		if got := ValidAccountID(id); got != ok {
			t.Errorf("ValidAccountID(%q) = %v, want %v", id, got, ok)
		}
	}
}

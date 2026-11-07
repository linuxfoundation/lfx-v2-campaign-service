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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func TestValidateConversions(t *testing.T) {
	many := make([]string, MaxConversions+1)
	for i := range many {
		many[i] = strconv.Itoa(i + 1)
	}
	for _, tc := range []struct {
		name string
		in   []string
		want string // "" accepted
	}{
		{"absent", nil, ""},
		{"ids", []string{"104012", "107620"}, ""},
		{"empty list", []string{}, "omit conversions"},
		{"urn form", []string{"urn:lla:llaPartnerConversion:104012"}, "is a URN"},
		{"not numeric", []string{"abc"}, "not a conversion rule id"},
		{"leading zero", []string{"0104"}, "not a conversion rule id"},
		{"zero", []string{"0"}, "not a conversion rule id"},
		{"padded", []string{" 104"}, "not a conversion rule id"},
		{"past int64", []string{"9223372036854775808"}, "not a conversion rule id"},
		{"too many", many, "at most 20"},
		{"duplicates fit the cap", append(append([]string{}, many[:MaxConversions]...), "1"), ""},
		{"over twice the cap, duplicates or not", func() []string {
			out := make([]string, 2*MaxConversions+1)
			for i := range out {
				out[i] = "1"
			}
			return out
		}(), "at most 20"},
		{"invalid entry past the raw cap is not reached", append(make([]string, 2*MaxConversions+1), "x"), "at most 20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateConversions(tc.in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrConversionsInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal containing %q, got %v", tc.want, err)
			}
		})
	}
	out, _ := ValidateConversions([]string{"7", "7", "8"})
	if strings.Join(out, ",") != "7,8" {
		t.Errorf("duplicates kept: %v", out)
	}
}

type conversionServer struct {
	mu       sync.Mutex
	puts     []string // escaped paths
	bodies   []map[string]any
	failConv string // conversion id whose PUT answers failStatus
	status   int
	order    []string
}

func (s *conversionServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "campaignConversions"):
			if r.Method != http.MethodPut {
				t.Errorf("campaignConversions %s, want PUT", r.Method)
			}
			s.order = append(s.order, "conversion")
			s.puts = append(s.puts, r.URL.EscapedPath())
			var b map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &b)
			s.bodies = append(s.bodies, b)
			if s.failConv != "" && strings.HasSuffix(r.URL.EscapedPath(), "%3A"+s.failConv+")") {
				w.WriteHeader(s.status)
				_, _ = io.WriteString(w, `{"status":400,"message":"secret upstream text"}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			s.order = append(s.order, "campaign")
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			s.order = append(s.order, "post")
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}
}

// snapshot copies what the handler recorded, under the handler's lock.
func (s *conversionServer) snapshot() (puts []string, bodies []map[string]any, order []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.puts...), append([]map[string]any(nil), s.bodies...), append([]string(nil), s.order...)
}

func conversionInput(ids ...string) CampaignInput {
	return CampaignInput{
		EventName: "KubeCon", RegistrationURL: "https://events.example.org/kubecon",
		BudgetUSD: 100, StartDate: "2099-01-01", EndDate: "2099-02-01",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		TargetingProfile: "cloud-native",
		Variants:         []CreativeVariant{{IntroText: "Join us — it's great", Headline: "KubeCon 2099"}},
		Project:          "CNCF",
		Conversions:      ids,
	}
}

// TestCreateCampaign_AssociatesConversions pins the Campaign Conversions PUT: one per distinct id,
// on the Rest.li compound key with both URNs percent-encoded, a body naming both URNs, after the
// campaign and before the first dark post, with the split recorded on the result.
func TestCreateCampaign_AssociatesConversions(t *testing.T) {
	s := &conversionServer{}
	srv := httptest.NewServer(s.handler(t))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	res, err := c.CreateCampaign(context.Background(), conversionInput("104012", "107620", "104012"))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	puts, bodies, order := s.snapshot()
	want := []string{
		"/campaignConversions/(campaign:urn%3Ali%3AsponsoredCampaign%3A200,conversion:urn%3Alla%3AllaPartnerConversion%3A104012)",
		"/campaignConversions/(campaign:urn%3Ali%3AsponsoredCampaign%3A200,conversion:urn%3Alla%3AllaPartnerConversion%3A107620)",
	}
	if strings.Join(puts, "\n") != strings.Join(want, "\n") {
		t.Errorf("PUT paths:\n got %q\nwant %q", puts, want)
	}
	if len(bodies) != 2 || bodies[1]["campaign"] != "urn:li:sponsoredCampaign:200" || bodies[1]["conversion"] != "urn:lla:llaPartnerConversion:107620" {
		t.Errorf("PUT bodies = %v", bodies)
	}
	if got := strings.Join(order, ","); got != "campaign,conversion,conversion,post" {
		t.Errorf("order = %s, want the associations after the campaign and before the dark post", got)
	}
	if strings.Join(res.ConversionsAssociated, ",") != "104012,107620" || len(res.ConversionsFailed) != 0 {
		t.Errorf("result split = %v / %v", res.ConversionsAssociated, res.ConversionsFailed)
	}
}

// TestCreateCampaign_FailedConversionDoesNotStopTheCreate pins that a refused association is
// recorded (failed list, a step naming the id and status, no upstream text) and the creatives are
// still created.
func TestCreateCampaign_FailedConversionDoesNotStopTheCreate(t *testing.T) {
	s := &conversionServer{failConv: "107620", status: http.StatusBadRequest}
	srv := httptest.NewServer(s.handler(t))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	res, err := c.CreateCampaign(context.Background(), conversionInput("104012", "107620"))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if strings.Join(res.ConversionsAssociated, ",") != "104012" || strings.Join(res.ConversionsFailed, ",") != "107620" {
		t.Errorf("result split = %v / %v", res.ConversionsAssociated, res.ConversionsFailed)
	}
	if res.CreativeCount != 1 {
		t.Errorf("creatives = %d, want the create to go on", res.CreativeCount)
	}
	steps := strings.Join(res.Steps, "\n")
	if !strings.Contains(steps, "Conversion 107620 association with campaign 200 FAILED (LinkedIn answered 400)") {
		t.Errorf("steps do not name the failure: %s", steps)
	}
	if strings.Contains(steps, "secret upstream text") {
		t.Error("upstream text reached the steps")
	}
}

func TestCreateCampaign_InvalidConversionsRefusedBeforeAnyRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), conversionInput("urn:lla:llaPartnerConversion:1"))
	if _, pre := domain.AsPreflightRefusal(err); !errors.Is(err, ErrConversionsInvalid) || !pre {
		t.Fatalf("want a pre-flight ErrConversionsInvalid, got %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d requests before the refusal", n)
	}
}

func TestCreateCampaign_NoConversionsSendsNoAssociation(t *testing.T) {
	s := &conversionServer{}
	srv := httptest.NewServer(s.handler(t))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), conversionInput())
	if err != nil {
		t.Fatal(err)
	}
	puts, _, _ := s.snapshot()
	if len(puts) != 0 || res.ConversionsAssociated != nil || res.ConversionsFailed != nil {
		t.Errorf("associations without conversions: %v %+v", puts, res)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "conversions") {
		t.Errorf("result blob carries empty conversion fields: %s", b)
	}
}

// TestCreateCampaign_ConversionPUTRetriesA429 pins that the association PUT, idempotent on its
// key, is retried after a 429 like a GET rather than reported failed.
func TestCreateCampaign_ConversionPUTRetriesA429(t *testing.T) {
	var throttled atomic.Bool
	s := &conversionServer{}
	inner := s.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "campaignConversions") && throttled.CompareAndSwap(false, true) {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		inner(w, r)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	res, err := c.CreateCampaign(context.Background(), conversionInput("104012"))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if strings.Join(res.ConversionsAssociated, ",") != "104012" || len(res.ConversionsFailed) != 0 {
		t.Errorf("a throttled PUT was not retried: %v / %v", res.ConversionsAssociated, res.ConversionsFailed)
	}
}

// TestCreateCampaign_FailedConversionOnAReusedCampaignSaysItMayBeServing pins the step for a
// campaign reused by name: it keeps its own status, so "before activating" would be wrong.
func TestCreateCampaign_FailedConversionOnAReusedCampaignSaysItMayBeServing(t *testing.T) {
	s := &conversionServer{failConv: "104012", status: http.StatusBadRequest}
	inner := s.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaigns") && !strings.Contains(r.URL.Path, "adCampaignGroups") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[{"id":777,"name":"`+reusedCampaignName(t)+`","status":"ACTIVE","campaignGroup":"urn:li:sponsoredCampaignGroup:100","objectiveType":"WEBSITE_VISIT"}],"metadata":{}}`)
			return
		}
		inner(w, r)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), conversionInput("104012"))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CampaignID != "777" {
		t.Fatalf("campaign was not reused: %s (steps %v)", res.CampaignID, res.Steps)
	}
	steps := strings.Join(res.Steps, "\n")
	if !strings.Contains(steps, "this campaign was reused and may already be serving without it") || strings.Contains(steps, "before activating") {
		t.Errorf("reused-campaign step is wrong: %s", steps)
	}
}

// reusedCampaignName is the name CreateCampaign composes for conversionInput under automatic
// bidding, so a search can answer with an existing campaign of that name.
func reusedCampaignName(t *testing.T) string {
	t.Helper()
	plan, err := campaignBidding(0)
	if err != nil {
		t.Fatal(err)
	}
	return buildCampaignName("KubeCon", plan.nameObjective, "CNCF", conversionInput())
}

// TestAssociateConversions_RefusesAMalformedCampaignID pins that an upstream campaign id that is
// not a positive long never reaches the compound key: no request, every id failed.
func TestAssociateConversions_RefusesAMalformedCampaignID(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	for _, bad := range []string{"200)?q=unexpected", "0200", "", "abc"} {
		assoc, failed, steps := c.associateConversions(context.Background(), bad, []string{"104012", "7"}, false)
		if len(assoc) != 0 || strings.Join(failed, ",") != "104012,7" || len(steps) != 2 {
			t.Errorf("%q: associated %v failed %v steps %v", bad, assoc, failed, steps)
		}
	}
	// A reused campaign keeps its own status, which may be ACTIVE, so its step must not say
	// "before activating": it says the campaign may already be serving without the conversion.
	_, _, steps := c.associateConversions(context.Background(), "abc", []string{"104012"}, true)
	if len(steps) != 1 || !strings.Contains(steps[0], "may already be serving") || strings.Contains(steps[0], "before activating") {
		t.Errorf("reused malformed-id step = %v, want the reused advice", steps)
	}
	_, _, steps = c.associateConversions(context.Background(), "abc", []string{"104012"}, false)
	if len(steps) != 1 || !strings.Contains(steps[0], "before activating") {
		t.Errorf("created malformed-id step = %v, want the before-activating advice", steps)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d requests for a malformed campaign id", n)
	}
}

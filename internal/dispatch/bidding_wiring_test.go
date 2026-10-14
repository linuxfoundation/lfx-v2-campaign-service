// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// These pin the DISPATCHER half of LFXV2-2665 on Meta, Reddit, X and LinkedIn: the config's
// `bidding` reaches the platform client as the manual request shape (each client's own tests pin
// that shape against the bid lever's predicate), and the returned row records the strategy and
// the starting bid. A dropped mapping would create an automatic campaign while the row claimed
// manual — the exact divergence the column exists to prevent.

// bodyLog records request bodies (and query strings) by path suffix.
type bodyLog struct {
	mu   sync.Mutex
	seen map[string]string
}

func (l *bodyLog) put(key, v string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]string{}
	}
	l.seen[key] = v
}

func (l *bodyLog) get(key string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[key]
}

func assertManualRow(t *testing.T, camp *model.Campaign, bid float64) {
	t.Helper()
	if camp == nil || camp.BiddingStrategy == nil || *camp.BiddingStrategy != model.BiddingManual || camp.MaxCPCBid == nil || *camp.MaxCPCBid != bid {
		var s any
		var b any
		if camp != nil {
			s, b = camp.BiddingStrategy, camp.MaxCPCBid
		}
		t.Fatalf("row bidding = %v / %v, want manual / %v", s, b, bid)
	}
}

func TestMeta_DispatchManualBiddingReachesTheAdSet(t *testing.T) {
	log := &bodyLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			log.put("adsets", string(b))
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"metaConfig":{"budget":50,"startDate":"2099-01-01","endDate":"2099-02-01",
		"objective":"traffic","geoTargets":["US"],"currencyOffset":100,
		"variants":[{"headline":"KubeCon","primaryText":"Join us"}],
		"bidding":{"strategy":"manual","bidAmount":1.25}}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	adset := log.get("adsets")
	for _, want := range []string{`"bid_strategy":"LOWEST_COST_WITH_BID_CAP"`, `"billing_event":"LINK_CLICKS"`, `"bid_amount":125`} {
		if !strings.Contains(adset, want) {
			t.Errorf("ad set body %s lacks %s", adset, want)
		}
	}
	assertManualRow(t, camp, 1.25)
}

func TestReddit_DispatchManualBiddingReachesTheAdGroup(t *testing.T) {
	log := &bodyLog{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			log.put("campaigns", string(b))
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_123"}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			log.put("ad_groups", string(b))
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"],"bidding":{"strategy":"manual","bidAmount":0.75}}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if c := log.get("campaigns"); !strings.Contains(c, `"bid_strategy":"MANUAL_BIDDING"`) {
		t.Errorf("campaign body %s is not MANUAL_BIDDING", c)
	}
	if ag := log.get("ad_groups"); !strings.Contains(ag, `"bid_strategy":"MANUAL_BIDDING"`) || !strings.Contains(ag, `"bid_value":750000`) {
		t.Errorf("ad group body %s lacks MANUAL_BIDDING / bid_value 750000", ag)
	}
	assertManualRow(t, camp, 0.75)
}

func TestTwitter_DispatchManualBiddingReachesTheLineItem(t *testing.T) {
	log := &bodyLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case strings.HasSuffix(r.URL.Path, "line_items"):
			log.put("line_items", r.URL.RawQuery)
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890","bidding":{"strategy":"manual","bidAmount":2.5}}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	q := log.get("line_items")
	for _, want := range []string{"bid_strategy=MAX", "pay_by=LINK_CLICK", "bid_amount_local_micro=2500000"} {
		if !strings.Contains(q, want) {
			t.Errorf("line item params %q lack %s", q, want)
		}
	}
	assertManualRow(t, camp, 2.5)
}

func TestLinkedIn_DispatchManualBiddingReachesTheCampaign(t *testing.T) {
	log := &bodyLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			log.put("adCampaigns", string(b))
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}],
		"bidding":{"strategy":"manual","bidAmount":4.5}
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	body := log.get("adCampaigns")
	for _, want := range []string{`"objectiveType":"WEBSITE_CONVERSION"`, `"costType":"CPC"`, `"optimizationTargetType":"NONE"`, `"unitCost":{"amount":"4.50","currencyCode":"USD"}`, `| LinkedIn | Conversions |`} {
		if !strings.Contains(body, want) {
			t.Errorf("campaign body %s lacks %s", body, want)
		}
	}
	assertManualRow(t, camp, 4.5)
}

// TestLinkedIn_DispatchAutomaticBiddingIsWebsiteVisitMaxClick: a LinkedIn dispatch that omits
// `bidding` (the automatic default) or asks for automatic creates a WEBSITE_VISIT campaign bidding
// MAX_CLICK with no unitCost and a "Traffic" name segment, and records automatic with no bid.
func TestLinkedIn_DispatchAutomaticBiddingIsWebsiteVisitMaxClick(t *testing.T) {
	for name, bidding := range map[string]string{"omitted": ``, "automatic": `,"bidding":{"strategy":"automatic"}`} {
		t.Run(name, func(t *testing.T) {
			log := &bodyLog{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
					return
				}
				b, _ := io.ReadAll(r.Body)
				switch {
				case strings.Contains(r.URL.Path, "adCampaignGroups"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
				case strings.Contains(r.URL.Path, "adCampaigns"):
					log.put("adCampaigns", string(b))
					w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
					_, _ = io.WriteString(w, `{}`)
				case strings.Contains(r.URL.Path, "posts"):
					_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
				case strings.Contains(r.URL.Path, "creatives"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
				default:
					http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
				}
			}))
			defer srv.Close()
			d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
				linkedin.WithBaseURL(srv.URL), linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
			cfg := json.RawMessage(`{"linkedInConfig":{
				"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
				"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
				"targetingProfile":"cloud-native",
				"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
				"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]` + bidding + `
			}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			body := log.get("adCampaigns")
			for _, want := range []string{`"objectiveType":"WEBSITE_VISIT"`, `"optimizationTargetType":"MAX_CLICK"`, `"costType":"CPM"`, `| LinkedIn | Traffic |`} {
				if !strings.Contains(body, want) {
					t.Errorf("campaign body %s lacks %s", body, want)
				}
			}
			if strings.Contains(body, "unitCost") {
				t.Errorf("an automatic campaign must not send unitCost: %s", body)
			}
			if camp.BiddingStrategy == nil || *camp.BiddingStrategy != model.BiddingAutomatic || camp.MaxCPCBid != nil {
				t.Fatalf("row = %v / %v, want automatic / nil", camp.BiddingStrategy, camp.MaxCPCBid)
			}
		})
	}
}

// assertNoBiddingRecorded: a reused bid-carrying entity, or a partial, must leave the row's
// strategy and bid NULL — the row must never claim a bid the platform was not given.
func assertNoBiddingRecorded(t *testing.T, camp *model.Campaign) {
	t.Helper()
	if camp == nil {
		t.Fatal("no campaign row returned")
	}
	if camp.BiddingStrategy != nil || camp.MaxCPCBid != nil {
		t.Fatalf("row records bidding %v / %v, want both nil (bid not applied)", camp.BiddingStrategy, camp.MaxCPCBid)
	}
}

func TestTwitter_DispatchReusedLineItemRecordsNoBid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":[{"id":"li9","name":"Events | KubeCon NA 2026 | Promoted Tweets | MAX"}]}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case strings.HasSuffix(r.URL.Path, "line_items"):
			t.Errorf("a reused line item must not be re-created")
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890","bidding":{"strategy":"manual","bidAmount":2.5}}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	assertNoBiddingRecorded(t, camp)
}

func TestLinkedIn_DispatchReusedCampaignRecordsNoBid(t *testing.T) {
	const name = "Events | KubeCon NA 2026 | LinkedIn | Conversions | Prospecting | Static | cncf | MoFU"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if strings.Contains(r.URL.Path, "adCampaigns") && !strings.Contains(r.URL.Path, "adCampaignGroups") {
				_, _ = io.WriteString(w, `{"elements":[{"name":"`+name+`","status":"PAUSED","id":"urn:li:sponsoredCampaign:777","campaignGroup":"urn:li:sponsoredCampaignGroup:100"}],"metadata":{"nextPageToken":""}}`)
				return
			}
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			t.Errorf("a reused campaign must not be re-created")
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}],
		"bidding":{"strategy":"manual","bidAmount":4.5}
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.PlatformCampaignID != "777" {
		t.Fatalf("campaign = %q, want the reused 777", camp.PlatformCampaignID)
	}
	assertNoBiddingRecorded(t, camp)
}

// TestMicrosoft_DispatchPartialRecordsNoBid: the campaign was created but the ad group POST is
// ambiguous (5xx), so the UNCONFIRMED partial row carries no recorded bidding.
func TestMicrosoft_DispatchPartialRecordsNoBid(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"at","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Campaigns"):
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiSrv.Close()
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds,
		json.RawMessage(`{"microsoftConfig":{"budget":50,"bidding":{"strategy":"manual","bidAmount":1.5}}}`))
	if err == nil {
		t.Fatal("Dispatch succeeded; want an UNCONFIRMED partial")
	}
	assertNoBiddingRecorded(t, camp)
}

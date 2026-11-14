// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// frozenNameServer answers a CreateCampaign whose campaign group already exists and holds one
// campaign named existingName (WEBSITE_VISIT, so automatic bidding may reuse it). It counts
// campaign POSTs: a POST is a second, paid campaign beside the existing one.
func frozenNameServer(t *testing.T, existingName string) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	campaignPosts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if strings.Contains(r.URL.Path, "adCampaigns") && !strings.Contains(r.URL.Path, "adCampaignGroups") {
				_, _ = io.WriteString(w, `{"elements":[{"name":"`+existingName+`","status":"PAUSED","id":"urn:li:sponsoredCampaign:777","campaignGroup":"urn:li:sponsoredCampaignGroup:100","objectiveType":"WEBSITE_VISIT"}],"metadata":{"nextPageToken":""}}`)
				return
			}
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			mu.Lock()
			campaignPosts++
			mu.Unlock()
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
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return campaignPosts }
}

// #587 retry safety: a retry of a partial create whose campaign was created under an EARLIER
// naming rule (here a project segment the current rules no longer compose) must find and reuse
// that campaign by the name the row recorded. Recomposing the name would miss it and POST a
// duplicate paid campaign into the same group.
func TestCreateCampaign_FrozenNameReusesTheCampaignAnEarlierAttemptCreated(t *testing.T) {
	const recorded = "Events | KubeCon | LinkedIn | Traffic | Prospecting | Static | proj-kubecon-old | MoFU"
	in := biddingCreateInput(0)
	if composed := buildCampaignName(in.EventName, "Traffic", in.Project, in); composed == recorded {
		t.Fatalf("fixture: the composed name %q must differ from the recorded one", composed)
	}
	in.FrozenCampaignName = recorded

	srv, posts := frozenNameServer(t, recorded)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CampaignID != "777" || posts() != 0 {
		t.Fatalf("campaign = %q, campaign POSTs = %d; want the existing 777 reused and no POST (a POST is a duplicate)", res.CampaignID, posts())
	}
	if res.CampaignName != recorded {
		t.Errorf("CampaignName = %q, want the recorded %q", res.CampaignName, recorded)
	}
}

// The control: with no recorded name the client composes one as before, and the same server's
// differently named campaign is (correctly) not reused.
func TestCreateCampaign_NoFrozenNameComposesTheName(t *testing.T) {
	in := biddingCreateInput(0)
	srv, posts := frozenNameServer(t, "Events | KubeCon | LinkedIn | Traffic | Prospecting | Static | proj-kubecon-old | MoFU")
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if want := buildCampaignName(in.EventName, "Traffic", in.Project, in); res.CampaignName != want || posts() != 1 {
		t.Fatalf("CampaignName = %q, POSTs = %d; want the composed %q and one create", res.CampaignName, posts(), want)
	}
}

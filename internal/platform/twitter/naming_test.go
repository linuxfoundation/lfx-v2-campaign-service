// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/campaignname"
)

func TestBuildTwitterCampaignNameV2(t *testing.T) {
	cncf := campaignname.Naming{BusinessUnit: "Cloud Native Computing Foundation (CNCF)"}
	base := CampaignInput{EventName: "KubeCon | NA", Project: "cncf"}
	for _, tc := range []struct {
		name   string
		naming campaignname.Naming
		geos   []string
		mutate func(*CampaignInput)
		want   string
	}{
		{"no geo targets: Global, Prospecting, a text post", cncf, nil, nil,
			"Events | KubeCon - NA | Global | Traffic | Prospecting | Text | Cloud Native Computing Foundation (CNCF) | MoFU"},
		{"one bucket and an image", cncf, []string{"DE", "FR"}, func(in *CampaignInput) { in.Image = &ImageInput{} },
			"Events | KubeCon - NA | EMEA | Traffic | Prospecting | Static | Cloud Native Computing Foundation (CNCF) | MoFU"},
		{"two buckets are Global", cncf, []string{"US", "JP"}, nil,
			"Events | KubeCon - NA | Global | Traffic | Prospecting | Text | Cloud Native Computing Foundation (CNCF) | MoFU"},
		{"caller region and intent win", campaignname.Naming{Region: "India", AudienceIntent: "Intent", BusinessUnit: "LF Events"}, []string{"US"}, nil,
			"Events | KubeCon - NA | India | Traffic | Intent | Text | LF Events | MoFU"},
		{"an existing post is Social, even with an image", cncf, nil, func(in *CampaignInput) { in.TweetID, in.Image = "1234567890", &ImageInput{} },
			"Events | KubeCon - NA | Global | Traffic | Prospecting | Social | Cloud Native Computing Foundation (CNCF) | MoFU"},
		{"retargeting tactic, slot token and stage", cncf, []string{"US"}, func(in *CampaignInput) {
			in.Tactic, in.SlotToken, in.StageToken = "retargeting", "#2", "BoFU"
		}, "Events | KubeCon - NA | NA | Traffic | Retargeting retargeting #2 | Text | Cloud Native Computing Foundation (CNCF) | BoFU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			got := buildTwitterCampaignNameV2(in, tc.geos, tc.naming)
			if got != tc.want {
				t.Errorf("name = %q\nwant   %q", got, tc.want)
			}
			if n := strings.Count(got, " | "); n != 7 {
				t.Errorf("name has %d separators, want 7", n)
			}
		})
	}
}

const (
	xLegacyName = "Events | LFX Retest | Global | Awareness | Prospecting | Promoted Post | CNCF | MoFU"
	xV2Name     = "Events | LFX Retest | Global | Traffic | Prospecting | Text | Cloud Native Computing Foundation (CNCF) | MoFU"
)

func (f *fakeX) campaignNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.campaigns {
		out = append(out, c["name"].(string))
	}
	return out
}

func TestCreateCampaign_TwitterNamingV2NamesANewCampaign(t *testing.T) {
	for _, tc := range []struct {
		name   string
		naming *campaignname.Naming
		want   string
	}{
		{"nil naming: the legacy name", nil, xLegacyName},
		{"naming v2", &campaignname.Naming{BusinessUnit: "cloud native computing foundation (cncf)"}, xV2Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeX()
			in := baseAuthorInput("Join us at KubeCon")
			in.Naming = tc.naming
			res, err := newFakeXClient(t, f).CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if got := f.campaignNames(); len(got) != 1 || got[0] != tc.want || res.CampaignName != tc.want {
				t.Errorf("campaigns %q, result %q; want %q", got, res.CampaignName, tc.want)
			}
		})
	}
}

// #587 retry safety: a campaign an earlier attempt created under the legacy name — its row gone,
// so nothing is frozen — is found and reused under that name once v2 is on, never duplicated.
func TestCreateCampaign_TwitterNamingV2ReusesALegacyNamedCampaign(t *testing.T) {
	f := newFakeX()
	c := newFakeXClient(t, f)
	first, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err != nil {
		t.Fatalf("legacy attempt: %v", err)
	}
	in := baseAuthorInput("Join us at KubeCon")
	in.Naming = &campaignname.Naming{BusinessUnit: "Cloud Native Computing Foundation (CNCF)"}
	second, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("v2 attempt: %v", err)
	}
	if got := f.postCount("campaign"); got != 1 {
		t.Errorf("campaign POSTs = %d, want 1 (the legacy campaign reused)", got)
	}
	if second.CampaignID != first.CampaignID || second.CampaignName != xLegacyName {
		t.Errorf("v2 attempt reused %s %q; want %s under the legacy name", second.CampaignID, second.CampaignName, first.CampaignID)
	}
	if !stepsContain(second.Steps, "already exists under the legacy name") {
		t.Errorf("steps %q; want one naming the legacy reuse", second.Steps)
	}
}

// A name the claimed row already records is the reuse key, over both the legacy and the v2 name,
// and no legacy lookup is added on top of it.
func TestCreateCampaign_TwitterFrozenNameWinsOverNamingV2(t *testing.T) {
	f := newFakeX()
	in := baseAuthorInput("Join us at KubeCon")
	in.Naming = &campaignname.Naming{BusinessUnit: "LF Events"}
	in.FrozenCampaignName = " " + xLegacyName + " "
	res, err := newFakeXClient(t, f).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if got := f.campaignNames(); len(got) != 1 || got[0] != xLegacyName || res.CampaignName != xLegacyName {
		t.Errorf("campaigns %q, result %q; want the frozen %q", got, res.CampaignName, xLegacyName)
	}
	if !stepsContain(res.Steps, "recorded name") {
		t.Errorf("steps %q; want one recording the frozen name", res.Steps)
	}
}

func TestCreateCampaign_TwitterNamingV2RefusedBeforeAnyRequest(t *testing.T) {
	for _, n := range []campaignname.Naming{{}, {BusinessUnit: "cncf"}, {Region: "Twitter", BusinessUnit: "LF Events"}} {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}))
		in := baseAuthorInput("Join us at KubeCon")
		in.Naming = &n
		res, err := newAuthorTweetTestClient(srv.URL).CreateCampaign(context.Background(), in)
		srv.Close()
		if _, ok := domain.AsPreflightRefusal(err); res != nil || !ok || requests.Load() != 0 {
			t.Errorf("naming %+v: CreateCampaign = %+v, %v after %d request(s); want a pre-flight refusal before any", n, res, err, requests.Load())
		}
		if n.BusinessUnit == "cncf" && !errors.Is(err, ErrNamingInvalid) {
			t.Errorf("naming %+v: %v; want ErrNamingInvalid", n, err)
		}
	}
}

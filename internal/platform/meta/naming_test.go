// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/campaignname"
)

func TestBuildCampaignNameV2(t *testing.T) {
	cncf := campaignname.Naming{BusinessUnit: "Cloud Native Computing Foundation (CNCF)"}
	base := CampaignInput{EventName: " KubeCon | NA ", Project: "cncf", Objective: "traffic"}
	for _, tc := range []struct {
		name   string
		naming campaignname.Naming
		geos   []string
		mutate func(*CampaignInput)
		want   string
	}{
		{"defaults: one bucket, Intent, Static", cncf, []string{"US", "CA"}, nil,
			"Events | KubeCon - NA | NA | Traffic | Intent | Static | Cloud Native Computing Foundation (CNCF) | MoFU"},
		{"two buckets are Global, not the first country's", cncf, []string{"US", "DE"}, nil,
			"Events | KubeCon - NA | Global | Traffic | Intent | Static | Cloud Native Computing Foundation (CNCF) | MoFU"},
		{"caller region and intent win", campaignname.Naming{Region: "LATAM", AudienceIntent: "Prospecting", BusinessUnit: "LF Events"}, []string{"US"}, nil,
			"Events | KubeCon - NA | LATAM | Traffic | Prospecting | Static | LF Events | MoFU"},
		{"retargeting tactic, slot token and stage", cncf, []string{"IN"}, func(in *CampaignInput) {
			in.Tactic, in.SlotToken, in.StageToken = "retargeting", "#2", "BoFU"
		}, "Events | KubeCon - NA | India | Traffic | Retargeting retargeting #2 | Static | Cloud Native Computing Foundation (CNCF) | BoFU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			got := buildCampaignNameV2(in, tc.geos, tc.naming)
			if got != tc.want {
				t.Errorf("name = %q\nwant   %q", got, tc.want)
			}
			if n := strings.Count(got, " | "); n != 7 {
				t.Errorf("name has %d separators, want 7", n)
			}
		})
	}
}

// metaNamingServer answers a full CreateCampaign. existing names the campaigns a by-name lookup
// finds (each PAUSED, objective OUTCOME_TRAFFIC). It records every lookup's name and every campaign
// POST's name.
func metaNamingServer(t *testing.T, existing ...string) (*httptest.Server, func() (lookups, posted []string)) {
	t.Helper()
	var mu sync.Mutex
	var lookups, posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/campaigns"):
			var f []map[string]string
			_ = json.Unmarshal([]byte(r.URL.Query().Get("filtering")), &f)
			name := ""
			if len(f) == 1 {
				name = f[0]["value"]
			}
			lookups = append(lookups, name)
			for _, e := range existing {
				if e == name {
					_, _ = io.WriteString(w, `{"data":[{"id":"120100000000777","status":"PAUSED","objective":"OUTCOME_TRAFFIC"}]}`)
					return
				}
			}
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x","account_status":1,"currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			var body struct {
				Name string `json:"name"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			posted = append(posted, body.Name)
			_, _ = io.WriteString(w, `{"id":"120100000000001"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000001"}`)
		default:
			_, _ = io.WriteString(w, `{"id":"120300000000001"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() ([]string, []string) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lookups...), append([]string(nil), posted...)
	}
}

const (
	metaLegacyName = "Events | KubeCon | NA | Traffic | Intent | Social | cncf | MoFU"
	metaV2Name     = "Events | KubeCon | NA | Traffic | Intent | Static | Cloud Native Computing Foundation (CNCF) | MoFU"
)

func metaNamingInput() CampaignInput {
	return CampaignInput{
		EventName: "KubeCon", Project: "cncf", Objective: "traffic",
		RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"US"},
		Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	}
}

func runMetaCreate(t *testing.T, srv *httptest.Server, in CampaignInput) (*CampaignResult, error) {
	t.Helper()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	return c.CreateCampaign(context.Background(), in)
}

func TestCreateCampaign_MetaNamingV2NamesANewCampaign(t *testing.T) {
	for _, tc := range []struct {
		name   string
		naming *campaignname.Naming
		want   string
	}{
		{"nil naming: the legacy name", nil, metaLegacyName},
		{"naming v2", &campaignname.Naming{BusinessUnit: "cloud native computing foundation (cncf)"}, metaV2Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := metaNamingServer(t)
			in := metaNamingInput()
			in.Naming = tc.naming
			res, err := runMetaCreate(t, srv, in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if _, posted := calls(); len(posted) != 1 || posted[0] != tc.want || res.CampaignName != tc.want {
				t.Errorf("posted %q, result %q; want %q", posted, res.CampaignName, tc.want)
			}
		})
	}
}

// A name the claimed row already records is sent verbatim, over both the legacy and the v2 name.
func TestCreateCampaign_MetaFrozenNameWinsOverNamingV2(t *testing.T) {
	srv, calls := metaNamingServer(t)
	in := metaNamingInput()
	in.Naming = &campaignname.Naming{BusinessUnit: "LF Events"}
	in.FrozenCampaignName = " " + metaLegacyName + " "
	res, err := runMetaCreate(t, srv, in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if _, posted := calls(); len(posted) != 1 || posted[0] != metaLegacyName {
		t.Errorf("posted %q; want the frozen name %q", posted, metaLegacyName)
	}
	if !anyStepContains(res.Steps, "recorded name") {
		t.Errorf("steps %q; want one recording the frozen name", res.Steps)
	}
}

// On the ReconcileByName path, naming v2 never duplicates a campaign an earlier attempt created
// under the legacy name: it is found and reused under that name.
func TestCreateCampaign_MetaNamingV2ReusesALegacyNamedCampaign(t *testing.T) {
	srv, calls := metaNamingServer(t, metaLegacyName)
	in := metaNamingInput()
	in.Naming = &campaignname.Naming{BusinessUnit: "Cloud Native Computing Foundation (CNCF)"}
	in.ReconcileByName = true
	res, err := runMetaCreate(t, srv, in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	lookups, posted := calls()
	if len(posted) != 0 {
		t.Errorf("posted %q; want no campaign created beside the legacy one", posted)
	}
	if len(lookups) != 2 || lookups[0] != metaV2Name || lookups[1] != metaLegacyName {
		t.Errorf("lookups %q; want the v2 name then the legacy name", lookups)
	}
	if res.CampaignID != "120100000000777" || res.CampaignName != metaLegacyName {
		t.Errorf("result %s %q; want the legacy campaign under its legacy name", res.CampaignID, res.CampaignName)
	}
}

// Without a legacy campaign the v2 name is created, after both lookups; without ReconcileByName
// no lookup is made at all (the path is dormant until LFXV2-2665 sets it).
func TestCreateCampaign_MetaNamingV2LegacyLookupOnlyOnTheReconcilePath(t *testing.T) {
	for _, reconcile := range []bool{true, false} {
		srv, calls := metaNamingServer(t)
		in := metaNamingInput()
		in.Naming = &campaignname.Naming{BusinessUnit: "Cloud Native Computing Foundation (CNCF)"}
		in.ReconcileByName = reconcile
		if _, err := runMetaCreate(t, srv, in); err != nil {
			t.Fatalf("CreateCampaign(reconcile=%v): %v", reconcile, err)
		}
		lookups, posted := calls()
		wantLookups := 0
		if reconcile {
			wantLookups = 2
		}
		if len(lookups) != wantLookups || len(posted) != 1 || posted[0] != metaV2Name {
			t.Errorf("reconcile=%v: lookups %q, posted %q; want %d lookups and the v2 name created", reconcile, lookups, posted, wantLookups)
		}
	}
}

func TestCreateCampaign_MetaNamingV2RefusedBeforeAnyRequest(t *testing.T) {
	for _, n := range []campaignname.Naming{{}, {BusinessUnit: "cncf"}, {Region: "Meta", BusinessUnit: "LF Events"}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("naming %+v: unexpected request %s %s", n, r.Method, r.URL.Path)
		}))
		in := metaNamingInput()
		in.Naming = &n
		res, err := runMetaCreate(t, srv, in)
		srv.Close()
		if _, ok := domain.AsPreflightRefusal(err); res != nil || !ok {
			t.Errorf("naming %+v: CreateCampaign = %+v, %v; want a pre-flight refusal", n, res, err)
		}
		if n.BusinessUnit == "cncf" && !errors.Is(err, ErrNamingInvalid) {
			t.Errorf("naming %+v: %v; want ErrNamingInvalid", n, err)
		}
	}
}

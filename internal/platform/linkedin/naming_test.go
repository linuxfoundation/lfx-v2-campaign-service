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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

var (
	geoJapan = GeoTarget{Label: "Japan", URN: "urn:li:geo:101355337"}
	geoUS    = GeoTarget{Label: "United States", URN: "urn:li:geo:103644278"}
	geoDE    = GeoTarget{Label: "Germany", URN: "urn:li:geo:101282230"}
	geoOther = GeoTarget{Label: "Brazil", URN: "urn:li:geo:106057199"}
)

// The v2 name (#587) is LF's convention, segment for segment; the issue's own example is the
// first golden. Every name keeps exactly the 8 segments the pipeline parses positionally.
func TestBuildCampaignNameV2(t *testing.T) {
	lfEvents := CampaignNaming{BusinessUnit: "LF Events"}
	for _, tc := range []struct {
		name   string
		obj    string
		naming CampaignNaming
		in     CampaignInput
		want   string
	}{
		{"the issue's example", "Conversions", CampaignNaming{Region: "APAC", AudienceIntent: "Intent", BusinessUnit: "LF Events"}, CampaignInput{},
			"Events | Open Source Summit Japan 2026 | APAC | Conversions | Intent | Static | LF Events | MoFU"},
		{"region defaults from a single-bucket geo", "Traffic", lfEvents, CampaignInput{GeoTargets: []GeoTarget{geoJapan}},
			"Events | Open Source Summit Japan 2026 | APAC | Traffic | Prospecting | Static | LF Events | MoFU"},
		{"two buckets are Global", "Traffic", lfEvents, CampaignInput{GeoTargets: []GeoTarget{geoUS, geoDE}},
			"Events | Open Source Summit Japan 2026 | Global | Traffic | Prospecting | Static | LF Events | MoFU"},
		{"an unbucketed geo is Global", "Traffic", lfEvents, CampaignInput{GeoTargets: []GeoTarget{geoUS, geoOther}},
			"Events | Open Source Summit Japan 2026 | Global | Traffic | Prospecting | Static | LF Events | MoFU"},
		{"caller region wins over geo", "Traffic", CampaignNaming{Region: "LATAM", BusinessUnit: "LF Events"}, CampaignInput{GeoTargets: []GeoTarget{geoUS}},
			"Events | Open Source Summit Japan 2026 | LATAM | Traffic | Prospecting | Static | LF Events | MoFU"},
		{"retargeting tactic defaults the intent and keeps its token", "Traffic", lfEvents, CampaignInput{GeoTargets: []GeoTarget{geoUS}, StageToken: "BoFU", Tactic: "retargeting"},
			"Events | Open Source Summit Japan 2026 | NA | Traffic | Retargeting retargeting | Static | LF Events | BoFU"},
		{"dynamic segments default the intent", "Traffic", lfEvents, CampaignInput{GeoTargets: []GeoTarget{geoUS}, Targeting: &Targeting{Include: []map[string][]string{{FacetDynamicSegments: {"urn:li:adSegment:1"}}}}},
			"Events | Open Source Summit Japan 2026 | NA | Traffic | Retargeting | Static | LF Events | MoFU"},
		{"an excluded dynamic segment is not retargeting", "Traffic", lfEvents, CampaignInput{GeoTargets: []GeoTarget{geoUS}, Targeting: &Targeting{Include: []map[string][]string{{"skills": {"urn:li:skill:1"}}}, Exclude: map[string][]string{FacetDynamicSegments: {"urn:li:adSegment:1"}}}},
			"Events | Open Source Summit Japan 2026 | NA | Traffic | Prospecting | Static | LF Events | MoFU"},
		{"slot 2 keeps its token", "Traffic", CampaignNaming{AudienceIntent: "Intent", BusinessUnit: "Cloud Native Computing Foundation (CNCF)"}, CampaignInput{GeoTargets: []GeoTarget{geoUS}, SlotToken: "#2"},
			"Events | Open Source Summit Japan 2026 | NA | Traffic | Intent #2 | Static | Cloud Native Computing Foundation (CNCF) | MoFU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildCampaignNameV2("Open Source Summit Japan 2026", tc.obj, tc.naming, tc.in)
			if got != tc.want {
				t.Errorf("name = %q\nwant   %q", got, tc.want)
			}
			if n := strings.Count(got, " | "); n != 7 {
				t.Errorf("name %q has %d separators, want 7 (8 segments)", got, n)
			}
		})
	}
}

func TestValidateNaming(t *testing.T) {
	got, err := ValidateNaming(CampaignNaming{Region: " apac ", AudienceIntent: "retargeting", BusinessUnit: "lf events"})
	if err != nil || got != (CampaignNaming{Region: "APAC", AudienceIntent: "Retargeting", BusinessUnit: "LF Events"}) {
		t.Fatalf("ValidateNaming = %+v, %v; want the canonical spellings", got, err)
	}
	if got, err := ValidateNaming(CampaignNaming{}); err != nil || got != (CampaignNaming{}) {
		t.Fatalf("empty naming = %+v, %v; want it accepted and left empty", got, err)
	}
	for _, tc := range []struct {
		n     CampaignNaming
		field string
	}{
		{CampaignNaming{Region: "LinkedIn"}, "region"},
		{CampaignNaming{AudienceIntent: "Lookalike"}, "audienceIntent"},
		{CampaignNaming{BusinessUnit: "proj-kubecon-na-2026"}, "businessUnit"},
		{CampaignNaming{BusinessUnit: "CNCF"}, "businessUnit"},
	} {
		if _, err := ValidateNaming(tc.n); !errors.Is(err, ErrNamingInvalid) || !strings.Contains(err.Error(), tc.field+" ") {
			t.Errorf("ValidateNaming(%+v) = %v, want ErrNamingInvalid naming %s", tc.n, err, tc.field)
		}
	}
}

// dbtNormalizeProjectName is a COPY of lf-dbt's normalize_project_name (macros/parse_campaign_name.sql
// at origin/main 0b6b6c15d): ILIKE '%x%' tested in order, else the input unchanged. It exists only
// to prove each business unit this service will write survives the pipeline's normalizer as
// itself; if lf-dbt changes the macro, update this copy and NameBusinessUnits together.
func dbtNormalizeProjectName(v string) string {
	l := strings.ToLower(v)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(l, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("cncf", "cloud native computing"):
		return "Cloud Native Computing Foundation (CNCF)"
	case has("education", "edu"):
		return "LF Education"
	case has("event"):
		return "LF Events"
	case has("pytorch"):
		return "PyTorch Project"
	case has("opensearch"):
		return "OpenSearch Project"
	case has("openssf"):
		return "Open Source Security Foundation (OpenSSF)"
	case has("risc-v", "riscv"):
		return "RISC-V International"
	case has("aaif"):
		return "Agentic AI Foundation (AAIF)"
	case has("valkey"):
		return "Valkey"
	case has("finops"):
		return "FinOps Project"
	}
	return v
}

func TestNameBusinessUnits_AreFixedPointsOfTheDbtNormalizer(t *testing.T) {
	seen := map[string]bool{}
	for _, bu := range NameBusinessUnits {
		if got := dbtNormalizeProjectName(bu); got != bu {
			t.Errorf("business unit %q normalizes to %q in lf-dbt; the pipeline would report it under another name", bu, got)
		}
		if strings.Contains(bu, "|") || seen[bu] {
			t.Errorf("business unit %q is duplicated or contains a pipe", bu)
		}
		seen[bu] = true
	}
}

// namingServer answers a CreateCampaign. With reuseGroup the event's group already exists (id
// 100) and holds the campaigns named in existing; otherwise no group exists and one is created.
// It records the name of every campaign POST.
func namingServer(t *testing.T, reuseGroup bool, existing ...string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaignGroups/"):
			_, _ = io.WriteString(w, `{"id":100,"runSchedule":{"start":1}}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaignGroups"):
			if reuseGroup {
				_, _ = io.WriteString(w, `{"elements":[{"id":100,"name":"Events | KubeCon | CNCF","status":"ACTIVE"}],"metadata":{}}`)
				return
			}
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaigns"):
			els := make([]map[string]any, 0, len(existing))
			for i, n := range existing {
				els = append(els, map[string]any{"id": 700 + i, "name": n, "status": "PAUSED", "campaignGroup": "urn:li:sponsoredCampaignGroup:100", "objectiveType": "WEBSITE_VISIT"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"elements": els, "metadata": map[string]any{}})
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			var body struct {
				Name string `json:"name"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			posted = append(posted, body.Name)
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
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), posted...) }
}

const (
	kubeconLegacyName = "Events | KubeCon | LinkedIn | Traffic | Prospecting | Static | CNCF | MoFU"
	kubeconV2Name     = "Events | KubeCon | NA | Traffic | Prospecting | Static | Cloud Native Computing Foundation (CNCF) | MoFU"
)

func v2Input() CampaignInput {
	in := biddingCreateInput(0)
	in.Naming = &CampaignNaming{BusinessUnit: "Cloud Native Computing Foundation (CNCF)"}
	return in
}

func runNamingCreate(t *testing.T, srv *httptest.Server, in CampaignInput) *CampaignResult {
	t.Helper()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	return res
}

// With Naming set, a new campaign is POSTed under the v2 name; with it nil, under the legacy name.
func TestCreateCampaign_NamingV2NamesANewCampaign(t *testing.T) {
	srv, posted := namingServer(t, false)
	if res := runNamingCreate(t, srv, v2Input()); res.CampaignName != kubeconV2Name || strings.Join(posted(), ",") != kubeconV2Name {
		t.Fatalf("v2: CampaignName = %q, POSTed %q; want %q", res.CampaignName, posted(), kubeconV2Name)
	}
	srv, posted = namingServer(t, false)
	if res := runNamingCreate(t, srv, biddingCreateInput(0)); res.CampaignName != kubeconLegacyName || strings.Join(posted(), ",") != kubeconLegacyName {
		t.Fatalf("legacy: CampaignName = %q, POSTed %q; want %q", res.CampaignName, posted(), kubeconLegacyName)
	}
}

// #587 retry safety, second guard: the slot's row is gone (an operator hard-deleted a stuck
// claim), so nothing is frozen, but its campaign lives on in the reused group under the LEGACY
// name. Turning v2 on must reuse it under that name, not POST a duplicate under the v2 name.
func TestCreateCampaign_NamingV2ReusesALegacyNamedCampaignInTheGroup(t *testing.T) {
	srv, posted := namingServer(t, true, kubeconLegacyName)
	res := runNamingCreate(t, srv, v2Input())
	if res.CampaignID != "700" || len(posted()) != 0 {
		t.Fatalf("campaign = %q, POSTed %q; want the legacy-named 700 reused and nothing created", res.CampaignID, posted())
	}
	if res.CampaignName != kubeconLegacyName {
		t.Errorf("CampaignName = %q, want the legacy %q (an existing campaign is never renamed)", res.CampaignName, kubeconLegacyName)
	}
}

// The control: a reused group WITHOUT a legacy-named campaign gets a new campaign under the v2 name.
func TestCreateCampaign_NamingV2CreatesInAReusedGroupWithoutALegacyCampaign(t *testing.T) {
	srv, posted := namingServer(t, true, "Events | KubeCon | LinkedIn | Traffic | Prospecting #2 | Static | CNCF | MoFU")
	res := runNamingCreate(t, srv, v2Input())
	if res.CampaignName != kubeconV2Name || strings.Join(posted(), ",") != kubeconV2Name {
		t.Fatalf("CampaignName = %q, POSTed %q; want one create under %q", res.CampaignName, posted(), kubeconV2Name)
	}
}

// A frozen (recorded) name wins over v2, too.
func TestCreateCampaign_FrozenNameWinsOverNamingV2(t *testing.T) {
	srv, posted := namingServer(t, true, kubeconLegacyName)
	in := v2Input()
	in.FrozenCampaignName = kubeconLegacyName
	if res := runNamingCreate(t, srv, in); res.CampaignID != "700" || len(posted()) != 0 || res.CampaignName != kubeconLegacyName {
		t.Fatalf("campaign = %q (%q), POSTed %q; want the recorded 700 reused", res.CampaignID, res.CampaignName, posted())
	}
}

// The client's own fail-closed check: v2 without a business unit, or with a value off its list,
// is refused before any request.
func TestCreateCampaign_NamingV2RefusedBeforeAnyRequest(t *testing.T) {
	for _, n := range []CampaignNaming{{}, {BusinessUnit: "proj-kubecon"}, {Region: "LinkedIn", BusinessUnit: "LF Events"}} {
		var requests int
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			requests++
			mu.Unlock()
			http.Error(w, "stop", http.StatusBadRequest)
		}))
		in := biddingCreateInput(0)
		in.Naming = &n
		c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
		res, err := c.CreateCampaign(context.Background(), in)
		srv.Close()
		if _, pre := domain.AsPreflightRefusal(err); res != nil || !pre || requests != 0 {
			t.Errorf("naming %+v: CreateCampaign = %+v, %v after %d requests; want a pre-flight refusal and none", n, res, err, requests)
		}
	}
}

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

const boostTestPost = "urn:li:share:7001"

func TestValidateBoostPostURN(t *testing.T) {
	for _, ok := range []string{"urn:li:share:7001", "urn:li:ugcPost:6856810298419044352"} {
		if err := ValidateBoostPostURN(ok); err != nil {
			t.Errorf("ValidateBoostPostURN(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "7001", "urn:li:share:", "urn:li:share:7001 ", " urn:li:share:7001", "urn:li:Share:7001",
		"urn:li:activity:7001", "urn:li:ugcPost:abc", "urn:li:share:7001/x", "https://www.linkedin.com/feed/update/urn:li:share:7001/"} {
		if err := ValidateBoostPostURN(bad); !errors.Is(err, ErrFormatInvalid) {
			t.Errorf("ValidateBoostPostURN(%q) = %v, want ErrFormatInvalid", bad, err)
		}
	}
}

func TestNormalizeFormatBoost(t *testing.T) {
	if got, err := NormalizeFormat("boost"); err != nil || got != FormatBoost {
		t.Errorf("NormalizeFormat(boost) = %q, %v", got, err)
	}
	if _, err := NormalizeFormat("BOOST"); !errors.Is(err, ErrFormatInvalid) {
		t.Errorf("NormalizeFormat(BOOST) = %v, want ErrFormatInvalid", err)
	}
}

// organicPost is a Posts API GET body for boostTestPost, authored by testConfig's org, that a boost
// accepts. mut edits it before it is encoded.
func organicPost(mut func(map[string]any)) string {
	p := map[string]any{
		"id": boostTestPost, "author": "urn:li:organization:987654321", "lifecycleState": "PUBLISHED",
		"visibility": "PUBLIC", "commentary": "Join us at KubeCon",
		"distribution": map[string]any{"feedDistribution": "MAIN_FEED", "thirdPartyDistributionChannels": []any{}},
		"content":      map[string]any{"media": map[string]any{"id": "urn:li:image:C4E22AQ"}},
	}
	if mut != nil {
		mut(p)
	}
	b, _ := json.Marshal(p)
	return string(b)
}

// boostServer records every request of a boost create.
type boostServer struct {
	mu         sync.Mutex
	calls      []string // method + escaped path, in order
	queries    []string // raw query of each creatives GET
	post       string   // GET /posts/{urn} body
	postStatus int      // GET /posts/{urn} status; 0 is 200
	campaign   map[string]any
	creatives  []map[string]any
	// existingCampaign answers the campaign search (a same-name campaign in group 100);
	// existingCreatives the creatives finder. Empty means none.
	existingCampaign  string
	existingCreatives string
	creativeStatus    int // creative POST status; 0 is 201
}

func (s *boostServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		path := r.URL.EscapedPath()
		s.calls = append(s.calls, r.Method+" "+path)
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/posts/"):
			if s.postStatus != 0 {
				w.WriteHeader(s.postStatus)
				_, _ = io.WriteString(w, `{"status":`+http.StatusText(s.postStatus)+`}`)
				return
			}
			_, _ = io.WriteString(w, s.post)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/creatives"):
			s.queries = append(s.queries, r.URL.RawQuery)
			if s.existingCreatives != "" {
				_, _ = io.WriteString(w, s.existingCreatives)
				return
			}
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case r.Method == http.MethodGet && s.existingCampaign != "" && strings.HasSuffix(path, "/adCampaigns"):
			_, _ = io.WriteString(w, s.existingCampaign)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case strings.Contains(path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(path, "adCampaigns"):
			_ = json.Unmarshal(raw, &s.campaign)
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(path, "creatives"):
			var b map[string]any
			_ = json.Unmarshal(raw, &b)
			s.creatives = append(s.creatives, b)
			if s.creativeStatus != 0 {
				w.WriteHeader(s.creativeStatus)
				_, _ = io.WriteString(w, `{"status":400,"message":"bad"}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:401"}`)
		case strings.Contains(path, "posts"):
			t.Errorf("a boost create sent a dark post")
			http.Error(w, "no posts", http.StatusBadRequest)
		default:
			http.Error(w, "unexpected path "+path, http.StatusBadRequest)
		}
	}
}

// mutations reports the mutating requests (every non-GET) the server saw.
func (s *boostServer) mutations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

func boostInput() CampaignInput {
	in := conversionInput()
	in.Variants = nil
	in.Format = FormatBoost
	in.BoostPost = boostTestPost
	return in
}

func newBoostClient(t *testing.T, s *boostServer) *Client {
	t.Helper()
	if s.post == "" {
		s.post = organicPost(nil)
	}
	srv := httptest.NewServer(s.handler(t))
	t.Cleanup(srv.Close)
	return NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
}

// TestCreateCampaign_BoostWireBodies pins the boost create end to end: the post is read (its URN
// percent-encoded in the path) before the first POST, the campaign is the ordinary automatic
// SPONSORED_UPDATES one with the Boost name segment and no format, no dark post is sent, and the
// one DRAFT creative references the post. The result records the post.
func TestCreateCampaign_BoostWireBodies(t *testing.T) {
	s := &boostServer{}
	res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CreativeCount != 1 || res.BoostedPostURN != boostTestPost || !res.BidApplied {
		t.Errorf("CreativeCount %d, BoostedPostURN %q, BidApplied %v; want 1, %s, true", res.CreativeCount, res.BoostedPostURN, res.BidApplied, boostTestPost)
	}
	if res.ObjectiveType != ObjectiveWebsiteVisit || res.OptimizationTarget != OptimizationTargetMaxClick {
		t.Errorf("objective %q / %q, want the automatic plan", res.ObjectiveType, res.OptimizationTarget)
	}
	wantName := "Events | KubeCon | NA | Traffic | Prospecting | Boost | CNCF | MoFU"
	if res.CampaignName != wantName {
		t.Errorf("campaign name = %q, want %q", res.CampaignName, wantName)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Creative (DRAFT) sponsoring post "+boostTestPost+": urn:li:sponsoredCreative:401") {
		t.Errorf("steps do not record the creative: %v", res.Steps)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 || s.calls[0] != "GET /posts/urn%3Ali%3Ashare%3A7001" {
		t.Fatalf("first request = %v, want the post read", s.calls)
	}
	for k, want := range map[string]any{"type": "SPONSORED_UPDATES", "objectiveType": "WEBSITE_VISIT", "status": "PAUSED", "name": wantName} {
		if s.campaign[k] != want {
			t.Errorf("campaign %s = %v, want %v", k, s.campaign[k], want)
		}
	}
	if _, has := s.campaign["format"]; has {
		t.Errorf("an image boost sends campaign format %v; LinkedIn sets it from the creative", s.campaign["format"])
	}
	if len(s.creatives) != 1 {
		t.Fatalf("creatives = %d, want 1", len(s.creatives))
	}
	cr := s.creatives[0]
	content, _ := cr["content"].(map[string]any)
	if len(content) != 1 || content["reference"] != boostTestPost {
		t.Errorf("creative content = %v, want only reference %s", content, boostTestPost)
	}
	if cr["campaign"] != "urn:li:sponsoredCampaign:200" || cr["intendedStatus"] != "DRAFT" || cr["name"] != "KubeCon | boost 7001" {
		t.Errorf("creative = %v", cr)
	}
	// A new campaign holds no creative: no finder lookup.
	if len(s.queries) != 0 {
		t.Errorf("creatives finder called on a new campaign: %v", s.queries)
	}
}

// TestCreateCampaign_BoostContentDecidesFormat: a video post's campaign is created with format
// SINGLE_VIDEO, which LinkedIn requires on a video campaign; text-only and article posts send none.
func TestCreateCampaign_BoostContentDecidesFormat(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content any
		want    any
	}{
		{"video", map[string]any{"media": map[string]any{"id": "urn:li:video:C5F10AQ"}}, "SINGLE_VIDEO"},
		{"article", map[string]any{"article": map[string]any{"source": "https://x.org", "title": "t"}}, nil},
		{"text only", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &boostServer{post: organicPost(func(p map[string]any) {
				if tc.content == nil {
					delete(p, "content")
				} else {
					p["content"] = tc.content
				}
			})}
			if _, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput()); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if got := s.campaign["format"]; got != tc.want {
				t.Errorf("campaign format = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCreateCampaign_BoostPostRefusedBeforeAnyPOST: every post LinkedIn would refuse to sponsor
// from this account, and every failed read, stops the create after the one GET, with no result
// and no POST. The post refusals are operator-safe pre-flight refusals; a read that failed for
// another reason is not.
func TestCreateCampaign_BoostPostRefusedBeforeAnyPOST(t *testing.T) {
	for _, tc := range []struct {
		name      string
		post      func(map[string]any)
		status    int
		want      string
		preflight bool
	}{
		{"another organization's post", func(p map[string]any) { p["author"] = "urn:li:organization:5515715" }, 0,
			`post "urn:li:share:7001" is authored by "urn:li:organization:5515715", not by urn:li:organization:987654321, the organization ad account 123456789 serves`, true},
		{"a member's post", func(p map[string]any) { p["author"] = "urn:li:person:abc" }, 0, `is authored by "urn:li:person:abc"`, true},
		{"draft", func(p map[string]any) { p["lifecycleState"] = "DRAFT" }, 0, `has lifecycleState "DRAFT"; only a PUBLISHED post can be boosted`, true},
		{"connections only", func(p map[string]any) { p["visibility"] = "CONNECTIONS" }, 0, `has visibility "CONNECTIONS"; LinkedIn sponsors only a PUBLIC post`, true},
		{"reshare", func(p map[string]any) {
			p["reshareContext"] = map[string]any{"parent": "urn:li:share:1", "root": "urn:li:share:1"}
		}, 0, "is a reshare", true},
		{"dark post", func(p map[string]any) { p["distribution"] = map[string]any{"feedDistribution": "NONE"} }, 0, `has feedDistribution "NONE"`, true},
		{"no distribution", func(p map[string]any) { delete(p, "distribution") }, 0, "has feedDistribution", true},
		{"poll", func(p map[string]any) { p["content"] = map[string]any{"poll": map[string]any{"question": "q"}} }, 0, "is a poll post, which LinkedIn does not sponsor", true},
		{"multi-image", func(p map[string]any) { p["content"] = map[string]any{"multiImage": map[string]any{}} }, 0, "is a multiImage post", true},
		{"document", func(p map[string]any) {
			p["content"] = map[string]any{"media": map[string]any{"id": "urn:li:document:D1"}}
		}, 0, `carries media "urn:li:document:D1"`, true},
		{"carousel", func(p map[string]any) { p["content"] = map[string]any{"carousel": map[string]any{}} }, 0, `carries "carousel" content`, true},
		{"not found", nil, http.StatusNotFound, `post "urn:li:share:7001" was not found (LinkedIn answered 404)`, true},
		{"not readable", nil, http.StatusForbidden, "needs the r_organization_social scope", true},
		{"another post answered", func(p map[string]any) { p["id"] = "urn:li:share:9" }, 0, `describes post "urn:li:share:9"`, false},
		{"server error", nil, http.StatusInternalServerError, `reading boost post "urn:li:share:7001" failed before anything was created`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &boostServer{post: organicPost(tc.post), postStatus: tc.status}
			res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
			if res != nil {
				t.Errorf("a result for a refused boost: %+v", res)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if pre != tc.preflight {
				t.Errorf("pre-flight refusal = %v (%q), want %v", pre, msg, tc.preflight)
			}
			if tc.preflight && !errors.Is(err, ErrBoostPostIneligible) {
				t.Errorf("err does not wrap ErrBoostPostIneligible: %v", err)
			}
			if m := s.mutations(); len(m) != 0 {
				t.Errorf("mutating requests sent for a refused post: %v", m)
			}
		})
	}
}

// TestCreateCampaign_BoostPostReadExpiry: a 401 on the post read is the connection's expiry, so
// the dispatcher reports the connection, and nothing is created.
func TestCreateCampaign_BoostPostReadExpiry(t *testing.T) {
	s := &boostServer{postStatus: http.StatusUnauthorized}
	res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
	if res != nil || !errors.Is(err, ErrCredentialsExpired) {
		t.Fatalf("res %v, err %v; want nil and ErrCredentialsExpired", res, err)
	}
	if m := s.mutations(); len(m) != 0 {
		t.Errorf("mutating requests: %v", m)
	}
}

// TestCreateCampaign_BoostRefusedBeforeAnyRequest: a malformed boost input is refused before the
// post is read.
func TestCreateCampaign_BoostRefusedBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*CampaignInput)
		want string
	}{
		{"malformed URN", func(in *CampaignInput) { in.BoostPost = "urn:li:activity:7001" }, "boostPost must be a LinkedIn post URN"},
		{"no post", func(in *CampaignInput) { in.BoostPost = "" }, "boostPost must be a LinkedIn post URN"},
		{"variants", func(in *CampaignInput) { in.Variants = []CreativeVariant{{IntroText: "x", Headline: "y"}} }, "takes no variants"},
		{"boost post on a single-image create", func(in *CampaignInput) {
			in.Format = ""
			in.Variants = []CreativeVariant{{IntroText: "x", Headline: "y"}}
		}, "boostPost is accepted only with format \"boost\""},
		{"text ads", func(in *CampaignInput) { in.TextAds = []TextAd{{Headline: "h", Description: "d"}} }, "textAds are accepted only with format"},
		{"lead generation", func(in *CampaignInput) {
			in.Objective = ObjectiveLeadGeneration
			in.LeadGen = &LeadGen{FormID: "123"}
		}, "format \"boost\" is not available with objective LEAD_GENERATION"},
		{"carousel cards", func(in *CampaignInput) {
			in.CarouselCards = []CarouselCard{{ImageAssetID: "a", Headline: "h"}, {ImageAssetID: "b", Headline: "h"}}
		}, "accepted only with format \"carousel\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &boostServer{}
			in := boostInput()
			tc.mut(&in)
			res, err := newBoostClient(t, s).CreateCampaign(context.Background(), in)
			msg, pre := domain.AsPreflightRefusal(err)
			if res != nil || !pre || !strings.Contains(msg, tc.want) {
				t.Fatalf("res %v, err %v; want a pre-flight refusal containing %q", res, err, tc.want)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.calls) != 0 {
				t.Errorf("requests sent: %v", s.calls)
			}
		})
	}
}

// existingBoostCampaign is the campaign-search answer for a boost create's own name in group 100.
func existingBoostCampaign(t *testing.T) string {
	t.Helper()
	first := &boostServer{}
	ok, err := newBoostClient(t, first).CreateCampaign(context.Background(), boostInput())
	if err != nil {
		t.Fatalf("clean create: %v", err)
	}
	el, _ := json.Marshal(map[string]any{"elements": []map[string]any{{
		"name": ok.CampaignName, "status": "PAUSED", "id": 555, "type": "SPONSORED_UPDATES",
		"objectiveType": "WEBSITE_VISIT", "optimizationTargetType": "MAX_CLICK",
		"campaignGroup": "urn:li:sponsoredCampaignGroup:100",
	}}, "metadata": map[string]any{}})
	return string(el)
}

// TestCreateCampaign_BoostReusedCampaignReusesItsCreative: on a campaign found by name, a creative
// already referencing the post (the creatives finder filtered by campaign and contentReferences)
// is reused, and no second creative is posted.
func TestCreateCampaign_BoostReusedCampaignReusesItsCreative(t *testing.T) {
	s := &boostServer{
		existingCampaign:  existingBoostCampaign(t),
		existingCreatives: `{"elements":[{"id":"urn:li:sponsoredCreative:777"}],"metadata":{}}`,
	}
	res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CampaignID != "555" || res.CreativeCount != 1 || res.BoostedPostURN != boostTestPost || res.BidApplied {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "found on campaign 555: urn:li:sponsoredCreative:777 (not created again)") {
		t.Errorf("steps = %v", res.Steps)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.creatives) != 0 {
		t.Errorf("a creative was posted beside the found one: %v", s.creatives)
	}
	if len(s.queries) != 1 || !strings.Contains(s.queries[0], "contentReferences=List(urn%3Ali%3Ashare%3A7001)") ||
		!strings.Contains(s.queries[0], "campaigns=List(urn%3Ali%3AsponsoredCampaign%3A555)") {
		t.Errorf("creatives finder queries = %v", s.queries)
	}
}

// TestCreateCampaign_BoostReusedCampaignWithoutCreative: a campaign found by name with no creative
// for the post gets one.
func TestCreateCampaign_BoostReusedCampaignWithoutCreative(t *testing.T) {
	s := &boostServer{existingCampaign: existingBoostCampaign(t)}
	res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queries) != 1 || len(s.creatives) != 1 || s.creatives[0]["campaign"] != "urn:li:sponsoredCampaign:555" {
		t.Errorf("queries %v, creatives %v", s.queries, s.creatives)
	}
	if res.BoostedPostURN != boostTestPost {
		t.Errorf("BoostedPostURN = %q", res.BoostedPostURN)
	}
}

// TestCreateCampaign_BoostCreativeFailure: a refused creative POST returns the partial result
// carrying the group and campaign and no recorded post; a 5xx is UNCONFIRMED.
func TestCreateCampaign_BoostCreativeFailure(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "boost creative for post urn:li:share:7001 failed"},
		{http.StatusInternalServerError, "boost creative for post urn:li:share:7001 UNCONFIRMED"},
	} {
		s := &boostServer{creativeStatus: tc.status}
		res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("status %d: err = %v, want %q", tc.status, err, tc.want)
		}
		if res == nil || res.CampaignGroupID != "100" || res.CampaignID != "200" || res.CreativeCount != 0 || res.BoostedPostURN != "" {
			t.Errorf("status %d: partial = %+v", tc.status, res)
		}
	}
}

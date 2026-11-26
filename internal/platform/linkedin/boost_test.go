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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	finderStatus      int    // creatives finder status; 0 is 200
	creativeStatus    int    // creative POST status; 0 is 201
	creativeBody      string // the failed creative POST's body; "" is a plain 400 envelope
	// stateful makes the server remember what it created: the group search and the group GET
	// answer the posted group, the campaign search the posted campaign, and the creatives finder
	// the posted creative. creativeDelay holds each creative POST that long before it lands.
	stateful      bool
	group         map[string]any
	creativeDelay time.Duration
	// creativeRead answers a creative GET (the author check after an unread post); "" reads the
	// posted creative back as sponsoring boostTestPost, authored by testConfig's org.
	// creativeReadStatus fails that GET (0 is 200); deleteStatus fails the creative DELETE (0 is
	// 204), and deletes counts the DELETEs.
	creativeRead       string
	creativeReadStatus int
	deleteStatus       int
	deletes            int
	// campaignCreatives, when set, answers the campaign-wide creatives listing (no
	// contentReferences filter) instead of existingCreatives.
	campaignCreatives string
}

// boostCreative is a creative GET body sponsoring post, authored by author. Its id is "SELF",
// which the fake server replaces with the URN the GET asked for.
func boostCreative(post, author string) string {
	b, _ := json.Marshal(map[string]any{"id": "SELF", "intendedStatus": "DRAFT",
		"content": map[string]any{"standardUpdateAd": map[string]any{"contentAuthor": author, "reference": post}}})
	return string(b)
}

func (s *boostServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		if r.Method == http.MethodPost && strings.HasSuffix(path, "/creatives") && s.creativeDelay > 0 {
			time.Sleep(s.creativeDelay) // outside the lock, so the other create's requests proceed
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, r.Method+" "+path)
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if s.stateful && s.statefulAnswer(w, r, path, raw) {
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/posts/"):
			if s.postStatus != 0 {
				w.WriteHeader(s.postStatus)
				_, _ = io.WriteString(w, `{"status":`+http.StatusText(s.postStatus)+`}`)
				return
			}
			_, _ = io.WriteString(w, s.post)
		case r.Method == http.MethodGet && strings.Contains(path, "/creatives/"):
			if s.creativeReadStatus != 0 {
				w.WriteHeader(s.creativeReadStatus)
				_, _ = io.WriteString(w, `{"status":500,"message":"read down"}`)
				return
			}
			body := s.creativeRead
			if body == "" {
				body = boostCreative(boostTestPost, "urn:li:organization:987654321")
			}
			asked, _ := url.PathUnescape(path[strings.LastIndex(path, "/")+1:])
			_, _ = io.WriteString(w, strings.Replace(body, `"SELF"`, strconv.Quote(asked), 1))
		case r.Method == http.MethodDelete && strings.Contains(path, "/creatives/"):
			s.deletes++
			if s.deleteStatus != 0 {
				w.WriteHeader(s.deleteStatus)
				_, _ = io.WriteString(w, `{"status":400,"message":"cannot delete"}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/creatives"):
			s.queries = append(s.queries, r.URL.RawQuery)
			if s.finderStatus != 0 {
				w.WriteHeader(s.finderStatus)
				_, _ = io.WriteString(w, `{"status":500,"message":"finder down"}`)
				return
			}
			if s.campaignCreatives != "" && !strings.Contains(r.URL.RawQuery, "contentReferences") {
				_, _ = io.WriteString(w, s.campaignCreatives)
				return
			}
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
				body := s.creativeBody
				if body == "" {
					body = `{"status":400,"message":"bad"}`
				}
				_, _ = io.WriteString(w, body)
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

// statefulAnswer serves the stateful mode's requests and reports whether it answered; anything
// else falls through to the handler's fixed answers (with existingCampaign and existingCreatives
// set by it as the campaign and the creative are posted).
func (s *boostServer) statefulAnswer(w http.ResponseWriter, r *http.Request, path string, raw []byte) bool {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/adCampaignGroups") && s.group != nil:
		el, _ := json.Marshal(map[string]any{"elements": []map[string]any{{"id": 100, "name": s.group["name"], "status": "ACTIVE"}}, "metadata": map[string]any{}})
		_, _ = w.Write(el)
		return true
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/adCampaignGroups/100") && s.group != nil:
		el, _ := json.Marshal(map[string]any{"id": 100, "runSchedule": s.group["runSchedule"]})
		_, _ = w.Write(el)
		return true
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/adCampaignGroups"):
		_ = json.Unmarshal(raw, &s.group)
		_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		return true
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/adCampaigns"):
		_ = json.Unmarshal(raw, &s.campaign)
		el, _ := json.Marshal(map[string]any{"elements": []map[string]any{{
			"name": s.campaign["name"], "status": "PAUSED", "id": 200, "type": s.campaign["type"],
			"objectiveType": s.campaign["objectiveType"], "optimizationTargetType": s.campaign["optimizationTargetType"],
			"campaignGroup": "urn:li:sponsoredCampaignGroup:100",
		}}, "metadata": map[string]any{}})
		s.existingCampaign = string(el)
		w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
		_, _ = io.WriteString(w, `{}`)
		return true
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/creatives"):
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		s.creatives = append(s.creatives, b)
		s.existingCreatives = `{"elements":[{"id":"urn:li:sponsoredCreative:401"}],"metadata":{}}`
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:401"}`)
		return true
	}
	return false
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
			s.mu.Lock()
			got := s.campaign["format"]
			s.mu.Unlock()
			if got != tc.want {
				t.Errorf("campaign format = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCreateCampaign_BoostPostRefusedBeforeAnyPOST: every post LinkedIn would refuse to sponsor
// from this account, and every failed read but a 403 (TestCreateCampaign_BoostUnreadablePost),
// stops the create after the one GET, with no result and no POST. The post refusals are operator-safe pre-flight refusals; a read that failed for
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
		{"a member's post", func(p map[string]any) { p["author"] = "urn:li:person:abc" }, 0, `is authored by a LinkedIn member, not by`, true},
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
			if strings.Contains(err.Error(), "urn:li:person:") {
				t.Errorf("err echoes a member's person URN: %v", err)
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

// TestCreateCampaign_BoostUnreadablePost: a 403 on the post read means this connection cannot read
// posts (no r_organization_social), not that the post is ineligible, so the pre-checks are skipped
// and the create goes on with no campaign format, letting LinkedIn's creative POST decide. When
// LinkedIn takes the creative the boost is created; when it refuses it as UGC_FORBIDDEN the result
// is the partial carrying the group and the PAUSED campaign, with the page-role sentence, after
// exactly one creative POST.
func TestCreateCampaign_BoostUnreadablePost(t *testing.T) {
	t.Run("LinkedIn takes the creative", func(t *testing.T) {
		s := &boostServer{postStatus: http.StatusForbidden}
		res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
		if err != nil {
			t.Fatalf("CreateCampaign: %v", err)
		}
		if res.CreativeCount != 1 || res.BoostedPostURN != boostTestPost {
			t.Errorf("CreativeCount %d, BoostedPostURN %q; want 1, %s", res.CreativeCount, res.BoostedPostURN, boostTestPost)
		}
		steps := strings.Join(res.Steps, "\n")
		if !strings.Contains(steps, "not pre-checked: this connection cannot read posts") {
			t.Errorf("steps do not record the skipped pre-checks: %v", res.Steps)
		}
		if !strings.Contains(steps, "Boost creative urn:li:sponsoredCreative:401 read back: the post is authored by urn:li:organization:987654321") {
			t.Errorf("steps do not record the creative's author check: %v", res.Steps)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.campaign["status"] != "PAUSED" {
			t.Errorf("campaign status = %v, want PAUSED", s.campaign["status"])
		}
		if _, has := s.campaign["format"]; has {
			t.Errorf("campaign format %v sent for an unread post; LinkedIn infers it from the creative", s.campaign["format"])
		}
		if len(s.creatives) != 1 {
			t.Fatalf("creatives = %d, want 1", len(s.creatives))
		}
		if content, _ := s.creatives[0]["content"].(map[string]any); len(content) != 1 || content["reference"] != boostTestPost {
			t.Errorf("creative content = %v, want only reference %s", content, boostTestPost)
		}
	})

	t.Run("LinkedIn refuses the creative as UGC_FORBIDDEN", func(t *testing.T) {
		s := &boostServer{postStatus: http.StatusForbidden, creativeStatus: http.StatusForbidden,
			creativeBody: `{"status":403,"code":"UGC_FORBIDDEN","message":"Accessing the UGC resource is forbidden"}`}
		res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
		want := `LinkedIn refused to sponsor post "urn:li:share:7001" from ad account 123456789 (UGC_FORBIDDEN): the connection's LinkedIn member needs a sponsoring role (ADMINISTRATOR, DIRECT_SPONSORED_CONTENT_POSTER or CONTENT_ADMIN) on the organization page that authored it`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
		if _, pre := domain.AsPreflightRefusal(err); pre {
			t.Errorf("a refusal after the campaign exists is reported as pre-flight: %v", err)
		}
		if res == nil || res.CampaignGroupID != "100" || res.CampaignID != "200" || res.CreativeCount != 0 || res.BoostedPostURN != "" {
			t.Fatalf("partial = %+v", res)
		}
		if !strings.Contains(strings.Join(res.Steps, "\n"), want) {
			t.Errorf("steps do not carry the refusal: %v", res.Steps)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.creatives) != 1 {
			t.Errorf("creative POSTs = %d, want exactly 1", len(s.creatives))
		}
	})
}

// TestCreateCampaign_BoostUnreadablePostChecksCreativeAuthor: with no post read (a 403), the
// creative this call posts is read back. Anything but the requested post, authored by the account's
// organization, deletes it and returns the partial marked BoostCreativeUnverified (pending), since
// this call changed LinkedIn: a wrong creative wraps ErrBoostPostIneligible, one that could not be
// checked wraps the read's own error (a 401 keeps ErrCredentialsExpired) or
// errBoostCreativeUnchecked. On a reused campaign, a creative it already holds that is wrong is
// named, never deleted, and nothing is posted.
func TestCreateCampaign_BoostUnreadablePostChecksCreativeAuthor(t *testing.T) {
	const org = "urn:li:organization:987654321"
	other := func(id string) string {
		return strings.Replace(boostCreative(boostTestPost, org), `"SELF"`, strconv.Quote(id), 1)
	}
	for _, tc := range []struct {
		name     string
		server   *boostServer
		want     []string
		notWants []string
		cause    error // what the error wraps
		deletes  int
		posts    int
		reused   bool
	}{
		{
			name:    "another organization's post",
			server:  &boostServer{creativeRead: boostCreative(boostTestPost, "urn:li:organization:111")},
			want:    []string{`the post is authored by "urn:li:organization:111", not by ` + org, "the DRAFT creative urn:li:sponsoredCreative:401 was deleted", "campaign 200 is left PAUSED"},
			cause:   ErrBoostPostIneligible,
			deletes: 1, posts: 1,
		},
		{
			name:     "a member's post names no member",
			server:   &boostServer{creativeRead: boostCreative(boostTestPost, "urn:li:person:abc")},
			want:     []string{"the post is authored by a LinkedIn member"},
			notWants: []string{"urn:li:person:abc"},
			cause:    ErrBoostPostIneligible,
			deletes:  1, posts: 1,
		},
		{
			name:    "a creative sponsoring another post",
			server:  &boostServer{creativeRead: boostCreative("urn:li:share:9999", org)},
			want:    []string{`it sponsors post "urn:li:share:9999", not "urn:li:share:7001"`},
			cause:   ErrBoostPostIneligible,
			deletes: 1, posts: 1,
		},
		{
			name:    "an answer that is not a boost creative",
			server:  &boostServer{creativeRead: `{"id":"SELF","content":{"reference":"urn:li:share:7001"}}`},
			want:    []string{"it sponsors no organization post"},
			cause:   ErrBoostPostIneligible,
			deletes: 1, posts: 1,
		},
		{
			name:    "a failed read",
			server:  &boostServer{creativeReadStatus: http.StatusInternalServerError},
			want:    []string{"reading it back failed", "was deleted"},
			cause:   errAnyReadFailure,
			deletes: 1, posts: 1,
		},
		{
			name:    "an expired connection on the read",
			server:  &boostServer{creativeReadStatus: http.StatusUnauthorized},
			want:    []string{"reading it back failed", "was deleted"},
			cause:   ErrCredentialsExpired,
			deletes: 1, posts: 1,
		},
		{
			name:    "an expired connection on the read and the delete",
			server:  &boostServer{creativeReadStatus: http.StatusUnauthorized, deleteStatus: http.StatusUnauthorized},
			want:    []string{"deleting the DRAFT creative urn:li:sponsoredCreative:401 failed", "delete it in Campaign Manager"},
			cause:   ErrCredentialsExpired,
			deletes: 1, posts: 1,
		},
		{
			name:    "an untrusted answer",
			server:  &boostServer{creativeRead: `{"id":"SELF","id":"urn:li:sponsoredCreative:401","content":{}}`},
			want:    []string{"could not be trusted"},
			cause:   errBoostCreativeUnchecked,
			deletes: 1, posts: 1,
		},
		{
			name:    "an answer about another creative",
			server:  &boostServer{creativeRead: other("urn:li:sponsoredCreative:9")},
			want:    []string{"describes another creative"},
			cause:   errBoostCreativeUnchecked,
			deletes: 1, posts: 1,
		},
		{
			name:    "an answer without an id",
			server:  &boostServer{creativeRead: `{"content":{"standardUpdateAd":{"contentAuthor":"urn:li:organization:987654321","reference":"urn:li:share:7001"}}}`},
			want:    []string{"describes another creative"},
			cause:   errBoostCreativeUnchecked,
			deletes: 1, posts: 1,
		},
		{
			name:    "a failed delete",
			server:  &boostServer{creativeRead: boostCreative(boostTestPost, "urn:li:organization:111"), deleteStatus: http.StatusBadRequest},
			want:    []string{"deleting the DRAFT creative urn:li:sponsoredCreative:401 failed", "delete it in Campaign Manager"},
			cause:   ErrBoostPostIneligible,
			deletes: 1, posts: 1,
		},
		{
			name:     "a creative posted on a reused campaign",
			server:   &boostServer{creativeRead: boostCreative(boostTestPost, "urn:li:organization:111"), campaignCreatives: `{"elements":[],"metadata":{}}`},
			want:     []string{"campaign 555 (found by name) is left as it was"},
			notWants: []string{"PAUSED"},
			cause:    ErrBoostPostIneligible,
			deletes:  1, posts: 1, reused: true,
		},
		{
			name: "a creative the reused campaign holds for this post",
			server: &boostServer{creativeRead: boostCreative(boostTestPost, "urn:li:organization:111"),
				existingCreatives: `{"elements":[{"id":"urn:li:sponsoredCreative:777"}],"metadata":{}}`},
			want:   []string{"creative urn:li:sponsoredCreative:777 on campaign 555 (found by name) failed the author check", "nothing was posted", "remove it in Campaign Manager"},
			cause:  ErrBoostPostIneligible,
			reused: true,
		},
		{
			// An earlier boost of another post could not delete its rejected creative; a boost of this
			// post for the same event finds the campaign by name and must not add an ad beside it.
			name: "a leftover creative of another post on a reused campaign",
			server: &boostServer{creativeRead: boostCreative("urn:li:share:9999", "urn:li:organization:111"),
				existingCreatives: `{"elements":[{"id":"urn:li:sponsoredCreative:999"}],"metadata":{}}`},
			want:   []string{"creative urn:li:sponsoredCreative:999 on campaign 555 (found by name) failed the author check", "nothing was posted"},
			cause:  ErrBoostPostIneligible,
			reused: true,
		},
		{
			// The post's lookup finds a creative the campaign-wide listing did not (another process
			// posted it in between): it is checked too, and nothing is posted or deleted.
			name: "a found creative the scan did not list",
			server: &boostServer{creativeRead: boostCreative(boostTestPost, "urn:li:organization:111"),
				campaignCreatives: `{"elements":[],"metadata":{}}`,
				existingCreatives: `{"elements":[{"id":"urn:li:sponsoredCreative:777"}],"metadata":{}}`},
			want:   []string{"creative urn:li:sponsoredCreative:777, found on campaign 555 for post", "nothing was posted"},
			cause:  ErrBoostPostIneligible,
			reused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.server
			s.postStatus = http.StatusForbidden
			if tc.reused {
				s.existingCampaign = existingBoostCampaign(t)
			}
			res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
			wantCampaign := "200"
			if tc.reused {
				wantCampaign = "555"
			}
			if res == nil || !res.BoostCreativeUnverified || res.CampaignID != wantCampaign || res.CreativeCount != 0 || res.BoostedPostURN != "" {
				t.Errorf("partial = %+v, want campaign %s marked BoostCreativeUnverified with no creative or post recorded", res, wantCampaign)
			}
			if err == nil {
				t.Fatal("CreateCampaign accepted a creative that did not pass the author check")
			}
			if tc.cause == errAnyReadFailure {
				if errors.Is(err, ErrBoostPostIneligible) {
					t.Errorf("err = %v; a failed read is not an ineligible post", err)
				}
			} else if !errors.Is(err, tc.cause) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.cause)
			}
			if tc.cause != ErrBoostPostIneligible && errors.Is(err, ErrBoostPostIneligible) {
				t.Errorf("err = %v; a creative that could not be checked is not an ineligible post", err)
			}
			if _, pre := domain.AsPreflightRefusal(err); pre {
				t.Errorf("a refusal after the campaign exists is reported as pre-flight: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to contain %q", err, want)
				}
			}
			for _, notWant := range tc.notWants {
				if strings.Contains(err.Error(), notWant) {
					t.Errorf("err = %v, want it without %q", err, notWant)
				}
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.deletes != tc.deletes || len(s.creatives) != tc.posts {
				t.Errorf("creative DELETEs %d, POSTs %d; want %d, %d", s.deletes, len(s.creatives), tc.deletes, tc.posts)
			}
		})
	}
}

// TestCreateCampaign_BoostReusedCampaignChecksItsCreatives: on a campaign found by name, every
// creative it holds is checked before one is added, whether or not the post was read. When all
// pass, the found creative for the post is reused. A check that cannot finish (a failed or
// untrusted listing, a creative that cannot be read) returns no result and posts nothing, since
// this call changed nothing; a creative checked and wrong returns the pending partial.
func TestCreateCampaign_BoostReusedCampaignChecksItsCreatives(t *testing.T) {
	const leftover = `{"elements":[{"id":"urn:li:sponsoredCreative:999"}],"metadata":{}}`
	t.Run("every creative passes, without the post read", func(t *testing.T) {
		s := &boostServer{postStatus: http.StatusForbidden, existingCampaign: existingBoostCampaign(t),
			existingCreatives: `{"elements":[{"id":"urn:li:sponsoredCreative:777"}],"metadata":{}}`}
		res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
		if err != nil {
			t.Fatalf("CreateCampaign: %v", err)
		}
		if res.CreativeCount != 1 || res.BoostedPostURN != boostTestPost || res.BoostCreativeUnverified {
			t.Errorf("result = %+v", res)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.creatives) != 0 || s.deletes != 0 {
			t.Errorf("creatives posted %d, deleted %d; want neither", len(s.creatives), s.deletes)
		}
		if len(s.queries) != 2 || strings.Contains(s.queries[0], "contentReferences") {
			t.Errorf("finder queries = %v, want the whole campaign first, then the post's creative", s.queries)
		}
	})
	t.Run("a leftover creative blocks a boost whose post was read", func(t *testing.T) {
		s := &boostServer{existingCampaign: existingBoostCampaign(t), existingCreatives: leftover,
			creativeRead: boostCreative("urn:li:share:9999", "urn:li:organization:111")}
		res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
		if !errors.Is(err, ErrBoostPostIneligible) || !strings.Contains(err.Error(), "creative urn:li:sponsoredCreative:999 on campaign 555") {
			t.Fatalf("err = %v, want the leftover named as failing the author check", err)
		}
		if res == nil || !res.BoostCreativeUnverified || res.CampaignID != "555" {
			t.Errorf("partial = %+v, want campaign 555 marked BoostCreativeUnverified", res)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.creatives) != 0 || s.deletes != 0 {
			t.Errorf("creatives posted %d, deleted %d; want neither", len(s.creatives), s.deletes)
		}
	})
	t.Run("a posted creative that could not be checked and was deleted leaves no result", func(t *testing.T) {
		s := &boostServer{postStatus: http.StatusForbidden, existingCampaign: existingBoostCampaign(t),
			campaignCreatives: `{"elements":[],"metadata":{}}`, creativeReadStatus: http.StatusInternalServerError}
		res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
		if res != nil || err == nil || !strings.Contains(err.Error(), "this call created nothing that remains") || errors.Is(err, ErrBoostPostIneligible) {
			t.Fatalf("res %+v, err %v; want no result, since the creative was deleted from a campaign this call did not create", res, err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.creatives) != 1 || s.deletes != 1 {
			t.Errorf("creatives posted %d, deleted %d; want 1, 1", len(s.creatives), s.deletes)
		}
	})
	t.Run("a posted creative that could not be checked or deleted keeps the partial", func(t *testing.T) {
		s := &boostServer{postStatus: http.StatusForbidden, existingCampaign: existingBoostCampaign(t),
			campaignCreatives: `{"elements":[],"metadata":{}}`, creativeReadStatus: http.StatusInternalServerError, deleteStatus: http.StatusInternalServerError}
		res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
		if res == nil || !res.BoostCreativeUnverified || res.CampaignID != "555" || err == nil || !strings.Contains(err.Error(), "delete it in Campaign Manager") {
			t.Fatalf("res %+v, err %v; want the pending partial naming the creative to delete", res, err)
		}
	})
	for _, tc := range []struct {
		name  string
		s     *boostServer
		cause error
	}{
		{"an expired connection", &boostServer{creativeReadStatus: http.StatusUnauthorized, existingCreatives: leftover}, ErrCredentialsExpired},
		{"a failed read", &boostServer{creativeReadStatus: http.StatusInternalServerError, existingCreatives: leftover}, nil},
		{"an untrusted answer", &boostServer{creativeRead: `{"id":"SELF","id":"x","content":{}}`, existingCreatives: leftover}, errBoostCreativeUnchecked},
		{"an untrusted listing", &boostServer{campaignCreatives: `{"elements":[{"id":"urn:li:sponsoredCreative:999"}],"elements":[],"metadata":{}}`}, nil},
	} {
		t.Run(tc.name+" posts nothing and returns no result", func(t *testing.T) {
			s := tc.s
			s.postStatus = http.StatusForbidden
			s.existingCampaign = existingBoostCampaign(t)
			res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
			if res != nil || err == nil || !strings.Contains(err.Error(), "this call created nothing and posted no creative") {
				t.Fatalf("res %+v, err %v; want no result and an error saying nothing was posted", res, err)
			}
			if errors.Is(err, ErrBoostPostIneligible) {
				t.Errorf("err = %v; a check that could not finish is not an ineligible post", err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.cause)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.creatives) != 0 || s.deletes != 0 {
				t.Errorf("creatives posted %d, deleted %d; want neither", len(s.creatives), s.deletes)
			}
		})
	}
}

// errAnyReadFailure marks a table row whose error is any read failure but an ineligible post.
var errAnyReadFailure = errors.New("any read failure")

// TestCheckPostedBoostCreative_DeleteSurvivesCancelledJob: the compensating DELETE runs detached
// from the job's context, so a job cancelled during the read-back still removes the creative.
func TestCheckPostedBoostCreative_DeleteSurvivesCancelledJob(t *testing.T) {
	s := &boostServer{creativeRead: boostCreative(boostTestPost, "urn:li:organization:111")}
	c := newBoostClient(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	undone, err := c.checkPostedBoostCreative(ctx, "123456789", "urn:li:organization:987654321", "200", boostTestPost, "urn:li:sponsoredCreative:401", false)
	if !undone {
		t.Error("undone = false, want true: the creative could not be checked and was deleted")
	}
	if err == nil {
		t.Fatal("checkPostedBoostCreative accepted a creative it could not read")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deletes != 1 {
		t.Errorf("creative DELETEs = %d, want 1 even though the job's context was cancelled", s.deletes)
	}
}

// TestCreateCampaign_BoostCreativeRefusalOnReusedCampaign: a creative refusal on a campaign found by
// name does not claim the campaign is PAUSED, since a reused campaign may be ACTIVE.
func TestCreateCampaign_BoostCreativeRefusalOnReusedCampaign(t *testing.T) {
	s := &boostServer{existingCampaign: existingBoostCampaign(t), creativeStatus: http.StatusForbidden,
		creativeBody: `{"status":403,"code":"UGC_FORBIDDEN","message":"Accessing the UGC resource is forbidden"}`}
	_, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
	if err == nil || !strings.Contains(err.Error(), "UGC_FORBIDDEN") {
		t.Fatalf("err = %v, want the UGC_FORBIDDEN refusal", err)
	}
	if strings.Contains(err.Error(), "PAUSED") {
		t.Errorf("err = %v; a reused campaign is not known to be PAUSED", err)
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
	// The campaign-wide check first (every reused campaign), then the post's lookup.
	if len(s.queries) != 2 || strings.Contains(s.queries[0], "contentReferences") ||
		!strings.Contains(s.queries[1], "contentReferences=List(urn%3Ali%3Ashare%3A7001)") ||
		!strings.Contains(s.queries[1], "campaigns=List(urn%3Ali%3AsponsoredCampaign%3A555)") {
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
	if len(s.queries) != 2 || len(s.creatives) != 1 || s.creatives[0]["campaign"] != "urn:li:sponsoredCampaign:555" {
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

// TestCreateCampaign_BoostUntrustedFinderPostsNothing: a creatives-finder page identityjson refuses
// (a repeated key whose last value is an empty list, or malformed UTF-8) is a failed lookup, not
// an absence: no creative is posted, and the create returns no result, so the claim is released
// and a retry looks again.
func TestCreateCampaign_BoostUntrustedFinderPostsNothing(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate elements": `{"elements":[{"id":"urn:li:sponsoredCreative:777"}],"elements":[],"metadata":{}}`,
		"invalid utf-8":      "{\"elements\":[],\"metadata\":{},\"note\":\"\xff\"}",
	} {
		t.Run(name, func(t *testing.T) {
			// The campaign-wide check passes (a trusted empty listing), so the post's lookup is what fails.
			s := &boostServer{existingCampaign: existingBoostCampaign(t), existingCreatives: body, campaignCreatives: `{"elements":[],"metadata":{}}`}
			res, err := newBoostClient(t, s).CreateCampaign(context.Background(), boostInput())
			if res != nil || err == nil || !strings.Contains(err.Error(), "could not be trusted") ||
				!strings.Contains(err.Error(), "repeats the lookup") {
				t.Fatalf("res %+v, err %v; want no result and an untrusted-lookup error", res, err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.queries) != 2 || len(s.creatives) != 0 {
				t.Errorf("queries %v, creatives %v; want the check, one lookup and no creative POST", s.queries, s.creatives)
			}
		})
	}
}

// TestCreateCampaign_BoostLookupFailureThenRetry (#680 review): a failed lookup on a reused campaign
// returns no partial (one would be recorded as created_degraded and reused as complete, or as
// pending and refused), and the next create of the same input finds the campaign again and
// repeats the lookup: a creative the first attempt could not see is reused, none is posted.
func TestCreateCampaign_BoostLookupFailureThenRetry(t *testing.T) {
	s := &boostServer{existingCampaign: existingBoostCampaign(t), finderStatus: http.StatusInternalServerError}
	c := newBoostClient(t, s)
	res, err := c.CreateCampaign(context.Background(), boostInput())
	if res != nil || err == nil || !strings.Contains(err.Error(), "this call created nothing") {
		t.Fatalf("first create: res %+v, err %v; want no result", res, err)
	}

	s.mu.Lock()
	s.finderStatus = 0
	s.existingCreatives = `{"elements":[{"id":"urn:li:sponsoredCreative:777"}],"metadata":{}}`
	s.mu.Unlock()
	res, err = c.CreateCampaign(context.Background(), boostInput())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.CampaignID != "555" || res.CreativeCount != 1 || res.BoostedPostURN != boostTestPost {
		t.Errorf("retry result = %+v", res)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The first create's campaign-wide check fails at the finder; the retry checks, then looks up.
	if len(s.queries) != 3 || len(s.creatives) != 0 {
		t.Errorf("queries %v, creatives %v; want three finder reads and no creative POST", s.queries, s.creatives)
	}
}

// TestCreateCampaign_BoostConcurrentSameNameCreatesOneCreative (#680 review): two creates of the
// same campaign name and post, from different briefs in this process, post one creative. The
// event's group lock is held through finishBoost, so the second create finds the first one's
// group and campaign and then its creative; released after the campaign POST, the second would
// look up while the first's creative POST is still in flight (creativeDelay) and post a second ad.
func TestCreateCampaign_BoostConcurrentSameNameCreatesOneCreative(t *testing.T) {
	s := &boostServer{stateful: true, creativeDelay: 150 * time.Millisecond}
	first := newBoostClient(t, s)
	second := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(first.baseURL), WithClock(fixedClock()))

	var wg sync.WaitGroup
	results := make([]*CampaignResult, 2)
	errs := make([]error, 2)
	for i, c := range []*Client{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = c.CreateCampaign(context.Background(), boostInput())
		}()
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("create %d: %v", i, errs[i])
		}
		if results[i].CampaignID != "200" || results[i].CreativeCount != 1 || results[i].BoostedPostURN != boostTestPost {
			t.Errorf("create %d result = %+v", i, results[i])
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.creatives) != 1 {
		t.Errorf("creative POSTs = %d, want 1: %v", len(s.creatives), s.creatives)
	}
	if len(s.queries) != 2 {
		t.Errorf("creatives finder queries = %v, want two (the second create's check and lookup)", s.queries)
	}
}

// TestBoostCreativeRefusal covers every branch of the creative-refusal sentence: the top-level
// code, an input error's and a conditional input error's code, each listed code, an unlisted code
// (never quoted), an envelope identityjson refuses, a non-JSON body, and a non-API error.
func TestBoostCreativeRefusal(t *testing.T) {
	const post, account = "urn:li:share:7", "509430019"
	apiErr := func(raw string) error { return &apiError{StatusCode: http.StatusBadRequest, raw: []byte(raw)} }
	tests := []struct {
		name string
		err  error
		want string // "" means no sentence; otherwise the code the sentence must name
	}{
		{"top-level UGC_FORBIDDEN", apiErr(`{"status":403,"code":"UGC_FORBIDDEN"}`), "UGC_FORBIDDEN"},
		{"input error reshare", apiErr(`{"status":400,"errorDetails":{"inputErrors":[{"code":"UGC_RESHARE_CANNOT_BE_SPONSORED"}]}}`), "UGC_RESHARE_CANNOT_BE_SPONSORED"},
		{"conditional input error visibility", apiErr(`{"status":400,"errorDetails":{"conditionalInputErrors":[{"code":"UGC_INVALID_VISIBILITY_SETTING"}]}}`), "UGC_INVALID_VISIBILITY_SETTING"},
		{"input error feed distribution", apiErr(`{"status":400,"errorDetails":{"inputErrors":[{"code":"OTHER"},{"code":"UGC_INVALID_FEED_DISTRIBUTION"}]}}`), "UGC_INVALID_FEED_DISTRIBUTION"},
		{"unlisted code is not quoted", apiErr(`{"status":400,"code":"SOMETHING_ELSE"}`), ""},
		{"duplicate key envelope is refused", apiErr(`{"code":"UGC_FORBIDDEN","code":"UGC_FORBIDDEN"}`), ""},
		{"non-JSON body", apiErr(`<html>bad gateway</html>`), ""},
		{"empty body", apiErr(``), ""},
		{"not an API error", errors.New("transport"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boostCreativeRefusal(tt.err, post, account)
			if tt.want == "" {
				if got != "" {
					t.Fatalf("boostCreativeRefusal = %q, want no sentence", got)
				}
				return
			}
			if !strings.Contains(got, "("+tt.want+")") || !strings.Contains(got, boostCreativeCodes[tt.want]) || !strings.Contains(got, account) {
				t.Fatalf("boostCreativeRefusal = %q, want the %s sentence for account %s", got, tt.want, account)
			}
		})
	}
}

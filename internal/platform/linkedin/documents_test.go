// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
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

// testPDF is a small PDF-headed document.
var testPDF = []byte("%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")

func TestValidateDocument(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   DocumentUpload
		want string // "" accepted
	}{
		{"pdf", DocumentUpload{Data: testPDF, MimeType: "application/pdf"}, ""},
		{"empty", DocumentUpload{MimeType: "application/pdf"}, "no bytes"},
		{"png type", DocumentUpload{Data: testPDF, MimeType: "image/png"}, "only PDF documents"},
		{"not pdf bytes", DocumentUpload{Data: []byte("hello world"), MimeType: "application/pdf"}, "not a PDF file"},
		{"over 100 MB", DocumentUpload{Data: append(append([]byte{}, testPDF...), make([]byte, MaxDocumentBytes)...), MimeType: "application/pdf"}, "at most 100000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDocument(tc.in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrDocumentInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want ErrDocumentInvalid containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateDocumentFields(t *testing.T) {
	for _, tc := range []struct {
		name, asset, title, intro, want string
	}{
		{"valid", "a1", "KubeCon 2099 sponsor prospectus", "Download the prospectus", ""},
		{"title at 70", "a1", strings.Repeat("t", 70), "intro", ""},
		{"title at 71", "a1", strings.Repeat("t", 71), "intro", "documentTitle is 71 characters"},
		{"intro at 600", "a1", "title", strings.Repeat("i", 600), ""},
		{"intro at 601", "a1", "title", strings.Repeat("i", 601), "documentIntroText is 601 characters"},
		{"no asset", " ", "title", "intro", "requires documentAssetId"},
		{"dash-only title", "a1", "—", "intro", "documentTitle is required"},
		{"no intro", "a1", "title", "", "documentIntroText is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDocumentFields(tc.asset, tc.title, tc.intro)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrFormatInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want ErrFormatInvalid containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestDocumentObjective pins decision 2026-11-25: ENGAGEMENT by default, LEAD_GENERATION as asked,
// and no Website Visits or Website Conversions; ENGAGEMENT is automatic MAX_REACH only.
func TestDocumentObjective(t *testing.T) {
	for in, want := range map[string]string{"": ObjectiveEngagement, ObjectiveEngagement: ObjectiveEngagement, ObjectiveLeadGeneration: ObjectiveLeadGeneration} {
		if got, err := documentObjective(in); err != nil || got != want {
			t.Errorf("documentObjective(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{ObjectiveWebsiteVisit, ObjectiveWebsiteConversion} {
		if _, err := documentObjective(in); !errors.Is(err, ErrFormatInvalid) {
			t.Errorf("documentObjective(%q) = %v, want ErrFormatInvalid", in, err)
		}
	}
	plan, err := campaignBidding(0, ObjectiveEngagement)
	if err != nil || plan.objectiveType != ObjectiveEngagement || plan.optimizationTarget != OptimizationTargetMaxReach ||
		plan.fields["costType"] != costTypeCPM || plan.nameObjective != "Engagement" {
		t.Errorf("campaignBidding(engagement) = %+v, %v", plan, err)
	}
	if _, err := campaignBidding(5, ObjectiveEngagement); err == nil {
		t.Error("a manual bid was accepted for ENGAGEMENT")
	}
}

// documentServer records every request of a document create.
type documentServer struct {
	mu         sync.Mutex
	calls      []string
	campaign   map[string]any
	initBody   map[string]any
	put        []byte
	putType    string
	posts      []map[string]any
	creatives  []map[string]any
	statuses   []string // successive GET /documents/{urn} answers; the last repeats
	statusCall int
	existing   string // non-empty: the campaign-search answer
}

func (s *documentServer) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/dms-uploads/"):
			s.put, s.putType = raw, r.Header.Get("Content-Type")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/documents":
			s.initBody = body
			_, _ = io.WriteString(w, `{"value":{"uploadUrlExpiresAt":1,"uploadUrl":"http://`+r.Host+`/dms-uploads/DOC1/0?ut=sig","document":"urn:li:document:DOC1"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/documents/"):
			st := "AVAILABLE"
			if len(s.statuses) > 0 {
				st = s.statuses[min(s.statusCall, len(s.statuses)-1)]
			}
			s.statusCall++
			_, _ = io.WriteString(w, `{"status":"`+st+`"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/leadForms/"):
			_, _ = io.WriteString(w, `{"id":123,"name":"Prospectus","state":"PUBLISHED","owner":{"sponsoredAccount":"urn:li:sponsoredAccount:123456789"}}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaigns") && s.existing != "":
			_, _ = io.WriteString(w, s.existing)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			s.campaign = body
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			s.posts = append(s.posts, body)
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			s.creatives = append(s.creatives, body)
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return NewClient(Credentials{AccessToken: "tok-1"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()),
		WithDocumentPolling(time.Millisecond, 200*time.Millisecond), withRetryBaseDelay(time.Millisecond))
}

func documentInput() CampaignInput {
	in := imageCampaignInput(nil)
	in.Format = FormatDocument
	in.Document = &DocumentUpload{Data: testPDF, MimeType: "application/pdf"}
	in.DocumentAssetID = "asset-doc"
	in.DocumentTitle = "KubeCon 2099 sponsor prospectus"
	in.DocumentIntroText = "Sponsor KubeCon 2099"
	return in
}

// TestCreateCampaign_DocumentWireBodies pins every LinkedIn body of a default document create: an
// ENGAGEMENT / MAX_REACH / CPM campaign sent WITHOUT a format and named Engagement and Document, one
// Documents API upload (initializeUpload owned by the organization, the PDF PUT, the status poll),
// ONE dark post carrying content.media and no landing page, and ONE creative referencing it.
func TestCreateCampaign_DocumentWireBodies(t *testing.T) {
	s := &documentServer{statuses: []string{"PROCESSING", "AVAILABLE"}}
	res, err := s.client(t).CreateCampaign(context.Background(), documentInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CreativeCount != 1 || res.ObjectiveType != ObjectiveEngagement || res.OptimizationTarget != OptimizationTargetMaxReach ||
		res.DocumentURNs["asset-doc"] != "urn:li:document:DOC1" {
		t.Errorf("result = %+v", res)
	}
	c := s.campaign
	if c["objectiveType"] != ObjectiveEngagement || c["optimizationTargetType"] != OptimizationTargetMaxReach || c["costType"] != "CPM" ||
		c["type"] != "SPONSORED_UPDATES" || c["offsiteDeliveryEnabled"] != false {
		t.Errorf("campaign body = %v", c)
	}
	if _, set := c["format"]; set {
		t.Errorf("a document campaign sent format %v; LinkedIn takes it from the first creative", c["format"])
	}
	if name, _ := c["name"].(string); !strings.Contains(name, " | Engagement | ") || !strings.Contains(name, " | Document | ") {
		t.Errorf("campaign name = %q, want the Engagement objective and Document format segments", name)
	}
	req, _ := s.initBody["initializeUploadRequest"].(map[string]any)
	if req["owner"] != "urn:li:organization:987654321" {
		t.Errorf("initializeUploadRequest = %v, want the organization owner", req)
	}
	if !bytes.Equal(s.put, testPDF) || s.putType != "application/pdf" {
		t.Errorf("PUT %d bytes as %q, want the PDF as application/pdf", len(s.put), s.putType)
	}
	if s.statusCall != 2 {
		t.Errorf("status polls = %d, want 2", s.statusCall)
	}
	if len(s.posts) != 1 || len(s.creatives) != 1 {
		t.Fatalf("posts %d, creatives %d; want 1 and 1", len(s.posts), len(s.creatives))
	}
	post := s.posts[0]
	content, _ := post["content"].(map[string]any)
	media, _ := content["media"].(map[string]any)
	if media["id"] != "urn:li:document:DOC1" || media["title"] != "KubeCon 2099 sponsor prospectus" || post["commentary"] != "Sponsor KubeCon 2099" {
		t.Errorf("post = %v", post)
	}
	if _, set := post["contentLandingPage"]; set {
		t.Error("a document post carries a landing page")
	}
	if dist, _ := post["distribution"].(map[string]any); dist["feedDistribution"] != "NONE" {
		t.Errorf("not a dark post: %v", dist)
	}
	cr := s.creatives[0]
	// A document creative nests the post under content.documentAd ("Document Ads API").
	content2, _ := cr["content"].(map[string]any)
	docAd, _ := content2["documentAd"].(map[string]any)
	if docAd["reference"] != "urn:li:share:300" || content2["reference"] != nil || cr["intendedStatus"] != "DRAFT" {
		t.Errorf("creative = %v, want DRAFT with content.documentAd.reference = the post and no content.reference", cr)
	}
	if name, _ := cr["name"].(string); name != "KubeCon | document-1" {
		t.Errorf("creative name = %q", name)
	}
	if _, set := cr["leadgenCallToAction"]; set {
		t.Error("an engagement document creative carries a lead gen form")
	}
	// Order: campaign, then the upload, then the post.
	idx := func(prefix string) int {
		for i, c := range s.calls {
			if strings.HasPrefix(c, prefix) {
				return i
			}
		}
		return -1
	}
	if idx("POST /adAccounts/123456789/adCampaigns") >= idx("POST /documents") || idx("POST /documents") >= idx("PUT /dms-uploads/") ||
		idx("GET /documents/") >= idx("POST /posts") {
		t.Errorf("call order = %v", s.calls)
	}
}

// TestCreateCampaign_DocumentLeadGen: objective LEAD_GENERATION with a form makes a gated document:
// a LEAD_GENERATION / MAX_LEAD campaign whose creative carries the form.
func TestCreateCampaign_DocumentLeadGen(t *testing.T) {
	s := &documentServer{}
	in := documentInput()
	in.Objective = ObjectiveLeadGeneration
	in.LeadGen = &LeadGen{FormID: "123"}
	res, err := s.client(t).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if s.campaign["objectiveType"] != ObjectiveLeadGeneration || s.campaign["optimizationTargetType"] != OptimizationTargetMaxLead || res.CreativeCount != 1 {
		t.Errorf("campaign %v, result %+v", s.campaign, res)
	}
	content, _ := s.creatives[0]["content"].(map[string]any)
	if docAd, _ := content["documentAd"].(map[string]any); docAd["reference"] != "urn:li:share:300" {
		t.Errorf("gated creative content = %v, want content.documentAd.reference", content)
	}
	cta, _ := s.creatives[0]["leadgenCallToAction"].(map[string]any)
	if cta["destination"] != "urn:li:adForm:123" {
		t.Errorf("creative leadgenCallToAction = %v, want the form", s.creatives[0]["leadgenCallToAction"])
	}
}

// TestCreateCampaign_DocumentNeedsNoRegistrationURL: a document post carries no landing page, so a
// brief without a registration URL still creates a document ad (engagement or gated), while every
// other format still refuses one before any request.
func TestCreateCampaign_DocumentNeedsNoRegistrationURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*CampaignInput)
		refused bool
	}{
		{"engagement document", func(*CampaignInput) {}, false},
		{"gated document", func(in *CampaignInput) {
			in.Objective, in.LeadGen = ObjectiveLeadGeneration, &LeadGen{FormID: "123"}
		}, false},
		{"single image still requires it", func(in *CampaignInput) {
			*in = imageCampaignInput(nil, CreativeVariant{IntroText: "i", Headline: "h"})
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &documentServer{}
			in := documentInput()
			tc.edit(&in)
			in.RegistrationURL = ""
			res, err := s.client(t).CreateCampaign(context.Background(), in)
			if tc.refused {
				if err == nil || res != nil || !strings.Contains(err.Error(), "registration URL is required") || len(s.calls) != 0 {
					t.Fatalf("res=%v err=%v calls=%v; want the registration URL refusal before any request", res, err, s.calls)
				}
				return
			}
			if err != nil || res == nil || res.CreativeCount != 1 {
				t.Fatalf("res=%+v err=%v; want a created document ad", res, err)
			}
		})
	}
}

// TestCreateCampaign_DocumentRefusedBeforeAnyRequest: every document rule the client owns is decided
// before the first LinkedIn request.
func TestCreateCampaign_DocumentRefusedBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*CampaignInput)
		want string
	}{
		{"website visits", func(in *CampaignInput) { in.Objective = ObjectiveWebsiteVisit }, "LinkedIn offers document ads no WEBSITE_VISIT"},
		{"website conversions", func(in *CampaignInput) { in.Objective = ObjectiveWebsiteConversion }, "LinkedIn offers document ads no WEBSITE_CONVERSION"},
		{"manual bid", func(in *CampaignInput) { in.ManualCPCBid = 5 }, "automatic bidding"},
		{"engagement without a document", func(in *CampaignInput) {
			*in = imageCampaignInput(nil, CreativeVariant{IntroText: "i", Headline: "h"})
			in.Objective = ObjectiveEngagement
		}, "created for format \"document\" only"},
		{"variants beside the document", func(in *CampaignInput) { in.Variants = []CreativeVariant{{IntroText: "i", Headline: "h"}} }, "not variants"},
		{"document not supplied", func(in *CampaignInput) { in.Document = nil }, "which was not supplied"},
		{"not a pdf", func(in *CampaignInput) { in.Document = &DocumentUpload{Data: testPDF, MimeType: "image/png"} }, "only PDF documents"},
		{"title over 70", func(in *CampaignInput) { in.DocumentTitle = strings.Repeat("t", 71) }, "documentTitle is 71 characters"},
		{"document fields under another format", func(in *CampaignInput) {
			in.Format = ""
			in.Variants = []CreativeVariant{{IntroText: "i", Headline: "h"}}
		}, "accepted only with format \"document\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &documentServer{}
			in := documentInput()
			tc.edit(&in)
			res, err := s.client(t).CreateCampaign(context.Background(), in)
			if err == nil || res != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got res=%v err=%v; want nil and an error containing %q", res, err, tc.want)
			}
			if n := len(s.calls); n != 0 {
				t.Errorf("%d request(s) sent: %v", n, s.calls)
			}
		})
	}
}

// TestCreateCampaign_DocumentProcessingFailureKeepsThePartial: a document LinkedIn fails to process
// after the campaign exists returns the partial (campaign id, zero creatives, the minted URN), and
// no dark post is created.
func TestCreateCampaign_DocumentProcessingFailureKeepsThePartial(t *testing.T) {
	s := &documentServer{statuses: []string{"PROCESSING", "PROCESSING_FAILED"}}
	res, err := s.client(t).CreateCampaign(context.Background(), documentInput())
	if err == nil || !strings.Contains(err.Error(), "PROCESSING_FAILED") {
		t.Fatalf("err = %v, want the processing failure", err)
	}
	if res == nil || res.CampaignID != "200" || res.CreativeCount != 0 || res.DocumentURNs["asset-doc"] != "urn:li:document:DOC1" {
		t.Fatalf("partial = %+v; want campaign 200, no creative, the document URN", res)
	}
	if len(s.posts) != 0 {
		t.Errorf("%d dark post(s) created after the document failed", len(s.posts))
	}
}

// TestFindCampaignByNameInGroup_DocumentIsItsOwnFormat: a same-name image campaign is never reused
// for a document, nor a document campaign for an image create.
func TestFindCampaignByNameInGroup_DocumentIsItsOwnFormat(t *testing.T) {
	for _, tc := range []struct {
		name, existing, want string
		reuse                bool
	}{
		{"document reuses document", campaignFormatDocument, campaignFormatDocument, true},
		{"document reuses its own campaign before the creative", campaignFormatUnsupported, campaignFormatDocument, true},
		{"document refuses an image campaign", "STANDARD_UPDATE", campaignFormatDocument, false},
		{"image refuses a document campaign", campaignFormatDocument, "", false},
		{"carousel refuses a document campaign", campaignFormatDocument, campaignFormatCarousel, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"elements":[{"name":"N","status":"PAUSED","id":555,"type":"SPONSORED_UPDATES","format":"`+tc.existing+`","campaignGroup":"urn:li:sponsoredCampaignGroup:100"}],"metadata":{}}`)
			}))
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			id, err := c.findCampaignByNameInGroup(context.Background(), "adAccounts/123456789/adCampaigns", "N", "100", "", "", campaignTypeSponsoredUpdates, tc.want)
			if tc.reuse != (err == nil && id == "555") {
				t.Fatalf("id %q, err %v; want reuse=%v", id, err, tc.reuse)
			}
		})
	}
}

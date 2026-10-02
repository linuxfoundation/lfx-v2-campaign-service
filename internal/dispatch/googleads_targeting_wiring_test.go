// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// The dispatch-layer half of LFXV2-2665: every targeting, extension and ad-group
// field googleAdsConfig gained must actually reach the outbound requests. The
// platform client's own tests prove each feature builds the right payload FROM a
// CampaignInput; what they cannot prove is that the wire config is mapped onto that
// input at all. A dropped or mistyped field in googleads.go leaves every one of
// those tests green while the feature is unreachable through the API.

// targetingCapture records the cascade bodies per endpoint. Unlike geoCapture it
// ACCUMULATES: the Search path now sends up to three separate campaignCriteria
// mutates (locations, negative keywords, then the language/schedule/device/
// demographic criteria), and keeping only the last would silently assert against
// whichever happened to run last.
type targetingCapture struct {
	mu               sync.Mutex
	campaignCriteria [][]byte
	adGroups         [][]byte
	adGroupAds       [][]byte
	assets           []byte
	campaignAssets   []byte
	sawBudget        bool
}

func (c *targetingCapture) add(dst *[][]byte, body []byte) {
	c.mu.Lock()
	*dst = append(*dst, body)
	c.mu.Unlock()
}

// campaignCriteriaWith returns the first captured campaignCriteria body containing
// the given JSON key, so a test asserting on (say) the language criteria is not
// satisfied by the location mutate that shares the endpoint.
func (c *targetingCapture) campaignCriteriaWith(key string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, body := range c.campaignCriteria {
		if strings.Contains(string(body), `"`+key+`"`) {
			return body
		}
	}
	return nil
}

// targetingServers serves the whole Search create cascade, echoing ONE result per
// requested operation everywhere — the client reads any other count as UNCONFIRMED,
// so a fixed-size fake would fail whichever test's operation count it did not match
// and would hide a genuine count mismatch. Handlers never call t.Fatal: each runs on
// its own goroutine, where FailNow is invalid (see test-hygiene.md).
func targetingServers(t *testing.T) ([]googleads.Option, *targetingCapture) {
	t.Helper()
	cap := &targetingCapture{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	var adGroupSeq int
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			_, _ = io.WriteString(w, `{"results":[]}`)
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			cap.mu.Lock()
			cap.sawBudget = true
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			cap.add(&cap.adGroups, body)
			cap.mu.Lock()
			adGroupSeq++
			id := strconv.Itoa(330 + adGroupSeq - 1)
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/`+id+`"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			cap.add(&cap.adGroupAds, body)
			out, err := adGroupAdResults(body)
			if err != nil {
				t.Errorf("decode adGroupAds request: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, out)
		case strings.HasSuffix(r.URL.Path, "campaignCriteria:mutate"):
			cap.add(&cap.campaignCriteria, body)
			out, err := criteriaResultsOrErr(body, "campaignCriteria", "222")
			if err != nil {
				t.Errorf("decode campaignCriteria request: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, out)
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			out, err := adGroupCriteriaResults(body)
			if err != nil {
				t.Errorf("decode adGroupCriteria request: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, out)
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			cap.mu.Lock()
			cap.assets = body
			cap.mu.Unlock()
			out, err := assetResults(body)
			if err != nil {
				t.Errorf("decode assets request: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, out)
		case strings.HasSuffix(r.URL.Path, "campaignAssets:mutate"):
			cap.mu.Lock()
			cap.campaignAssets = body
			cap.mu.Unlock()
			out, err := campaignAssetResults(body)
			if err != nil {
				t.Errorf("decode campaignAssets request: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, out)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return []googleads.Option{googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL)}, cap
}

// adGroupAdResults echoes one adGroupAd per operation, naming the ad group the
// operation itself asked for. Returning a fixed group would make every multi-group
// test fail as "a different ad group id" — the client checks that the returned
// composite id names the group it just created.
func adGroupAdResults(body []byte) (string, error) {
	var req struct {
		Operations []struct {
			Create struct {
				AdGroup string `json:"adGroup"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(req.Operations))
	for i, op := range req.Operations {
		group := op.Create.AdGroup[strings.LastIndex(op.Create.AdGroup, "/")+1:]
		parts = append(parts, fmt.Sprintf(`{"resourceName":"customers/1234567890/adGroupAds/%s~%d"}`, group, 440+i))
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`, nil
}

// adGroupCriteriaResults echoes one criterion per operation, parented on the ad
// group the operation itself names. The shared criteriaResultsOrErr helper takes a
// FIXED parent, which cannot serve a multi-group cascade: the client checks that
// each returned composite id names the group it just created, so a fixed parent
// makes every group after the first read as UNCONFIRMED.
func adGroupCriteriaResults(body []byte) (string, error) {
	var req struct {
		Operations []struct {
			Create struct {
				AdGroup string `json:"adGroup"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(req.Operations))
	for i, op := range req.Operations {
		group := op.Create.AdGroup[strings.LastIndex(op.Create.AdGroup, "/")+1:]
		parts = append(parts, fmt.Sprintf(`{"resourceName":"customers/1234567890/adGroupCriteria/%s~%d"}`, group, 901+i))
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`, nil
}

func assetResults(body []byte) (string, error) {
	var req struct {
		Operations []json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(req.Operations))
	for i := range req.Operations {
		parts = append(parts, fmt.Sprintf(`{"resourceName":"customers/1234567890/assets/%d"}`, 700+i))
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`, nil
}

// campaignAssetResults echoes the three-part {campaignId}~{assetId}~{fieldType}
// shape, reading the field type back out of the operation so the link results
// describe what was actually asked for.
func campaignAssetResults(body []byte) (string, error) {
	var req struct {
		Operations []struct {
			Create struct {
				Asset     string `json:"asset"`
				FieldType string `json:"fieldType"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(req.Operations))
	for _, op := range req.Operations {
		assetID := op.Create.Asset[strings.LastIndex(op.Create.Asset, "/")+1:]
		parts = append(parts, fmt.Sprintf(`{"resourceName":"customers/1234567890/campaignAssets/222~%s~%s"}`, assetID, op.Create.FieldType))
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`, nil
}

// criterionOps decodes a captured campaignCriteria body into the full criterion
// shape, so a test can assert on whichever oneof arm it cares about.
func criterionOps(t *testing.T, body []byte) []criterionOp {
	t.Helper()
	var req struct {
		Operations []struct {
			Create criterionOp `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode criteria body: %v (body=%s)", err, body)
	}
	out := make([]criterionOp, 0, len(req.Operations))
	for _, op := range req.Operations {
		out = append(out, op.Create)
	}
	return out
}

type criterionOp struct {
	Campaign    string   `json:"campaign"`
	Negative    bool     `json:"negative"`
	BidModifier *float64 `json:"bidModifier"`
	Location    *struct {
		GeoTargetConstant string `json:"geoTargetConstant"`
	} `json:"location"`
	Proximity *struct {
		GeoPoint struct {
			LatitudeInMicroDegrees  int64 `json:"latitudeInMicroDegrees"`
			LongitudeInMicroDegrees int64 `json:"longitudeInMicroDegrees"`
		} `json:"geoPoint"`
		Radius      float64 `json:"radius"`
		RadiusUnits string  `json:"radiusUnits"`
	} `json:"proximity"`
	Language *struct {
		LanguageConstant string `json:"languageConstant"`
	} `json:"language"`
	AdSchedule *struct {
		DayOfWeek   string `json:"dayOfWeek"`
		StartHour   int    `json:"startHour"`
		StartMinute string `json:"startMinute"`
		EndHour     int    `json:"endHour"`
		EndMinute   string `json:"endMinute"`
	} `json:"adSchedule"`
	Device *struct {
		Type string `json:"type"`
	} `json:"device"`
	AgeRange *struct {
		Type string `json:"type"`
	} `json:"ageRange"`
	Gender *struct {
		Type string `json:"type"`
	} `json:"gender"`
}

// Geo depth: an exclusion and a radius target must reach the SAME location mutate
// the inclusions use, carrying the negative flag and the microdegree conversion.
func TestGoogleAds_GeoDepthConfigReachesCampaignCriteria(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{
		"budget":50,
		"geoTargets":["GB","1023191"],
		"excludedGeoTargets":["IN"],
		"proximityTargets":[{"latitude":37.7749,"longitude":-122.4194,"radius":25,"radiusUnit":"MILES"}]
	}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	body := cap.campaignCriteriaWith("location")
	if body == nil {
		t.Fatal("no location criteria were sent — the geo config never reached the client")
	}
	ops := criterionOps(t, body)
	if len(ops) != 4 {
		t.Fatalf("got %d location operations, want 4 (2 included + 1 excluded + 1 proximity): %s", len(ops), body)
	}
	// Included first, in caller order, with the raw constant id passed through as
	// the city-level spelling it is.
	for i, want := range []string{"geoTargetConstants/2826", "geoTargetConstants/1023191"} {
		if ops[i].Location == nil || ops[i].Location.GeoTargetConstant != want {
			t.Errorf("operation %d: got %+v, want %q", i, ops[i].Location, want)
		}
		if ops[i].Negative {
			t.Errorf("operation %d is an INCLUSION and must not carry negative:true", i)
		}
	}
	if ops[2].Location == nil || ops[2].Location.GeoTargetConstant != "geoTargetConstants/2356" {
		t.Errorf("excluded operation = %+v, want geoTargetConstants/2356 (IN)", ops[2].Location)
	}
	if !ops[2].Negative {
		t.Error("cfg.ExcludedGeoTargets must produce a NEGATIVE criterion; an exclusion sent as a positive buys the traffic it meant to refuse")
	}
	prox := ops[3].Proximity
	if prox == nil {
		t.Fatalf("operation 3 carries no proximity: %s", body)
	}
	if prox.GeoPoint.LatitudeInMicroDegrees != 37774900 || prox.GeoPoint.LongitudeInMicroDegrees != -122419400 {
		t.Errorf("proximity point = %+v, want the decimal degrees converted to microdegrees", prox.GeoPoint)
	}
	if prox.Radius != 25 || prox.RadiusUnits != "MILES" {
		t.Errorf("proximity radius = %v %q, want 25 MILES", prox.Radius, prox.RadiusUnits)
	}
}

// Language / schedule / device / demographics: all four reach the campaign-level
// criteria mutate, in the client's documented order, with the bid-modifier pointer
// preserved through the wire type.
func TestGoogleAds_CriteriaConfigReachesCampaignCriteria(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{
		"budget":50,
		"languages":["EN","DE"],
		"adSchedules":[{"dayOfWeek":"monday","startHour":9,"startMinute":30,"endHour":17,"endMinute":0,"bidModifier":1.2}],
		"deviceBidModifiers":[{"device":"TABLET","bidModifier":0}],
		"excludedAgeRanges":["18-24"],
		"excludedGenders":["UNDETERMINED"]
	}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	body := cap.campaignCriteriaWith("language")
	if body == nil {
		t.Fatal("no targeting criteria were sent — the language/schedule/device/demographic config never reached the client")
	}
	ops := criterionOps(t, body)
	if len(ops) != 6 {
		t.Fatalf("got %d criteria, want 6 (2 languages + 1 schedule + 1 device + 1 age + 1 gender): %s", len(ops), body)
	}
	for i, want := range []string{"languageConstants/1000", "languageConstants/1001"} {
		if ops[i].Language == nil || ops[i].Language.LanguageConstant != want {
			t.Errorf("operation %d: got %+v, want language %q", i, ops[i].Language, want)
		}
	}
	sched := ops[2].AdSchedule
	if sched == nil {
		t.Fatalf("operation 2 carries no adSchedule: %s", body)
	}
	if sched.DayOfWeek != "MONDAY" || sched.StartHour != 9 || sched.StartMinute != "THIRTY" || sched.EndHour != 17 || sched.EndMinute != "ZERO" {
		t.Errorf("ad schedule = %+v, want MONDAY 09:30–17:00 with Google's minute enums", sched)
	}
	if ops[2].BidModifier == nil || *ops[2].BidModifier != 1.2 {
		t.Errorf("ad schedule bid modifier = %v, want 1.2 carried through the pointer", ops[2].BidModifier)
	}
	if ops[3].Device == nil || ops[3].Device.Type != "TABLET" {
		t.Errorf("device criterion = %+v, want TABLET", ops[3].Device)
	}
	// The whole reason both sides use a pointer: an explicit 0 is the -100% opt-out
	// and must survive as 0, not be dropped as an empty value.
	if ops[3].BidModifier == nil || *ops[3].BidModifier != 0 {
		t.Errorf("device bid modifier = %v, want an explicit 0 (the do-not-serve opt-out)", ops[3].BidModifier)
	}
	if ops[4].AgeRange == nil || ops[4].AgeRange.Type != "AGE_RANGE_18_24" {
		t.Errorf("age range criterion = %+v, want AGE_RANGE_18_24", ops[4].AgeRange)
	}
	if !ops[4].Negative || !ops[5].Negative {
		t.Error("demographic criteria are EXCLUSIONS and must carry negative:true")
	}
	if ops[5].Gender == nil || ops[5].Gender.Type != "UNDETERMINED" {
		t.Errorf("gender criterion = %+v, want UNDETERMINED", ops[5].Gender)
	}
}

// Ad extensions: all three kinds reach assets:mutate in one batch and are then
// linked to the campaign with the right field types.
func TestGoogleAds_ExtensionConfigReachesAssets(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{
		"budget":50,
		"sitelinks":[{"text":"Register","description1":"Save your seat","description2":"Early bird pricing","finalUrl":"https://events.example/kc/register"}],
		"callouts":["Free workshops"],
		"structuredSnippets":[{"header":"Courses","values":["Kubernetes","Observability","Security"]}]
	}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	assetBody, linkBody := cap.assets, cap.campaignAssets
	cap.mu.Unlock()
	if assetBody == nil {
		t.Fatal("no assets:mutate request was sent — the extension config never reached the client")
	}

	var assetReq struct {
		Operations []struct {
			Create struct {
				FinalURLs     []string `json:"finalUrls"`
				SitelinkAsset *struct {
					LinkText     string `json:"linkText"`
					Description1 string `json:"description1"`
					Description2 string `json:"description2"`
				} `json:"sitelinkAsset"`
				CalloutAsset *struct {
					CalloutText string `json:"calloutText"`
				} `json:"calloutAsset"`
				StructuredSnippetAsset *struct {
					Header string   `json:"header"`
					Values []string `json:"values"`
				} `json:"structuredSnippetAsset"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(assetBody, &assetReq); err != nil {
		t.Fatalf("decode assets body: %v (body=%s)", err, assetBody)
	}
	if len(assetReq.Operations) != 3 {
		t.Fatalf("got %d asset operations, want 3 (1 sitelink + 1 callout + 1 snippet): %s", len(assetReq.Operations), assetBody)
	}
	sl := assetReq.Operations[0].Create.SitelinkAsset
	if sl == nil || sl.LinkText != "Register" || sl.Description1 != "Save your seat" || sl.Description2 != "Early bird pricing" {
		t.Errorf("sitelink asset = %+v, want the config's text and both description lines", sl)
	}
	if urls := assetReq.Operations[0].Create.FinalURLs; len(urls) != 1 || !strings.Contains(urls[0], "events.example/kc/register") {
		t.Errorf("sitelink final URLs = %v, want the config's destination (tagged)", urls)
	}
	if co := assetReq.Operations[1].Create.CalloutAsset; co == nil || co.CalloutText != "Free workshops" {
		t.Errorf("callout asset = %+v, want the config's callout text", co)
	}
	sn := assetReq.Operations[2].Create.StructuredSnippetAsset
	if sn == nil || sn.Header != "Courses" || len(sn.Values) != 3 {
		t.Errorf("structured snippet asset = %+v, want header Courses with 3 values", sn)
	}

	if linkBody == nil {
		t.Fatal("assets were created but never linked to the campaign")
	}
	var linkReq struct {
		Operations []struct {
			Create struct {
				Campaign  string `json:"campaign"`
				FieldType string `json:"fieldType"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(linkBody, &linkReq); err != nil {
		t.Fatalf("decode campaignAssets body: %v (body=%s)", err, linkBody)
	}
	wantFields := []string{"SITELINK", "CALLOUT", "STRUCTURED_SNIPPET"}
	if len(linkReq.Operations) != len(wantFields) {
		t.Fatalf("got %d link operations, want %d: %s", len(linkReq.Operations), len(wantFields), linkBody)
	}
	for i, want := range wantFields {
		if got := linkReq.Operations[i].Create.FieldType; got != want {
			t.Errorf("link %d fieldType = %q, want %q", i, got, want)
		}
		if got := linkReq.Operations[i].Create.Campaign; got != "customers/1234567890/campaigns/222" {
			t.Errorf("link %d campaign = %q, want the created campaign resource", i, got)
		}
	}
}

// Multi ad group + multi RSA: one adGroups:mutate per theme, and each group's ads
// in ONE adGroupAds:mutate — a group holding fewer ads than asked for reads as
// complete in the Google Ads UI while rotating less copy than intended.
func TestGoogleAds_AdGroupConfigCreatesOneGroupPerTheme(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{
		"budget":50,
		"cpcBid":2,
		"keywords":[{"text":"cloud native","matchType":"PHRASE"}],
		"adGroups":[
			{"name":"Training","cpcBid":3,"keywords":[{"text":"kubernetes training","matchType":"EXACT"}],
			 "ads":[{"headlines":["Train with us","Hands-on labs","Certify your team"]},{"headlines":["Level up fast","Expert instructors","Live sessions"]}]},
			{"name":"Conference"}
		]
	}}`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil {
		t.Fatal("expected a campaign result")
	}

	cap.mu.Lock()
	groups, ads := cap.adGroups, cap.adGroupAds
	cap.mu.Unlock()
	if len(groups) != 2 {
		t.Fatalf("got %d adGroups:mutate calls, want one per configured theme (2)", len(groups))
	}
	if len(ads) != 2 {
		t.Fatalf("got %d adGroupAds:mutate calls, want one per ad group (2)", len(ads))
	}

	var firstGroup struct {
		Operations []struct {
			Create struct {
				Name         string `json:"name"`
				CPCBidMicros int64  `json:"cpcBidMicros"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(groups[0], &firstGroup); err != nil {
		t.Fatalf("decode adGroups body: %v (body=%s)", err, groups[0])
	}
	if len(firstGroup.Operations) != 1 {
		t.Fatalf("got %d ad group operations in one call, want 1", len(firstGroup.Operations))
	}
	if !strings.HasSuffix(firstGroup.Operations[0].Create.Name, "| Training") {
		t.Errorf("ad group name = %q, want the composed campaign name with the theme label appended", firstGroup.Operations[0].Create.Name)
	}
	// 3 whole currency units, not the campaign-level 2: the per-group override must
	// win, and only for the group that set one.
	if got := firstGroup.Operations[0].Create.CPCBidMicros; got != 3_000_000 {
		t.Errorf("ad group cpcBidMicros = %d, want 3000000 (the per-group override)", got)
	}

	var firstGroupAds struct {
		Operations []json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal(ads[0], &firstGroupAds); err != nil {
		t.Fatalf("decode adGroupAds body: %v (body=%s)", err, ads[0])
	}
	if len(firstGroupAds.Operations) != 2 {
		t.Errorf("got %d ads in the first group's mutate, want both configured RSAs in ONE call", len(firstGroupAds.Operations))
	}

	var secondGroup struct {
		Operations []struct {
			Create struct {
				CPCBidMicros int64 `json:"cpcBidMicros"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(groups[1], &secondGroup); err != nil {
		t.Fatalf("decode adGroups body: %v (body=%s)", err, groups[1])
	}
	if got := secondGroup.Operations[0].Create.CPCBidMicros; got != 2_000_000 {
		t.Errorf("second group cpcBidMicros = %d, want 2000000 inherited from the campaign-level cpcBid", got)
	}
}

// The additive guarantee: a config naming none of the new fields must produce
// exactly the cascade it produced before they existed — one ad group, one ad, and
// no criteria or asset calls at all.
func TestGoogleAds_WithoutTheNewFieldsTheCascadeIsUnchanged(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.campaignCriteria) != 0 {
		t.Errorf("a config with no targeting fields must send no campaign criteria; got %d calls", len(cap.campaignCriteria))
	}
	if cap.assets != nil || cap.campaignAssets != nil {
		t.Error("a config with no extension fields must create no assets")
	}
	if len(cap.adGroups) != 1 || len(cap.adGroupAds) != 1 {
		t.Errorf("got %d ad group and %d ad mutates, want exactly one of each", len(cap.adGroups), len(cap.adGroupAds))
	}
}

// The Search-only fields must be refused BEFORE the budget mutate on Demand Gen:
// discovering the channel cannot take them after the campaign exists leaves a paid
// campaign behind. Each is listed separately because they are refused by three
// different validators, and a regression in one would be hidden by the others.
func TestGoogleAds_SearchOnlyFieldsAreRefusedOnDemandGenBeforeAnyCreate(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
	}{
		{"proximity targets", `"proximityTargets":[{"latitude":37.7749,"longitude":-122.4194,"radius":25,"radiusUnit":"MILES"}]`},
		{"languages", `"languages":["EN"]`},
		{"ad schedules", `"adSchedules":[{"dayOfWeek":"MONDAY","startHour":9,"startMinute":0,"endHour":17,"endMinute":0}]`},
		{"device bid modifiers", `"deviceBidModifiers":[{"device":"MOBILE","bidModifier":1.2}]`},
		{"excluded age ranges", `"excludedAgeRanges":["18-24"]`},
		{"sitelinks", `"sitelinks":[{"text":"Register","finalUrl":"https://events.example/kc/register"}]`},
		{"callouts", `"callouts":["Free workshops"]`},
		{"ad groups", `"adGroups":[{"name":"Training"}]`},
		// demandGenAdGroupCreate has no cpcBidMicros field and demandgen.go never
		// reads the validated bid, so anything other than a refusal here means the
		// caller's bid was accepted and silently discarded.
		{"cpc bid", `"cpcBid":2.5`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen",` + tc.cfg + `}}`)

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil {
				t.Fatal("expected the dispatch to be refused on the demand-gen channel")
			}
			if camp != nil {
				t.Errorf("nothing may be created for a refused input, got campaign %+v", camp)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget {
				t.Error("the refusal must happen BEFORE the budget mutate — a later one strands a paid campaign")
			}
		})
	}
}

// The mappers themselves: nil in, nil out. An omitted field that became an
// empty-but-non-nil slice would read downstream as "the caller asked for none of
// these" rather than "the caller said nothing", which is the difference between
// inheriting a campaign-level value and overriding it with nothing.
func TestGoogleAdsMappers_EmptyInputStaysNil(t *testing.T) {
	if got := googleAdsProximityTargets(nil); got != nil {
		t.Errorf("googleAdsProximityTargets(nil) = %v, want nil", got)
	}
	if got := googleAdsAdSchedules([]googleAdsAdScheduleConfig{}); got != nil {
		t.Errorf("googleAdsAdSchedules(empty) = %v, want nil", got)
	}
	if got := googleAdsDeviceBidModifiers(nil); got != nil {
		t.Errorf("googleAdsDeviceBidModifiers(nil) = %v, want nil", got)
	}
	if got := googleAdsSitelinks(nil); got != nil {
		t.Errorf("googleAdsSitelinks(nil) = %v, want nil", got)
	}
	if got := googleAdsStructuredSnippets(nil); got != nil {
		t.Errorf("googleAdsStructuredSnippets(nil) = %v, want nil", got)
	}
	if got := googleAdsAdGroups(nil); got != nil {
		t.Errorf("googleAdsAdGroups(nil) = %v, want nil", got)
	}
	if got := googleAdsAds(nil); got != nil {
		t.Errorf("googleAdsAds(nil) = %v, want nil", got)
	}

	// A group that overrides nothing must carry nil lists through, so the client
	// reads every field as "inherit".
	groups := googleAdsAdGroups([]googleAdsAdGroupConfig{{Name: "Training"}})
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].Keywords != nil || groups[0].Ads != nil {
		t.Errorf("an override-nothing group must carry nil lists, got %+v", groups[0])
	}
	if groups[0].Name != "Training" {
		t.Errorf("group name = %q, want Training", groups[0].Name)
	}
}

// The bid-modifier pointer must be carried, not copied through a value: an absent
// modifier and an explicit 0 mean opposite things (no adjustment vs the -100%
// opt-out), and a value-typed hop would collapse them.
func TestGoogleAdsAdSchedules_CarriesTheBidModifierPointer(t *testing.T) {
	zero := 0.0
	out := googleAdsAdSchedules([]googleAdsAdScheduleConfig{
		{DayOfWeek: "MONDAY", BidModifier: nil},
		{DayOfWeek: "TUESDAY", BidModifier: &zero},
	})
	if len(out) != 2 {
		t.Fatalf("got %d schedules, want 2", len(out))
	}
	if out[0].BidModifier != nil {
		t.Errorf("an absent bidModifier must stay nil, got %v", *out[0].BidModifier)
	}
	if out[1].BidModifier == nil || *out[1].BidModifier != 0 {
		t.Errorf("an explicit 0 must survive as 0, got %v", out[1].BidModifier)
	}
}

// The Search-only refusals must hold on the ADOPTION path too, which is the half the
// per-field table above cannot reach: adoption returns before any create runs, so a
// request that CreateDemandGenCampaign would refuse is never handed to it.
//
// This is the asymmetry ValidateCampaignInput exists to prevent, reaching the new
// fields. The dispatcher used to validate every request as Search because
// preflightCampaignKind only used the kind to compose a name; now the kind gates
// refusals, so validating as Search let a Demand Gen request carrying proximity,
// criteria, extensions, ad groups or a CPC bid validate clean and then be adopted —
// accepted when a same-name campaign happened to exist, refused when it did not.
//
// The server fails on ANY request: the refusal must land before the adoption lookup,
// not merely before a create, so there is nothing legitimate for it to serve.
func TestGoogleAds_SearchOnlyFieldsAreRefusedOnDemandGenAdoptionToo(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
	}{
		{"proximity targets", `"proximityTargets":[{"latitude":37.7749,"longitude":-122.4194,"radius":25,"radiusUnit":"MILES"}]`},
		{"languages", `"languages":["EN"]`},
		{"ad schedules", `"adSchedules":[{"dayOfWeek":"MONDAY","startHour":9,"startMinute":0,"endHour":17,"endMinute":0}]`},
		{"device bid modifiers", `"deviceBidModifiers":[{"device":"MOBILE","bidModifier":1.2}]`},
		{"excluded age ranges", `"excludedAgeRanges":["18-24"]`},
		{"sitelinks", `"sitelinks":[{"text":"Register","finalUrl":"https://events.example/kc/register"}]`},
		{"callouts", `"callouts":["Free workshops"]`},
		{"ad groups", `"adGroups":[{"name":"Training"}]`},
		{"cpc bid", `"cpcBid":2.5`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			}))
			t.Cleanup(tokenSrv.Close)
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// t.Errorf, never t.Fatal: this runs on the server's goroutine.
				t.Errorf("a Search-only field on demand-gen must be refused before any call to Google, got %s", r.URL.Path)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}))
			t.Cleanup(apiSrv.Close)

			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)},
				identityEncryptor{},
				googleads.WithTokenURL(tokenSrv.URL),
				googleads.WithBaseURL(apiSrv.URL),
			)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","adoptExisting":true,` + tc.cfg + `}}`)

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil {
				t.Fatalf("a Search-only field must be refused whatever sits in the ad account, got campaign %+v", camp)
			}
			if camp != nil {
				t.Errorf("a pre-send validation failure created nothing, so the result must be nil to release the claim; got %+v", camp)
			}
		})
	}
}

// The kind-taking entry point is what makes the test above possible, and the kind must
// genuinely reach the preflight's gates rather than being accepted and ignored. The same
// input is refused for Demand Gen and accepted for Search, with no upstream call either
// way — ValidateCampaignInputKind's contract is that it mutates nothing and sends nothing.
func TestGoogleAdsClient_ValidateCampaignInputKindGatesOnTheKind(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("validation must send nothing, got %s", r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(apiSrv.Close)

	client := googleads.NewClient(
		googleads.Credentials{ClientID: "cid", ClientSecret: "sec", RefreshToken: "ref", DeveloperToken: "dev"},
		googleads.AccountConfig{CustomerID: "1234567890"},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)

	in := googleads.CampaignInput{
		Project:         "tlf",
		EventName:       "KubeCon",
		Budget:          50,
		RegistrationURL: "https://events.example/kc",
		Headlines:       []string{"Attend KubeCon", "Register today", "Join the community"},
		Descriptions:    []string{"Three days of talks and workshops.", "Meet maintainers in person."},
		Languages:       []string{"EN"},
	}
	if err := client.ValidateCampaignInputKind(googleads.CampaignKindDemandGen, in); err == nil {
		t.Error("languages are Search-only; validating for Demand Gen must refuse them")
	}
	if err := client.ValidateCampaignInputKind(googleads.CampaignKindSearch, in); err != nil {
		t.Errorf("the same input is valid for Search, got %v", err)
	}
	// The un-kinded entry point keeps its documented Search behaviour for callers that
	// have not been updated.
	if err := client.ValidateCampaignInput(in); err != nil {
		t.Errorf("ValidateCampaignInput assumes Search, got %v", err)
	}
}

// Sibling parity with the Microsoft dispatcher's TestMicrosoft_UnresolvableGeoTargetCreatesNothing
// and with TestGoogleAds_BadServingReadinessConfigIsPreCreate, extended to the field
// families LFXV2-2665 added. Those two cover an unusable value in a field that
// predates this work; every validator below is new, and each one of them runs
// inside preflightCampaignKind precisely so a bad value costs nothing upstream.
//
// This is the SEARCH channel, where all of these fields are supported — so a
// refusal here is about the value, not the channel, which is what separates this
// table from TestGoogleAds_SearchOnlyFieldsAreRefusedOnDemandGenBeforeAnyCreate.
// One case per validator: they fail independently, and a regression in one would
// be invisible behind the others.
func TestGoogleAds_BadTargetingConfigIsPreCreate(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
	}{
		{"geo target also excluded", `"geoTargets":["GB"],"excludedGeoTargets":["GB"]`},
		{"unmapped excluded geo target", `"excludedGeoTargets":["Narnia"]`},
		{"proximity radius unit", `"proximityTargets":[{"latitude":37.7749,"longitude":-122.4194,"radius":25,"radiusUnit":"FURLONGS"}]`},
		{"unmapped language", `"languages":["Klingon"]`},
		{"schedule ends before it starts", `"adSchedules":[{"dayOfWeek":"MONDAY","startHour":17,"startMinute":0,"endHour":9,"endMinute":0}]`},
		{"duplicate device", `"deviceBidModifiers":[{"device":"MOBILE","bidModifier":1.2},{"device":"MOBILE","bidModifier":1.4}]`},
		{"bid modifier out of range", `"deviceBidModifiers":[{"device":"MOBILE","bidModifier":99}]`},
		{"unknown excluded gender", `"excludedGenders":["OTHER"]`},
		{"sitelink with one description line", `"sitelinks":[{"text":"Register","finalUrl":"https://events.example/kc","description1":"Save your seat"}]`},
		{"duplicate callout", `"callouts":["Free workshops","Free workshops"]`},
		{"structured snippet with too few values", `"structuredSnippets":[{"header":"Courses","values":["Kubernetes"]}]`},
		{"ad group without a name", `"adGroups":[{"name":""}]`},
		{"duplicate ad group name", `"adGroups":[{"name":"Training"},{"name":"Training"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,` + tc.cfg + `}}`)

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil {
				t.Fatal("expected the dispatch to be refused")
			}
			if camp != nil {
				t.Errorf("nothing may be created for a refused input, got campaign %+v", camp)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget {
				t.Error("the refusal must happen BEFORE the budget mutate — a later one strands a paid budget and campaign")
			}
		})
	}
}

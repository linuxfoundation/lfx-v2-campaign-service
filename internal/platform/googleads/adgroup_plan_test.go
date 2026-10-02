// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// samplePlanBase is the group preflightCampaignKind derives from the
// campaign-level fields — the no-AdGroups answer and the source of every
// per-group fallback. Each field carries a distinct value so an inheritance
// test can tell which one it got.
func samplePlanBase() adGroupPlan {
	return adGroupPlan{
		name:             "LFX | Ad Group | CNCF | KubeCon | brief-1",
		cpcBidMicros:     2_000_000,
		keywords:         []Keyword{{Text: "kubecon", MatchType: MatchTypeExact}},
		audienceSegments: []string{"customers/1234567890/userLists/9"},
		ads: []adPlan{{
			headlines:    []string{"Join KubeCon", "Register today", "Cloud native"},
			descriptions: []string{"Three days of talks", "Early bird pricing"},
		}},
	}
}

// adGroupCascade answers a multi-group create. Both handlers run on httptest's
// own goroutines, so every request body and every id they hand back to the test
// goes through the mutex — never a bare variable, and never t.Fatalf from inside
// a handler. See test-hygiene.md:httptest-handler-state-needs-synchronized-handoff.
type adGroupCascade struct {
	mu          sync.Mutex
	groupIDs    []string
	groupBodies []string
	adBodies    []string
	// failGroup, when >0, makes the Nth adGroups:mutate (1-based) fail with a
	// definite 4xx, so a test can place a failure at a chosen point in the loop.
	failGroup int
	// adGroupOverride, when non-empty, is used as the ad-group half of every
	// adGroupAd resource name instead of the group the ad was created under.
	adGroupOverride string
	// truncateAds drops all but the first result from each ad mutate reply.
	truncateAds bool
}

func (s *adGroupCascade) adGroup(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.groupBodies = append(s.groupBodies, string(b))
	n := len(s.groupBodies)
	fail := s.failGroup == n
	var id string
	if !fail {
		id = strconv.Itoa(330 + len(s.groupIDs))
		s.groupIDs = append(s.groupIDs, id)
	}
	s.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT","message":"boom"}}`)
		return
	}
	_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/`+id+`"}]}`)
}

func (s *adGroupCascade) adGroupAd(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var req mutateRequest
	_ = json.Unmarshal(b, &req)
	s.mu.Lock()
	s.adBodies = append(s.adBodies, string(b))
	seq := len(s.adBodies)
	group := "0"
	if n := len(s.groupIDs); n > 0 {
		group = s.groupIDs[n-1]
	}
	if s.adGroupOverride != "" {
		group = s.adGroupOverride
	}
	truncate := s.truncateAds
	s.mu.Unlock()

	parts := make([]string, 0, len(req.Operations))
	for i := range req.Operations {
		if truncate && i > 0 {
			break
		}
		parts = append(parts, `{"resourceName":"customers/1234567890/adGroupAds/`+group+`~`+strconv.Itoa(400+seq*10+i)+`"}`)
	}
	_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
}

func (s *adGroupCascade) groupRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.groupBodies...)
}

func (s *adGroupCascade) adRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.adBodies...)
}

// newCascadeClient wires the Search cascade against one adGroupCascade. Tests
// that use it pass no keywords or audience segments, so no adGroupCriteria
// mutate is sent and the server's default arm stays an assertion rather than a
// route.
func newCascadeClient(t *testing.T, s *adGroupCascade) *Client {
	t.Helper()
	return newCampaignClientFull(t, okBudget, okCampaign, s.adGroup, s.adGroupAd)
}

// twoThemeInput is a Search brief split into two themed groups, each with its
// own copy, and with no campaign-level keywords or audiences so the cascade
// sends no criteria mutate.
func twoThemeInput() CampaignInput {
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{
		{
			Name: "Training",
			Ads: []AdSpec{
				{Headlines: []string{"Kubernetes training", "Hands on labs", "Learn by doing"}, Descriptions: []string{"Four tracks over three days", "Taught by maintainers"}},
				{Headlines: []string{"Certification prep", "CKA and CKAD", "Study with us"}, Descriptions: []string{"Practice exams included", "Taught by maintainers"}},
			},
		},
		{
			Name: "Conference",
			Ads: []AdSpec{
				{Headlines: []string{"KubeCon keynotes", "Meet the maintainers", "Hallway track"}, Descriptions: []string{"Three days of talks", "Early bird pricing"}},
			},
		},
	}
	return in
}

// ---------------------------------------------------------------------------
// Plan validation
// ---------------------------------------------------------------------------

// The whole feature is opt-in. A caller that sets no AdGroups must get back
// EXACTLY the campaign this client built before — same name, same single ad —
// because the single-group path is what every existing campaign was created
// with and what every existing test and reader assumes.
func TestValidateAdGroupPlans_NoAdGroupsYieldsTheLegacySingleGroup(t *testing.T) {
	base := samplePlanBase()
	got, err := validateAdGroupPlans(campaignKindSearch, sampleInput(), base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d plans, want exactly the single default group", len(got))
	}
	if got[0].name != base.name {
		t.Errorf("name = %q, want the composed campaign-level name %q unchanged", got[0].name, base.name)
	}
	if len(got[0].ads) != 1 {
		t.Errorf("got %d ads, want the single ad the campaign-level copy produces", len(got[0].ads))
	}
	if got[0].cpcBidMicros != base.cpcBidMicros || len(got[0].keywords) != len(base.keywords) {
		t.Errorf("got %+v, want the base plan verbatim", got[0])
	}
}

// Demand Gen builds its own ad group and deliberately creates no ad at all, so
// an AdGroups list there is not "ignored" — it is a request this channel cannot
// carry out, and silently dropping it would report a split campaign that does
// not exist. The Search half of each case is the over-refusal guard: the
// refusal must be about the CHANNEL, never about the input.
func TestValidateAdGroupPlans_RefusesAdGroupsOnDemandGen(t *testing.T) {
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{{Name: "Training"}}

	if _, err := validateAdGroupPlans(campaignKindDemandGen, in, samplePlanBase()); err == nil {
		t.Fatal("an AdGroups list must be refused on Demand Gen")
	}
	if _, err := validateAdGroupPlans(campaignKindSearch, in, samplePlanBase()); err != nil {
		t.Fatalf("the same input must be accepted on Search, got %v", err)
	}
	// And an empty list stays legal on both — Demand Gen still needs its one group.
	for _, kind := range []string{campaignKindSearch, campaignKindDemandGen} {
		plans, err := validateAdGroupPlans(kind, sampleInput(), samplePlanBase())
		if err != nil || len(plans) != 1 {
			t.Errorf("%s: got (%d plans, %v), want the single default group", kind, len(plans), err)
		}
	}
}

// Both caps refuse at the boundary+1 and accept AT the boundary — a cap that
// also rejects the last legal value refuses a campaign Google would have
// created, which is the expensive direction to be wrong in.
func TestValidateAdGroupPlans_CapsTheGroupList(t *testing.T) {
	specs := func(n int) []AdGroupSpec {
		out := make([]AdGroupSpec, n)
		for i := range out {
			out[i] = AdGroupSpec{Name: "Theme " + strconv.Itoa(i)}
		}
		return out
	}

	in := sampleInput()
	in.AdGroups = specs(maxAdGroupsPerCampaign)
	plans, err := validateAdGroupPlans(campaignKindSearch, in, samplePlanBase())
	if err != nil {
		t.Fatalf("exactly %d ad groups must be accepted, got %v", maxAdGroupsPerCampaign, err)
	}
	if len(plans) != maxAdGroupsPerCampaign {
		t.Errorf("got %d plans, want %d", len(plans), maxAdGroupsPerCampaign)
	}

	in.AdGroups = specs(maxAdGroupsPerCampaign + 1)
	if _, err := validateAdGroupPlans(campaignKindSearch, in, samplePlanBase()); err == nil {
		t.Fatalf("%d ad groups must be refused", maxAdGroupsPerCampaign+1)
	}
}

// Google's limit is three enabled responsive search ads per group, and it is
// enforced at the adGroupAds:mutate — which on this cascade runs after the
// campaign is already paid for.
func TestValidateAdGroupPlans_CapsAdsPerGroup(t *testing.T) {
	ads := func(n int) []AdSpec {
		out := make([]AdSpec, n)
		for i := range out {
			out[i] = AdSpec{Headlines: []string{"Headline " + strconv.Itoa(i), "Second", "Third"}, Descriptions: []string{"First description", "Second description"}}
		}
		return out
	}

	in := sampleInput()
	in.AdGroups = []AdGroupSpec{{Name: "Training", Ads: ads(maxAdsPerAdGroup)}}
	plans, err := validateAdGroupPlans(campaignKindSearch, in, samplePlanBase())
	if err != nil {
		t.Fatalf("exactly %d ads must be accepted, got %v", maxAdsPerAdGroup, err)
	}
	if len(plans) != 1 || len(plans[0].ads) != maxAdsPerAdGroup {
		t.Fatalf("got %d plans with %d ads, want 1 plan with %d ads", len(plans), len(plans[0].ads), maxAdsPerAdGroup)
	}

	in.AdGroups = []AdGroupSpec{{Name: "Training", Ads: ads(maxAdsPerAdGroup + 1)}}
	err = func() error {
		_, err := validateAdGroupPlans(campaignKindSearch, in, samplePlanBase())
		return err
	}()
	if err == nil {
		t.Fatalf("%d ads in one group must be refused", maxAdsPerAdGroup+1)
	}
	if !strings.Contains(err.Error(), "Training") {
		t.Errorf("the rejection must name the group it is about, got: %v", err)
	}
}

// Google rejects a duplicate ad group name within a campaign, and two names
// differing only in case collide upstream — by which point the earlier groups
// in the list already exist and need manual cleanup.
func TestValidateAdGroupPlans_RefusesDuplicateNamesCaseInsensitively(t *testing.T) {
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{{Name: "Training"}, {Name: "training"}}
	_, err := validateAdGroupPlans(campaignKindSearch, in, samplePlanBase())
	if err == nil {
		t.Fatal("two group names differing only in case must be refused")
	}
	if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("the rejection must say the name is reused, got: %v", err)
	}
}

func TestValidateAdGroupPlans_RefusesAnUnnamedGroup(t *testing.T) {
	for name, label := range map[string]string{"empty": "", "whitespace only": "   ", "separators only": " | "} {
		t.Run(name, func(t *testing.T) {
			in := sampleInput()
			in.AdGroups = []AdGroupSpec{{Name: label}}
			if _, err := validateAdGroupPlans(campaignKindSearch, in, samplePlanBase()); err == nil {
				t.Fatalf("a group named %q must be refused", label)
			}
		})
	}
}

// Fallback is PER FIELD, not all-or-nothing: a group that overrides only its
// bid keeps the campaign's keywords, audiences and copy. Anything else turns
// "give this theme a higher bid" into "give this theme no keywords".
func TestValidateAdGroupPlans_InheritsEachFieldIndependently(t *testing.T) {
	base := samplePlanBase()
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{
		{Name: "Bid only", CPCBid: 4},
		{Name: "Keywords only", Keywords: []Keyword{{Text: "cloud native training", MatchType: MatchTypePhrase}}},
		{Name: "Audiences only", AudienceSegments: []string{"customers/1234567890/userLists/77"}},
		{Name: "Copy only", Ads: []AdSpec{{Headlines: []string{"Own copy", "Second", "Third"}, Descriptions: []string{"First description", "Second description"}}}},
	}
	plans, err := validateAdGroupPlans(campaignKindSearch, in, base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plans) != 4 {
		t.Fatalf("got %d plans, want 4", len(plans))
	}

	bidOnly := plans[0]
	if bidOnly.cpcBidMicros != 4*int64(microsPerUnit) {
		t.Errorf("cpcBidMicros = %d, want the overridden bid", bidOnly.cpcBidMicros)
	}
	if len(bidOnly.keywords) != len(base.keywords) || len(bidOnly.audienceSegments) != len(base.audienceSegments) || len(bidOnly.ads) != len(base.ads) {
		t.Errorf("a bid-only override must inherit keywords, audiences and copy, got %+v", bidOnly)
	}

	kwOnly := plans[1]
	if len(kwOnly.keywords) != 1 || kwOnly.keywords[0].Text != "cloud native training" {
		t.Errorf("keywords = %+v, want the group's own list", kwOnly.keywords)
	}
	if kwOnly.cpcBidMicros != base.cpcBidMicros {
		t.Errorf("cpcBidMicros = %d, want the inherited %d", kwOnly.cpcBidMicros, base.cpcBidMicros)
	}

	audOnly := plans[2]
	if len(audOnly.audienceSegments) != 1 || audOnly.audienceSegments[0] != "customers/1234567890/userLists/77" {
		t.Errorf("audienceSegments = %+v, want the group's own list", audOnly.audienceSegments)
	}
	if len(audOnly.keywords) != len(base.keywords) {
		t.Errorf("an audience-only override must inherit keywords, got %+v", audOnly.keywords)
	}

	copyOnly := plans[3]
	if len(copyOnly.ads) != 1 || copyOnly.ads[0].headlines[0] != "Own copy" {
		t.Errorf("ads = %+v, want the group's own copy", copyOnly.ads)
	}
	if copyOnly.cpcBidMicros != base.cpcBidMicros {
		t.Errorf("a copy-only override must inherit the bid, got %d", copyOnly.cpcBidMicros)
	}
}

// 0 is the zero value of a float64 field, so it is what every caller that does
// not care about per-theme bidding sends. Reading it as "bid nothing" would
// silently unset a bid the campaign deliberately set.
func TestValidateAdGroupPlans_ZeroCPCBidInherits(t *testing.T) {
	base := samplePlanBase()
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{{Name: "Training", CPCBid: 0}}
	plans, err := validateAdGroupPlans(campaignKindSearch, in, base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plans[0].cpcBidMicros != base.cpcBidMicros {
		t.Errorf("cpcBidMicros = %d, want the inherited %d — CPCBid 0 means inherit, not zero", plans[0].cpcBidMicros, base.cpcBidMicros)
	}
}

// The composed name is what every reconcile-by-name path matches on, so its
// shape is a contract, not cosmetics — and it is bounded by the same 255-rune
// limit the single-group name is.
func TestValidateAdGroupPlans_ComposesTheNameAndBoundsItsLength(t *testing.T) {
	base := samplePlanBase()
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{{Name: "Training"}}
	plans, err := validateAdGroupPlans(campaignKindSearch, in, base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := base.name + " | Training"; plans[0].name != want {
		t.Errorf("name = %q, want %q", plans[0].name, want)
	}

	in.AdGroups = []AdGroupSpec{{Name: strings.Repeat("x", maxAdGroupNameRunes)}}
	if _, err := validateAdGroupPlans(campaignKindSearch, in, base); err == nil {
		t.Fatalf("a composed name over %d runes must be refused", maxAdGroupNameRunes)
	}
}

// Google accepts two ads with identical copy in one group. Refusing it here
// would turn a campaign Google would have created into a failure — wasteful is
// not the same as invalid, and only the second is ours to refuse.
func TestValidateAdGroupPlans_AllowsDuplicateCopyAcrossAdsInAGroup(t *testing.T) {
	ad := AdSpec{Headlines: []string{"Join KubeCon", "Register today", "Cloud native"}, Descriptions: []string{"Three days of talks", "Early bird pricing"}}
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{{Name: "Training", Ads: []AdSpec{ad, ad}}}
	plans, err := validateAdGroupPlans(campaignKindSearch, in, samplePlanBase())
	if err != nil {
		t.Fatalf("duplicate copy within a group must be accepted, got %v", err)
	}
	if len(plans[0].ads) != 2 {
		t.Errorf("got %d ads, want both kept", len(plans[0].ads))
	}
}

// A group's copy goes through the SAME composeAdCopy the campaign-level copy
// does, so it inherits that contract exactly: over-long assets are truncated to
// Google's weight limit and short lists are padded to the minimum, rather than
// refused. A second, stricter rule for per-group copy would refuse briefs the
// single-group path accepts.
func TestValidateAdGroupPlans_ComposesGroupCopyLikeTheCampaignsOwn(t *testing.T) {
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{{Name: "Training", Ads: []AdSpec{{Headlines: []string{strings.Repeat("x", 500)}}}}}
	plans, err := validateAdGroupPlans(campaignKindSearch, in, samplePlanBase())
	if err != nil {
		t.Fatalf("over-long copy is truncated, not refused: %v", err)
	}
	ad := plans[0].ads[0]
	if len(ad.headlines) < minHeadlines || len(ad.descriptions) < minDescriptions {
		t.Fatalf("got %d headlines and %d descriptions, want padding to Google's minimums", len(ad.headlines), len(ad.descriptions))
	}
	// The headline is pure ASCII, so its Google character weight is its rune count.
	if n := utf8.RuneCountInString(ad.headlines[0]); n > maxHeadlineWeight {
		t.Errorf("headline is %d runes, over the %d weight limit; it should have been truncated", n, maxHeadlineWeight)
	}
}

func TestAdGroupPlanStep_CountsGroupsAdsAndKeywords(t *testing.T) {
	plans := []adGroupPlan{
		{ads: []adPlan{{}, {}}, keywords: []Keyword{{Text: "a"}, {Text: "b"}}},
		{ads: []adPlan{{}}, keywords: []Keyword{{Text: "c"}}},
	}
	if got, want := adGroupPlanStep(plans), "2 ad groups, 3 ads, 3 keywords"; got != want {
		t.Errorf("adGroupPlanStep = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// The cascade on the wire
// ---------------------------------------------------------------------------

// The shape that matters upstream: one adGroups:mutate per group, and ALL of a
// group's ads in ONE adGroupAds:mutate. Google applies a mutate atomically, so
// a single mutate per group means a group gets all its ads or none — a group
// holding one of the three ads asked for reads as complete in the UI while
// rotating less copy than the campaign was built to test.
func TestCreateCampaign_CreatesOneMutatePerGroupAndOneAdMutatePerGroup(t *testing.T) {
	s := &adGroupCascade{}
	res, err := newCascadeClient(t, s).CreateCampaign(context.Background(), twoThemeInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	groups := s.groupRequests()
	if len(groups) != 2 {
		t.Fatalf("got %d adGroups:mutate calls, want one per planned group", len(groups))
	}
	if !strings.Contains(groups[0], "| Training") || !strings.Contains(groups[1], "| Conference") {
		t.Errorf("groups created out of the planned order: %v", groups)
	}

	ads := s.adRequests()
	if len(ads) != 2 {
		t.Fatalf("got %d adGroupAds:mutate calls, want exactly one per group", len(ads))
	}
	var first, second mutateRequest
	if err := json.Unmarshal([]byte(ads[0]), &first); err != nil {
		t.Fatalf("decode first ad mutate: %v", err)
	}
	if err := json.Unmarshal([]byte(ads[1]), &second); err != nil {
		t.Fatalf("decode second ad mutate: %v", err)
	}
	if len(first.Operations) != 2 {
		t.Errorf("the Training group sent %d ad operations, want both its ads in one mutate", len(first.Operations))
	}
	if len(second.Operations) != 1 {
		t.Errorf("the Conference group sent %d ad operations, want 1", len(second.Operations))
	}

	if len(res.AdGroups) != 2 {
		t.Fatalf("res.AdGroups = %+v, want one entry per group", res.AdGroups)
	}
	if res.AdGroups[0].ID != "330" || res.AdGroups[1].ID != "331" {
		t.Errorf("res.AdGroups ids = %q/%q, want the ids each mutate returned in order", res.AdGroups[0].ID, res.AdGroups[1].ID)
	}
	if len(res.AdGroups[0].AdIDs) != 2 || len(res.AdGroups[1].AdIDs) != 1 {
		t.Errorf("ad ids per group = %v / %v, want 2 then 1", res.AdGroups[0].AdIDs, res.AdGroups[1].AdIDs)
	}
	// The scalars predate multi-group support and must keep describing the FIRST
	// group, so the dispatcher's status toggle and every persisted blob still
	// resolve to a real ad group.
	if res.AdGroupID != res.AdGroups[0].ID || res.AdGroupName != res.AdGroups[0].Name {
		t.Errorf("scalar ad group = %q/%q, want the first group %q/%q", res.AdGroupID, res.AdGroupName, res.AdGroups[0].ID, res.AdGroups[0].Name)
	}
	if res.AdID != res.AdGroups[0].AdIDs[0] {
		t.Errorf("res.AdID = %q, want the first ad of the first group %q", res.AdID, res.AdGroups[0].AdIDs[0])
	}
}

// A single-group campaign must still produce exactly the result shape readers
// had before this feature — one AdGroups entry, and the scalars populated.
func TestCreateCampaign_SingleGroupStillReportsOneEntry(t *testing.T) {
	s := &adGroupCascade{}
	res, err := newCascadeClient(t, s).CreateCampaign(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(s.groupRequests()) != 1 || len(s.adRequests()) != 1 {
		t.Fatalf("got %d group and %d ad mutates, want one each", len(s.groupRequests()), len(s.adRequests()))
	}
	if len(res.AdGroups) != 1 || res.AdGroups[0].ID != res.AdGroupID || len(res.AdGroups[0].AdIDs) != 1 {
		t.Errorf("res.AdGroups = %+v, want the single group mirroring the scalars", res.AdGroups)
	}
}

// Past the campaign create the campaign already costs money, so a failure on
// group 2 of 3 must come back ALONGSIDE a result that says exactly how far the
// cascade got: group 1 complete, group 2 attempted with no id, group 3 absent.
func TestCreateCampaign_MidCascadeFailureReportsHowFarItGot(t *testing.T) {
	s := &adGroupCascade{failGroup: 2}
	in := twoThemeInput()
	in.AdGroups = append(in.AdGroups, AdGroupSpec{Name: "Certification"})

	res, err := newCascadeClient(t, s).CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected the second ad group's failure to be returned")
	}
	if res == nil {
		t.Fatal("the campaign exists and costs money: the result must never be nil past the campaign create")
	}
	if !strings.Contains(err.Error(), "ad group 2 of 3") {
		t.Errorf("the error must say which group failed and how many there were, got: %v", err)
	}
	if len(res.AdGroups) != 2 {
		t.Fatalf("res.AdGroups = %+v, want an entry for the group that succeeded and the one that failed, and none for the group never attempted", res.AdGroups)
	}
	if res.AdGroups[0].ID == "" || len(res.AdGroups[0].AdIDs) != 2 {
		t.Errorf("the first group should be complete, got %+v", res.AdGroups[0])
	}
	if res.AdGroups[1].ID != "" {
		t.Errorf("the failed group must carry its name with an empty id, got %+v", res.AdGroups[1])
	}
	if res.AdGroups[1].Name == "" {
		t.Error("the failed group must still carry its deterministic name, which is the only reconcile key before an id exists")
	}
}

// A 2xx whose results do not cover every operation is not a success: some ads
// may exist with ids this run cannot read, which is exactly the case where a
// blind retry double-creates them.
func TestCreateCampaign_ShortAdResponseIsUnconfirmed(t *testing.T) {
	s := &adGroupCascade{truncateAds: true}
	res, err := newCascadeClient(t, s).CreateCampaign(context.Background(), twoThemeInput())
	if err == nil {
		t.Fatal("a 2 operation ad mutate answered with 1 result must not be treated as confirmed")
	}
	if res == nil {
		t.Fatal("the campaign exists: the result must be non-nil")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("want an UNCONFIRMED error, got: %v", err)
	}
	if res.AdID != "" {
		t.Errorf("no ad id may be persisted from an unconfirmed mutate, got %q", res.AdID)
	}
}

// The adGroupAd resource name's ad-group half is identity proof. One naming a
// different ad group means the response does not describe the ad this call just
// created, so the id in it cannot be trusted enough to persist.
func TestCreateCampaign_AdNamingAnotherAdGroupIsUnconfirmed(t *testing.T) {
	s := &adGroupCascade{adGroupOverride: "999"}
	res, err := newCascadeClient(t, s).CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("an adGroupAd resource name reporting another ad group must be refused")
	}
	if !strings.Contains(err.Error(), "different ad group id") {
		t.Errorf("want the mismatch named in the error, got: %v", err)
	}
	if res == nil || res.AdID != "" {
		t.Errorf("res = %+v, want a non-nil partial carrying no ad id", res)
	}
}

// Every ad group rejection is pure local input validation and must land before
// the budget mutate — the first thing in the cascade that costs money. The
// paired ValidateCampaignInput call is the adoption path: the same input must
// be refused there too, or validity would depend on whether a same-name
// campaign happened to exist.
func TestCreateCampaign_BadAdGroupsFailBeforeAnyMutate(t *testing.T) {
	ads := make([]AdSpec, maxAdsPerAdGroup+1)
	groups := make([]AdGroupSpec, maxAdGroupsPerCampaign+1)
	for i := range groups {
		groups[i] = AdGroupSpec{Name: "Theme " + strconv.Itoa(i)}
	}

	cases := map[string][]AdGroupSpec{
		"unnamed group":     {{Name: "  "}},
		"duplicate names":   {{Name: "Training"}, {Name: "TRAINING"}},
		"too many groups":   groups,
		"too many ads":      {{Name: "Training", Ads: ads}},
		"out of range bid":  {{Name: "Training", CPCBid: maxCPCBid + 1}},
		"invalid keyword":   {{Name: "Training", Keywords: []Keyword{{Text: "kubecon", MatchType: "FUZZY"}}}},
		"invalid audience":  {{Name: "Training", AudienceSegments: []string{"customers/1/customAudiences/3"}}},
		"over long name":    {{Name: strings.Repeat("x", maxAdGroupNameRunes)}},
		"name only pipes":   {{Name: " | "}},
		"second group dupe": {{Name: "Training"}, {Name: "Conference"}, {Name: "training"}},
	}

	for name, specs := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCampaignClientFull(t,
				func(w http.ResponseWriter, r *http.Request) {
					t.Errorf("budget mutate must not be sent: %s", r.URL.Path)
				},
				func(w http.ResponseWriter, r *http.Request) {
					t.Errorf("campaign mutate must not be sent: %s", r.URL.Path)
				},
				func(w http.ResponseWriter, r *http.Request) {
					t.Errorf("ad group mutate must not be sent: %s", r.URL.Path)
				},
				func(w http.ResponseWriter, r *http.Request) { t.Errorf("ad mutate must not be sent: %s", r.URL.Path) },
			)
			in := sampleInput()
			in.AdGroups = specs

			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatalf("%s must be refused", name)
			}
			if res != nil {
				t.Errorf("nothing was created, so the result must be nil, got %+v", res)
			}
			if err := c.ValidateCampaignInput(in); err == nil {
				t.Errorf("the adoption path must refuse %s identically", name)
			}
		})
	}
}

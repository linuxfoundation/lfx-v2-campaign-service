// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

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
	"time"
)

// #473: Search parity with the live KubeCon NA 2026 campaign — responsive search ad
// display paths, ad-group negative keywords, and ad-group income/parental-status
// exclusions. The live shapes these tests pin were checked with validateOnly probes on
// 2026-11-03; the fakes below answer in Google's real mutate-response shape.

// parityServer serves every mutate the Search and Display cascades send, records each
// request path in order and every adGroupAds/adGroupCriteria body, and leaves the
// adGroupCriteria answer to the test. adGroups are numbered 330, 331, … so a
// multi-group test can tell which group a criterion was created under.
type parityServer struct {
	mu            sync.Mutex
	paths         []string
	adBodies      []string
	criteriaReqs  []string
	groupSeq      int
	criteriaReply func(body []byte) (int, string)
}

func (s *parityServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		s.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			s.mu.Lock()
			id := strconv.Itoa(330 + s.groupSeq)
			s.groupSeq++
			s.mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/`+id+`"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			s.mu.Lock()
			s.adBodies = append(s.adBodies, string(body))
			group := strconv.Itoa(330 + s.groupSeq - 1)
			s.mu.Unlock()
			_, _ = io.WriteString(w, perGroupResults(body, "adGroupAds", group, 440))
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			s.mu.Lock()
			s.criteriaReqs = append(s.criteriaReqs, string(body))
			reply := s.criteriaReply
			s.mu.Unlock()
			if reply == nil {
				_, _ = io.WriteString(w, criteriaEcho(body))
				return
			}
			status, out := reply(body)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, out)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (s *parityServer) snapshot() (paths, ads, criteria []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...), append([]string(nil), s.adBodies...), append([]string(nil), s.criteriaReqs...)
}

// perGroupResults answers one composite resource per operation under the given group.
func perGroupResults(body []byte, kind, group string, base int) string {
	var req struct {
		Operations []json.RawMessage `json:"operations"`
	}
	_ = json.Unmarshal(body, &req)
	parts := make([]string, 0, len(req.Operations))
	for i := range req.Operations {
		parts = append(parts, fmt.Sprintf(`{"resourceName":"customers/1234567890/%s/%s~%d"}`, kind, group, base+i))
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`
}

// criteriaEcho answers an adGroupCriteria:mutate with one criterion per operation under
// the ad group that operation named, as Google does.
func criteriaEcho(body []byte) string {
	var req struct {
		Operations []struct {
			Create struct {
				AdGroup string `json:"adGroup"`
			} `json:"create"`
		} `json:"operations"`
	}
	_ = json.Unmarshal(body, &req)
	parts := make([]string, 0, len(req.Operations))
	for i, op := range req.Operations {
		group := op.Create.AdGroup[strings.LastIndex(op.Create.AdGroup, "/")+1:]
		parts = append(parts, fmt.Sprintf(`{"resourceName":"customers/1234567890/adGroupCriteria/%s~%d"}`, group, 900+i))
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`
}

func newParityClient(t *testing.T, s *parityServer) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(s.handler(t))
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
}

// createOps decodes a mutate body into its create payloads, in order.
func createOps(t *testing.T, body string) []map[string]any {
	t.Helper()
	var req struct {
		Operations []struct {
			Create map[string]any `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode mutate body: %v (%s)", err, body)
	}
	out := make([]map[string]any, 0, len(req.Operations))
	for _, op := range req.Operations {
		out = append(out, op.Create)
	}
	return out
}

func rsaOf(t *testing.T, op map[string]any) map[string]any {
	t.Helper()
	ad, _ := op["ad"].(map[string]any)
	rsa, ok := ad["responsiveSearchAd"].(map[string]any)
	if !ok {
		t.Fatalf("ad operation carries no responsiveSearchAd: %v", op)
	}
	return rsa
}

// ---------------------------------------------------------------------------
// Display paths
// ---------------------------------------------------------------------------

func TestValidateSearchAdPaths(t *testing.T) {
	cases := []struct {
		name, p1, p2     string
		want1, want2     string
		wantErrSubstring string
	}{
		{name: "none", p1: "", p2: ""},
		{name: "trimmed", p1: "  kubecon ", p2: " na ", want1: "kubecon", want2: "na"},
		// The probe's boundary: 15 accepted, 16 refused with TOO_LONG.
		{name: "15 accepted", p1: strings.Repeat("a", 15), want1: strings.Repeat("a", 15)},
		{name: "16 refused", p1: strings.Repeat("a", 16), wantErrSubstring: "display width of 16"},
		{name: "path2 16 refused", p1: "kubecon", p2: strings.Repeat("b", 16), wantErrSubstring: "path2"},
		// Double-width: 7 CJK runes weigh 14, 8 weigh 16.
		{name: "7 wide accepted", p1: strings.Repeat("会", 7), want1: strings.Repeat("会", 7)},
		{name: "8 wide refused", p1: strings.Repeat("会", 8), wantErrSubstring: "display width of 16"},
		{name: "slash refused", p1: "kube/con", wantErrSubstring: "not accepted in a display path"},
		{name: "space refused", p1: "kube con", wantErrSubstring: "not accepted in a display path"},
		// The refusal is the class (unicode.IsSpace, unicode.IsControl), not a list of
		// ASCII spellings: shared with the Performance Max paths.
		{name: "NBSP refused", p1: "kube\u00a0con", wantErrSubstring: "not accepted in a display path"},
		{name: "ideographic space refused", p1: "kube\u3000con", wantErrSubstring: "not accepted in a display path"},
		{name: "carriage return refused", p2: "n\ra", p1: "kubecon", wantErrSubstring: "not accepted in a display path"},
		{name: "path2 without path1", p2: "na", wantErrSubstring: "path2 was supplied without path1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p1, p2, err := validateSearchAdPaths(tc.p1, tc.p2)
			if tc.wantErrSubstring != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstring) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErrSubstring)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p1 != tc.want1 || p2 != tc.want2 {
				t.Errorf("paths = %q/%q, want %q/%q", p1, p2, tc.want1, tc.want2)
			}
		})
	}
}

// The campaign-level pair reaches the single ad exactly as the probe sent it, and the
// step list records it. path2 is absent from the payload, not sent empty.
func TestCreateCampaign_SendsTheDisplayPath(t *testing.T) {
	s := &parityServer{}
	in := sampleInput()
	in.Path1 = "kubecon"
	res, err := newParityClient(t, s).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, ads, _ := s.snapshot()
	if len(ads) != 1 {
		t.Fatalf("got %d ad mutates, want 1", len(ads))
	}
	rsa := rsaOf(t, createOps(t, ads[0])[0])
	if rsa["path1"] != "kubecon" {
		t.Errorf("path1 = %v, want kubecon", rsa["path1"])
	}
	if _, present := rsa["path2"]; present {
		t.Errorf("path2 = %v, want the key absent when unset", rsa["path2"])
	}
	if !containsStep(res.Steps, "Ad display path: /kubecon") {
		t.Errorf("steps = %v, want the display path recorded", res.Steps)
	}
}

// Without paths the RSA payload carries neither key: the pre-#473 bytes.
func TestCreateCampaign_NoDisplayPathSendsNoPathKeys(t *testing.T) {
	s := &parityServer{}
	res, err := newParityClient(t, s).CreateCampaign(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, ads, _ := s.snapshot()
	rsa := rsaOf(t, createOps(t, ads[0])[0])
	for _, k := range []string{"path1", "path2"} {
		if _, present := rsa[k]; present {
			t.Errorf("%s present on an ad that set none: %v", k, rsa)
		}
	}
	for _, step := range res.Steps {
		if strings.Contains(step, "display path") {
			t.Errorf("step %q claims a display path on an ad that has none", step)
		}
	}
}

// An ad that sets neither path takes the campaign pair; an ad that sets its own uses it.
func TestCreateCampaign_AdSpecPathsOverrideOrInherit(t *testing.T) {
	s := &parityServer{}
	in := sampleInput()
	in.Path1, in.Path2 = "kubecon", "na"
	in.AdGroups = []AdGroupSpec{{
		Name: "Training",
		Ads: []AdSpec{
			{Headlines: []string{"Train with us", "Hands-on labs", "Certify"}},
			{Headlines: []string{"Level up", "Experts", "Live"}, Path1: "training"},
		},
	}}
	res, err := newParityClient(t, s).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, ads, _ := s.snapshot()
	ops := createOps(t, ads[0])
	if len(ops) != 2 {
		t.Fatalf("got %d ad operations, want 2", len(ops))
	}
	first, second := rsaOf(t, ops[0]), rsaOf(t, ops[1])
	if first["path1"] != "kubecon" || first["path2"] != "na" {
		t.Errorf("inheriting ad paths = %v/%v, want kubecon/na", first["path1"], first["path2"])
	}
	if second["path1"] != "training" {
		t.Errorf("overriding ad path1 = %v, want training", second["path1"])
	}
	if _, present := second["path2"]; present {
		t.Errorf("an ad that set its own path1 must not inherit the campaign path2, got %v", second["path2"])
	}
	if !containsStep(res.Steps, "Ad 1 display path: /kubecon/na") || !containsStep(res.Steps, "Ad 2 display path: /training") {
		t.Errorf("steps = %v, want one display-path line per ad", res.Steps)
	}
}

// A whitespace-only AdSpec path is "not set": the ad inherits the campaign pair rather
// than clearing it.
func TestCreateCampaign_BlankAdSpecPathInheritsTheCampaignPair(t *testing.T) {
	s := &parityServer{}
	in := sampleInput()
	in.Path1, in.Path2 = "kubecon", "na"
	in.AdGroups = []AdGroupSpec{{
		Name: "Training",
		Ads:  []AdSpec{{Headlines: []string{"Train with us", "Hands-on labs", "Certify"}, Path1: "  "}},
	}}
	if _, err := newParityClient(t, s).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, ads, _ := s.snapshot()
	rsa := rsaOf(t, createOps(t, ads[0])[0])
	if rsa["path1"] != "kubecon" || rsa["path2"] != "na" {
		t.Errorf("paths = %v/%v, want the inherited kubecon/na", rsa["path1"], rsa["path2"])
	}
}

// A whitespace-only path is not a path, so it does not trip the off-Search refusal.
func TestPreflight_BlankDisplayPathOffSearchIsNotAPath(t *testing.T) {
	in := demandGenInput()
	in.Path1 = "  "
	if err := NewClient(testCreds(), testAccount()).ValidateCampaignInputKind(CampaignKindDisplay, in); err != nil {
		t.Fatalf("a blank Path1 on Display was refused: %v", err)
	}
}

func TestPreflight_RefusesBadAdSpecPath(t *testing.T) {
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{{Name: "Training", Ads: []AdSpec{{Path2: "na"}}}}
	err := NewClient(testCreds(), testAccount()).ValidateCampaignInputKind(CampaignKindSearch, in)
	if err == nil || !strings.Contains(err.Error(), "path2 was supplied without path1") || !strings.Contains(err.Error(), "Training") {
		t.Fatalf("err = %v, want the path2-without-path1 refusal naming the group", err)
	}
}

// Off Search the channel is refused, and BEFORE the path is measured: a 16-character
// path on Performance Max hears about the channel, not the length.
func TestPreflight_RefusesDisplayPathsOffSearch(t *testing.T) {
	for _, kind := range []string{CampaignKindDemandGen, CampaignKindPerformanceMax, CampaignKindVideo, CampaignKindDisplay} {
		for _, in := range []CampaignInput{
			func() CampaignInput { in := demandGenInput(); in.Path1 = strings.Repeat("a", 16); return in }(),
			func() CampaignInput { in := demandGenInput(); in.Path2 = "na"; return in }(),
		} {
			err := NewClient(testCreds(), testAccount()).ValidateCampaignInputKind(kind, in)
			if err == nil || !strings.Contains(err.Error(), "display paths are not supported on "+kind) {
				t.Errorf("%s: err = %v, want the off-Search refusal", kind, err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Ad-group negative keywords
// ---------------------------------------------------------------------------

// The probe shape: {adGroup, keyword:{text, matchType}, negative:true}, no status — sent
// in the same mutate as the group's positive keywords, after them.
func TestCreateCampaign_AdGroupNegativeKeywordsRideTheCriteriaMutate(t *testing.T) {
	s := &parityServer{}
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{
		{
			Name:             "Training",
			Keywords:         []Keyword{{Text: "kubernetes training", MatchType: MatchTypeExact}},
			NegativeKeywords: []Keyword{{Text: "free", MatchType: "exact"}, {Text: "jobs", MatchType: MatchTypeExact}},
		},
		// No keywords and no negatives: no criteria mutate at all for this group.
		{Name: "Conference"},
	}
	res, err := newParityClient(t, s).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, _, criteria := s.snapshot()
	if len(criteria) != 1 {
		t.Fatalf("got %d adGroupCriteria mutates, want 1 (only the group with criteria)", len(criteria))
	}
	ops := createOps(t, criteria[0])
	if len(ops) != 3 {
		t.Fatalf("got %d operations, want the keyword then the two negatives", len(ops))
	}
	if ops[0]["status"] != StatusEnabled || ops[0]["negative"] != nil {
		t.Errorf("positive keyword op = %v, want status ENABLED and no negative key", ops[0])
	}
	for i, op := range ops[1:] {
		kw, _ := op["keyword"].(map[string]any)
		if op["negative"] != true || op["status"] != nil || kw["matchType"] != MatchTypeExact || op["adGroup"] != "customers/1234567890/adGroups/330" {
			t.Errorf("negative op %d = %v, want {adGroup, negative:true, keyword{EXACT}} with no status", i, op)
		}
	}
	group := res.AdGroups[0]
	if strings.Join(group.KeywordCriteriaIDs, ",") != "900" || strings.Join(group.NegativeKeywordCriteriaIDs, ",") != "901,902" {
		t.Errorf("group ids keyword=%v negative=%v, want [900] and [901 902]", group.KeywordCriteriaIDs, group.NegativeKeywordCriteriaIDs)
	}
	if len(res.AdGroups[1].NegativeKeywordCriteriaIDs) != 0 {
		t.Errorf("the second group asked for no negatives, got %v", res.AdGroups[1].NegativeKeywordCriteriaIDs)
	}
	if !containsStep(res.Steps, "Keyword/audience targeting attached: 1 keyword(s), 0 audience segment(s), 2 negative keyword(s)") {
		t.Errorf("steps = %v, want the negatives counted", res.Steps)
	}
}

// A group with ONLY negatives still sends its criteria mutate.
func TestCreateCampaign_AdGroupNegativesAloneStillSendTheMutate(t *testing.T) {
	s := &parityServer{}
	in := sampleInput()
	in.AdGroups = []AdGroupSpec{{Name: "Training", NegativeKeywords: []Keyword{{Text: "free", MatchType: MatchTypeExact}}}}
	res, err := newParityClient(t, s).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, _, criteria := s.snapshot()
	if len(criteria) != 1 || len(res.AdGroups[0].NegativeKeywordCriteriaIDs) != 1 {
		t.Fatalf("criteria mutates = %d, negative ids = %v; want the lone negative sent and recorded", len(criteria), res.AdGroups[0].NegativeKeywordCriteriaIDs)
	}
}

func TestValidateAdGroupPlans_RefusesBadAdGroupNegatives(t *testing.T) {
	tooMany := make([]Keyword, maxNegativeKeywords+1)
	for i := range tooMany {
		tooMany[i] = Keyword{Text: "term" + strconv.Itoa(i), MatchType: MatchTypeExact}
	}
	for name, negatives := range map[string][]Keyword{
		"bad match type": {{Text: "free", MatchType: "FUZZY"}},
		"empty text":     {{Text: " ", MatchType: MatchTypeExact}},
		"over the cap":   tooMany,
	} {
		t.Run(name, func(t *testing.T) {
			in := sampleInput()
			in.AdGroups = []AdGroupSpec{{Name: "Training", NegativeKeywords: negatives}}
			_, err := validateAdGroupPlans(campaignKindSearch, in, samplePlanBase())
			if err == nil || !strings.Contains(err.Error(), "ad group negative keyword") || !strings.Contains(err.Error(), "Training") {
				t.Fatalf("err = %v, want an ad-group-negative refusal naming the group", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Income and parental-status exclusions
// ---------------------------------------------------------------------------

func TestValidateAdGroupDemographicExclusions_ResolvesAndDedupes(t *testing.T) {
	in := CampaignInput{
		ExcludedIncomeRanges:     []string{"income_range_0_50", "UNDETERMINED", "INCOME_RANGE_UNDETERMINED", " INCOME_RANGE_90_UP "},
		ExcludedParentalStatuses: []string{"parent", "PARENT", "NOT_A_PARENT"},
	}
	plan, err := validateAdGroupDemographicExclusions(campaignKindDisplay, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var incomes, parents []string
	for _, r := range plan.incomeRanges {
		incomes = append(incomes, r.Type)
	}
	for _, p := range plan.parentalStatuses {
		parents = append(parents, p.Type)
	}
	if got := strings.Join(incomes, ","); got != "INCOME_RANGE_0_50,INCOME_RANGE_UNDETERMINED,INCOME_RANGE_90_UP" {
		t.Errorf("income ranges = %s", got)
	}
	if got := strings.Join(parents, ","); got != "PARENT,NOT_A_PARENT" {
		t.Errorf("parental statuses = %s", got)
	}
}

func TestValidateAdGroupDemographicExclusions_RefusesBadValues(t *testing.T) {
	repeat := func(v string) []string {
		out := make([]string, maxDemographicExclusions+1)
		for i := range out {
			out[i] = v
		}
		return out
	}
	cases := map[string]struct {
		in      CampaignInput
		wantErr string
	}{
		"unknown income":   {CampaignInput{ExcludedIncomeRanges: []string{"50-60"}}, "must be one of"},
		"unknown parental": {CampaignInput{ExcludedParentalStatuses: []string{"GUARDIAN"}}, "must be one of"},
		// The dotless ı upper-cases to an ASCII I, so without the non-ASCII refusal this
		// would resolve to INCOME_RANGE_0_50.
		"non-ASCII fold": {CampaignInput{ExcludedIncomeRanges: []string{"ıncome_range_0_50"}}, "must be one of"},
		"income over the cap": {
			CampaignInput{ExcludedIncomeRanges: repeat("INCOME_RANGE_0_50")},
			fmt.Sprintf("at most %d excluded income ranges are supported", maxDemographicExclusions),
		},
		"parental over the cap": {
			CampaignInput{ExcludedParentalStatuses: repeat("PARENT")},
			fmt.Sprintf("at most %d excluded parental statuses are supported", maxDemographicExclusions),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := validateAdGroupDemographicExclusions(campaignKindDisplay, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// The channel fence is the probes' and no wider: income on Search and Display,
// parental status on Display only, both refused everywhere else.
func TestValidateCampaignInputKind_DemographicExclusionChannels(t *testing.T) {
	income := func(in CampaignInput) CampaignInput {
		in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"}
		return in
	}
	parental := func(in CampaignInput) CampaignInput { in.ExcludedParentalStatuses = []string{"PARENT"}; return in }
	cases := []struct {
		kind    string
		in      CampaignInput
		wantErr string
	}{
		{CampaignKindSearch, income(sampleInput()), ""},
		{CampaignKindDisplay, income(demandGenInput()), ""},
		{CampaignKindDisplay, parental(demandGenInput()), ""},
		{CampaignKindSearch, parental(sampleInput()), "OPERATION_NOT_PERMITTED_FOR_CONTEXT"},
		{CampaignKindDemandGen, income(demandGenInput()), "income-range exclusions are not supported on " + CampaignKindDemandGen},
		{CampaignKindPerformanceMax, income(demandGenInput()), "income-range exclusions are not supported on " + CampaignKindPerformanceMax},
		{CampaignKindVideo, income(demandGenInput()), "income-range exclusions are not supported on " + CampaignKindVideo},
		{CampaignKindDemandGen, parental(demandGenInput()), "parental-status exclusions are not supported on " + CampaignKindDemandGen},
		{CampaignKindPerformanceMax, parental(demandGenInput()), "parental-status exclusions are not supported on " + CampaignKindPerformanceMax},
		{CampaignKindVideo, parental(demandGenInput()), "parental-status exclusions are not supported on " + CampaignKindVideo},
	}
	for _, tc := range cases {
		err := NewClient(testCreds(), testAccount()).ValidateCampaignInputKind(tc.kind, tc.in)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected refusal %v", tc.kind, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want one containing %q", tc.kind, err, tc.wantErr)
		}
	}
}

// On Search the income exclusion goes on EVERY ad group, inside that group's criteria
// mutate, as {adGroup, negative:true, incomeRange:{type}} with no status.
func TestCreateCampaign_IncomeExclusionsGoOnEveryAdGroup(t *testing.T) {
	s := &parityServer{}
	in := sampleInput()
	in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"}
	in.AdGroups = []AdGroupSpec{
		// A negative keyword too, so the ids must split three ways: keyword, negative, exclusion.
		{Name: "Training", Keywords: []Keyword{{Text: "kubernetes training", MatchType: MatchTypeExact}}, NegativeKeywords: []Keyword{{Text: "free", MatchType: MatchTypeExact}}},
		{Name: "Conference"},
	}
	res, err := newParityClient(t, s).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, _, criteria := s.snapshot()
	if len(criteria) != 2 {
		t.Fatalf("got %d adGroupCriteria mutates, want one per group", len(criteria))
	}
	for i, body := range criteria {
		ops := createOps(t, body)
		last := ops[len(ops)-1]
		income, _ := last["incomeRange"].(map[string]any)
		wantGroup := "customers/1234567890/adGroups/" + strconv.Itoa(330+i)
		if last["negative"] != true || last["status"] != nil || income["type"] != "INCOME_RANGE_0_50" || last["adGroup"] != wantGroup {
			t.Errorf("group %d exclusion op = %v, want {adGroup %s, negative:true, incomeRange INCOME_RANGE_0_50}", i, last, wantGroup)
		}
	}
	if strings.Join(res.AdGroups[0].NegativeKeywordCriteriaIDs, ",") != "901" {
		t.Errorf("first group negative ids = %v, want [901]", res.AdGroups[0].NegativeKeywordCriteriaIDs)
	}
	if strings.Join(res.AdGroups[0].DemographicCriteriaIDs, ",") != "902" || strings.Join(res.AdGroups[1].DemographicCriteriaIDs, ",") != "900" {
		t.Errorf("per-group demographic ids = %v / %v", res.AdGroups[0].DemographicCriteriaIDs, res.AdGroups[1].DemographicCriteriaIDs)
	}
	if strings.Join(res.AdGroupDemographicCriteriaIDs, ",") != "902" {
		t.Errorf("AdGroupDemographicCriteriaIDs = %v, want the first group's", res.AdGroupDemographicCriteriaIDs)
	}
	if !containsStep(res.Steps, "1 income/parental exclusion(s)") {
		t.Errorf("steps = %v, want the exclusions counted", res.Steps)
	}
}

// A definite refusal of a group's criteria leaves the campaign, group and ad on the
// result and is not UNCONFIRMED; a 5xx is.
func TestCreateCampaign_AdGroupCriteriaFailureIsAPartialResult(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		wantUnconfirmed bool
	}{
		{"definite 4xx", http.StatusBadRequest, false},
		{"ambiguous 5xx", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &parityServer{criteriaReply: func([]byte) (int, string) {
				return tc.status, `{"error":{"code":3,"status":"INVALID_ARGUMENT","message":"boom"}}`
			}}
			in := sampleInput()
			in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"}
			in.AdGroups = []AdGroupSpec{{Name: "Training", NegativeKeywords: []Keyword{{Text: "free", MatchType: MatchTypeExact}}}}
			res, err := newParityClient(t, s).CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("expected the criteria failure to surface")
			}
			if res == nil || res.CampaignID != "222" || res.AdID == "" {
				t.Fatalf("res = %+v, want the campaign, ad group and ad kept on the partial result", res)
			}
			if got := strings.Contains(err.Error(), "UNCONFIRMED"); got != tc.wantUnconfirmed {
				t.Errorf("err = %v, UNCONFIRMED = %v, want %v", err, got, tc.wantUnconfirmed)
			}
			ag := res.AdGroups[0]
			if len(ag.NegativeKeywordCriteriaIDs)+len(ag.DemographicCriteriaIDs)+len(res.AdGroupDemographicCriteriaIDs) != 0 {
				t.Errorf("no criterion id may be recorded for a failed mutate: %+v", res.AdGroups[0])
			}
		})
	}
}

// Display: the exclusions are one adGroupCriteria:mutate right after the ad group, with
// both kinds negative and no status.
func TestCreateDisplayCampaign_AppliesAdGroupExclusions(t *testing.T) {
	s := &parityServer{}
	in := demandGenInput()
	in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"}
	in.ExcludedParentalStatuses = []string{"NOT_A_PARENT"}
	res, err := newParityClient(t, s).CreateDisplayCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateDisplayCampaign: %v", err)
	}
	paths, _, criteria := s.snapshot()
	if got := strings.Join(paths, " "); got != "campaignBudgets:mutate campaigns:mutate adGroups:mutate adGroupCriteria:mutate" {
		t.Errorf("mutate order = %s", got)
	}
	ops := createOps(t, criteria[0])
	if len(ops) != 2 {
		t.Fatalf("got %d exclusion operations, want 2", len(ops))
	}
	income, _ := ops[0]["incomeRange"].(map[string]any)
	parental, _ := ops[1]["parentalStatus"].(map[string]any)
	if ops[0]["negative"] != true || ops[0]["status"] != nil || income["type"] != "INCOME_RANGE_0_50" {
		t.Errorf("income op = %v", ops[0])
	}
	if ops[1]["negative"] != true || ops[1]["status"] != nil || parental["type"] != "NOT_A_PARENT" {
		t.Errorf("parental op = %v", ops[1])
	}
	if strings.Join(res.AdGroupDemographicCriteriaIDs, ",") != "900,901" {
		t.Errorf("AdGroupDemographicCriteriaIDs = %v, want [900 901]", res.AdGroupDemographicCriteriaIDs)
	}
	if !containsStep(res.Steps, "Ad group exclusions applied: 2 criteria (1 income range exclusion(s); 1 parental status exclusion(s))") {
		t.Errorf("steps = %v", res.Steps)
	}
}

func TestCreateDisplayCampaign_NoExclusionsSendsNoCriteriaMutate(t *testing.T) {
	s := &parityServer{}
	if _, err := newParityClient(t, s).CreateDisplayCampaign(context.Background(), demandGenInput()); err != nil {
		t.Fatalf("CreateDisplayCampaign: %v", err)
	}
	if _, _, criteria := s.snapshot(); len(criteria) != 0 {
		t.Errorf("got %d adGroupCriteria mutates with no exclusions asked for", len(criteria))
	}
}

// Display exclusions follow the partial-result contract: the ad group stays on the
// result, nothing after it runs, a 5xx or a foreign-account criterion is UNCONFIRMED and
// a 4xx is not.
func TestCreateDisplayCampaign_ExclusionFailureIsAPartialResult(t *testing.T) {
	for _, tc := range []struct {
		name            string
		reply           func([]byte) (int, string)
		wantUnconfirmed bool
	}{
		{"definite 4xx", func([]byte) (int, string) {
			return http.StatusBadRequest, `{"error":{"code":3,"status":"INVALID_ARGUMENT","message":"boom"}}`
		}, false},
		{"ambiguous 5xx", func([]byte) (int, string) { return http.StatusBadGateway, `` }, true},
		{"foreign account", func([]byte) (int, string) {
			return http.StatusOK, `{"results":[{"resourceName":"customers/9999999999/adGroupCriteria/330~900"}]}`
		}, true},
		{"short response", func([]byte) (int, string) { return http.StatusOK, `{"results":[]}` }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &parityServer{criteriaReply: tc.reply}
			in := demandGenInput()
			in.ExcludedParentalStatuses = []string{"PARENT"}
			res, err := newParityClient(t, s).CreateDisplayCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("expected the exclusion failure to surface")
			}
			if res == nil || res.AdGroupID != "330" {
				t.Fatalf("res = %+v, want the ad group kept on the partial result", res)
			}
			if len(res.AdGroupDemographicCriteriaIDs) != 0 {
				t.Errorf("ids recorded for a failed mutate: %v", res.AdGroupDemographicCriteriaIDs)
			}
			if got := strings.Contains(err.Error(), "UNCONFIRMED"); got != tc.wantUnconfirmed {
				t.Errorf("err = %v, UNCONFIRMED = %v, want %v", err, got, tc.wantUnconfirmed)
			}
		})
	}
}

// displayExclusionServer serves a Display cascade WITH a creative: the images, the shell
// mutates, the assets and the ad from displayCascade, and the ad-group criteria mutate
// from criteria. It records every mutate path in order.
func displayExclusionServer(t *testing.T, criteria http.HandlerFunc) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		paths []string
	)
	adGroupH, _ := capturedMutate(1, displayAdGroupResource)
	assetH, _ := displayAssets()
	adH, _ := capturedMutate(1, displayAdResource)
	cascade := displayCascade(t, displayImages(t), adGroupH, assetH, adH)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":mutate") {
			mu.Lock()
			paths = append(paths, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			mu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate") {
			criteria(w, r)
			return
		}
		cascade(w, r)
	})
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// With a creative, the exclusions run right after the ad group, before the images and
// the ad, so a refused exclusion leaves nothing more built on the group.
func TestCreateDisplayCampaign_ExclusionsRunBeforeTheCreative(t *testing.T) {
	srv, paths := displayExclusionServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, criteriaEcho(body))
	})
	in := displayInput(srv.URL)
	in.ExcludedParentalStatuses = []string{"PARENT"}
	res, err := demandGenClient(t, srv).CreateDisplayCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateDisplayCampaign: %v", err)
	}
	want := "campaignBudgets:mutate campaigns:mutate adGroups:mutate adGroupCriteria:mutate assets:mutate adGroupAds:mutate"
	if got := strings.Join(paths(), " "); got != want {
		t.Errorf("mutate order = %s, want %s", got, want)
	}
	if res.AdID == "" || len(res.CreativeAssetIDs) == 0 || len(res.AdGroupDemographicCriteriaIDs) != 1 {
		t.Errorf("res = %+v, want the exclusion, the assets and the ad all recorded", res)
	}
}

func TestCreateDisplayCampaign_ExclusionFailureSendsNoCreative(t *testing.T) {
	srv, paths := displayExclusionServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT","message":"boom"}}`)
	})
	in := displayInput(srv.URL)
	in.ExcludedParentalStatuses = []string{"PARENT"}
	res, err := demandGenClient(t, srv).CreateDisplayCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected the exclusion failure to surface")
	}
	for _, p := range paths() {
		if p == "assets:mutate" || p == "adGroupAds:mutate" {
			t.Errorf("%s was sent after the exclusions failed (paths %v)", p, paths())
		}
	}
	if res == nil || res.AdGroupID == "" {
		t.Fatalf("res = %+v, want the ad group kept on the partial result", res)
	}
	if len(res.CreativeAssetIDs) != 0 || res.AdID != "" {
		t.Errorf("creative recorded after a failed exclusion: assets %v, ad %q", res.CreativeAssetIDs, res.AdID)
	}
}

func containsStep(steps []string, want string) bool {
	for _, s := range steps {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

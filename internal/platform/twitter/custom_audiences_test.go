// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func audienceBody(els ...string) string {
	return `{"data":[` + strings.Join(els, ",") + `],"next_cursor":null}`
}

func audienceEl(id string, targetable, deleted bool, reasons ...string) string {
	rs := "[]"
	if len(reasons) > 0 {
		rs = `["` + strings.Join(reasons, `","`) + `"]`
	}
	return fmt.Sprintf(`{"id":%q,"name":"Aud %s","audience_type":"WEB","targetable":%t,"deleted":%t,"reasons_not_targetable":%s}`, id, id, targetable, deleted, rs)
}

func TestValidateCustomAudienceIDs(t *testing.T) {
	over := make([]string, MaxCustomAudienceIDs+1)
	for i := range over {
		over[i] = fmt.Sprintf("a%d", i)
	}
	for _, tc := range []struct {
		name             string
		include, exclude []string
		want             string // "" = accepted
	}{
		{"both empty", nil, nil, ""},
		{"valid", []string{"1nmth", "ABC123"}, []string{"2906h"}, ""},
		{"blank", []string{""}, nil, "customAudienceIds: entry 1 is not an X custom audience id"},
		{"padded is not trimmed", []string{" 1nmth"}, nil, "customAudienceIds: entry 1 is not"},
		{"punctuation", nil, []string{"ok", "a_b"}, "excludedCustomAudienceIds: entry 2 is not"},
		{"too long", []string{strings.Repeat("a", 33)}, nil, "customAudienceIds: entry 1 is not"},
		{"repeat", []string{"a", "a"}, nil, "customAudienceIds: entry 2 repeats"},
		{"case differs is not a repeat", []string{"a", "A"}, nil, ""},
		{"overlap", []string{"a1"}, []string{"b", "a1"}, "excludedCustomAudienceIds: entry 2 is also in customAudienceIds"},
		{"over the cap", over, nil, fmt.Sprintf("customAudienceIds: holds %d ids; at most %d", MaxCustomAudienceIDs+1, MaxCustomAudienceIDs)},
		{"excluded over the cap", nil, over, fmt.Sprintf("excludedCustomAudienceIds: holds %d ids", MaxCustomAudienceIDs+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCustomAudienceIDs(tc.include, tc.exclude)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var tErr *TargetingError
			if !errors.As(err, &tErr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a TargetingError containing %q", err, tc.want)
			}
		})
	}
}

func audienceInput(include, exclude []string) CampaignInput {
	in := twitterBiddingInput(0)
	in.CustomAudienceIDs, in.ExcludedCustomAudienceIDs = include, exclude
	return in
}

// The audiences are confirmed with one custom_audiences read before anything is created, then
// posted on the new line item: the exclusion (NE) first of all the criteria, the inclusions (EQ)
// after the location and language criteria, all before the promoted tweet.
func TestCreateCampaign_CustomAudienceCriteriaOrderAndOperators(t *testing.T) {
	f := newTargetingFakeX()
	f.audiences = audienceBody(audienceEl("1nmth", true, false), audienceEl("2906h", true, false))
	c := targetingClient(t, f)
	in := audienceInput([]string{"1nmth"}, []string{"2906h"})
	in.GeoTargets = []string{"US"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, lookups := f.snapshot()
	want := []string{
		"POST campaigns",
		"POST line_items",
		"POST targeting_criteria CUSTOM_AUDIENCE=2906h",
		"POST targeting_criteria LOCATION=96683cc9126741d1",
		"POST targeting_criteria CUSTOM_AUDIENCE=1nmth",
		"POST promoted_tweets",
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	if got := strings.Join(f.ops(), ","); got != "NE,EQ,EQ" {
		t.Errorf("operators = %q, want NE,EQ,EQ", got)
	}
	var lookup string
	for _, l := range lookups {
		if strings.HasPrefix(l, "custom_audiences?") {
			lookup = l
		}
	}
	if !strings.Contains(lookup, "custom_audience_ids=2906h%2C1nmth") || !strings.Contains(lookup, "with_deleted=true") {
		t.Errorf("custom audience lookup = %q", lookup)
	}
	steps := strings.Join(res.Steps, "\n")
	for _, s := range []string{"Targeting added to line item li1: excluded custom audience 2906h", "Targeting added to line item li1: custom audience 1nmth"} {
		if !strings.Contains(steps, s) {
			t.Errorf("steps lack %q: %v", s, res.Steps)
		}
	}
}

// An audience X does not return, a deleted one, or one that is not targetable refuses the create
// with nothing created; the refusal names the list, the id and X's enum-shaped reasons only.
func TestCreateCampaign_CustomAudienceRefusalsCreateNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"missing", audienceBody(audienceEl("2906h", true, false)), `invalid twitterConfig.customAudienceIds: X has no custom audience "1nmth"`},
		{"deleted", audienceBody(audienceEl("1nmth", true, true), audienceEl("2906h", true, false)), `X custom audience "1nmth" is deleted`},
		{"not targetable", audienceBody(audienceEl("1nmth", false, false, "TOO_SMALL", "free text <b>"), audienceEl("2906h", true, false)), `X custom audience "1nmth" is not targetable (TOO_SMALL)`},
		{"excluded not targetable", audienceBody(audienceEl("1nmth", true, false), audienceEl("2906h", false, false)), `invalid twitterConfig.excludedCustomAudienceIds: X custom audience "2906h" is not targetable`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTargetingFakeX()
			f.audiences = tc.body
			c := targetingClient(t, f)
			res, err := c.CreateCampaign(context.Background(), audienceInput([]string{"1nmth"}, []string{"2906h"}))
			if res != nil || err == nil {
				t.Fatalf("CreateCampaign = (%v, %v), want (nil, err)", res, err)
			}
			if !isRefusal(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "free text") {
				t.Errorf("refusal carries a non-enum reason: %v", err)
			}
			if writes, _ := f.snapshot(); len(writes) != 0 {
				t.Errorf("writes = %v, want none", writes)
			}
		})
	}
}

// A lookup that fails, or answers what cannot be believed, also creates nothing, and is not a
// refusal of the caller's value.
func TestCreateCampaign_CustomAudienceLookupFailureCreatesNothing(t *testing.T) {
	for name, set := range map[string]func(*targetingFakeX){
		"500":     func(f *targetingFakeX) { f.failAudiences = true },
		"no list": func(f *targetingFakeX) { f.audiences = `{"data":null}` },
		"listed twice": func(f *targetingFakeX) {
			f.audiences = audienceBody(audienceEl("1nmth", true, false), audienceEl("1nmth", true, false))
		},
		"duplicated key": func(f *targetingFakeX) { f.audiences = `{"data":[],"data":[]}` },
		"deleted flag absent": func(f *targetingFakeX) {
			f.audiences = audienceBody(`{"id":"1nmth","targetable":true}`)
		},
		"deleted flag null": func(f *targetingFakeX) {
			f.audiences = audienceBody(`{"id":"1nmth","targetable":true,"deleted":null}`)
		},
		"targetable flag absent": func(f *targetingFakeX) {
			f.audiences = audienceBody(`{"id":"1nmth","deleted":false}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTargetingFakeX()
			set(f)
			c := targetingClient(t, f)
			res, err := c.CreateCampaign(context.Background(), audienceInput([]string{"1nmth"}, nil))
			if res != nil || err == nil || isRefusal(err) {
				t.Fatalf("CreateCampaign = (%v, %v), want (nil, a non-refusal error)", res, err)
			}
			if writes, _ := f.snapshot(); len(writes) != 0 {
				t.Errorf("writes = %v, want none", writes)
			}
		})
	}
}

// An NE criterion echoed without operator_type is not confirmed: X's default is EQ, so the echo
// may describe a targeting criterion, the opposite of the exclusion sent. The create stops before
// the tweet.
func TestCreateCampaign_ExclusionEchoedWithoutOperatorIsUnconfirmed(t *testing.T) {
	f := newTargetingFakeX()
	f.audiences = audienceBody(audienceEl("2906h", true, false))
	f.dropNEOperator = true
	c := targetingClient(t, f)
	res, err := c.CreateCampaign(context.Background(), audienceInput(nil, []string{"2906h"}))
	if res == nil || !errors.Is(err, ErrCreateTargetingIncomplete) {
		t.Fatalf("CreateCampaign = (%v, %v), want a partial with ErrCreateTargetingIncomplete", res, err)
	}
	if !strings.Contains(err.Error(), "excluded custom audience 2906h UNCONFIRMED") {
		t.Errorf("err = %v", err)
	}
	for _, w := range func() []string { w, _ := f.snapshot(); return w }() {
		if w == "POST promoted_tweets" {
			t.Fatal("a tweet was promoted after an unconfirmed exclusion")
		}
	}
}

// A reused PAUSED line item that already holds the exclusion as NE gets only the inclusion; one
// holding the same audience as EQ does not count as holding the exclusion.
func TestMissingCriteria_ComparesTheOperator(t *testing.T) {
	excluded, included := customAudienceCriteria([]string{"1nmth"}, []string{"2906h"})
	want := append(excluded, included...)
	live := []TargetingCriterion{
		{TargetingType: TargetingCustomAudience, TargetingValue: "2906h", OperatorType: OperatorEQ},
		{TargetingType: TargetingCustomAudience, TargetingValue: "1NMTH", OperatorType: OperatorEQ},
	}
	if got := missingCriteria(want, live); len(got) != 2 {
		t.Fatalf("missing = %+v, want both (wrong operator; a case variant is another id)", got)
	}
	live = []TargetingCriterion{
		{TargetingType: TargetingCustomAudience, TargetingValue: "2906h", OperatorType: OperatorNE},
		{TargetingType: TargetingCustomAudience, TargetingValue: "1nmth"},
	}
	if got := missingCriteria(want, live); len(got) != 0 {
		t.Fatalf("missing = %+v, want none", got)
	}
}

// listAudiencesServer serves the custom_audiences list from pages keyed by cursor ("" first).
func listAudiencesServer(t *testing.T, pages map[string]string) (*Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RawQuery)
		mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/accounts/acc1/custom_audiences") || r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, ok := pages[r.URL.Query().Get("cursor")]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"}, WithBaseURL(srv.URL), WithWriteDelay(0))
	return c, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func TestListCustomAudiences_WalksPagesAndSkipsDeleted(t *testing.T) {
	c, seen := listAudiencesServer(t, map[string]string{
		"":   `{"data":[` + audienceEl("aa1", true, false) + `,` + audienceEl("dd1", true, true) + `],"next_cursor":"c2"}`,
		"c2": `{"data":[{"id":"bb2","name":" B ","audience_type":"LIST","targetable":false,"audience_size":1470,"reasons_not_targetable":["TOO_SMALL","not an enum"]}],"next_cursor":null}`,
	})
	rows, truncated, err := c.ListCustomAudiences(context.Background())
	if err != nil || truncated {
		t.Fatalf("ListCustomAudiences: truncated=%v err=%v", truncated, err)
	}
	if len(rows) != 2 || rows[0].ID != "aa1" || rows[1].ID != "bb2" || rows[1].Name != "B" || rows[1].Targetable ||
		rows[1].AudienceSize == nil || *rows[1].AudienceSize != 1470 || strings.Join(rows[1].ReasonsNotTargetable, ",") != "TOO_SMALL" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].AudienceSize != nil {
		t.Errorf("an absent audience_size must stay nil, got %v", *rows[0].AudienceSize)
	}
	if q := seen(); len(q) != 2 || !strings.Contains(q[0], "count=1000") || !strings.Contains(q[1], "cursor=c2") {
		t.Errorf("queries = %v", q)
	}
}

func TestListCustomAudiences_TruncatesAtThePageCap(t *testing.T) {
	pages := map[string]string{}
	for i := range customAudienceMaxPages + 1 {
		cur := ""
		if i > 0 {
			cur = fmt.Sprintf("c%d", i)
		}
		pages[cur] = fmt.Sprintf(`{"data":[%s],"next_cursor":"c%d"}`, audienceEl(fmt.Sprintf("a%d", i), true, false), i+1)
	}
	c, seen := listAudiencesServer(t, pages)
	rows, truncated, err := c.ListCustomAudiences(context.Background())
	if err != nil || !truncated || len(rows) != customAudienceMaxPages {
		t.Fatalf("rows=%d truncated=%v err=%v, want %d rows truncated", len(rows), truncated, err, customAudienceMaxPages)
	}
	if n := len(seen()); n != customAudienceMaxPages {
		t.Errorf("%d pages fetched, want %d", n, customAudienceMaxPages)
	}
}

func TestListCustomAudiences_RefusesWhatItCannotBelieve(t *testing.T) {
	full := make([]string, listPageSize)
	for i := range full {
		full[i] = audienceEl(fmt.Sprintf("f%d", i), true, false)
	}
	for name, pages := range map[string]map[string]string{
		"no data list":                           {"": `{"next_cursor":null}`},
		"unusable id":                            {"": audienceBody(audienceEl("a-1", true, false))},
		"listed twice":                           {"": `{"data":[` + audienceEl("a1", true, false) + `],"next_cursor":"c1"}`, "c1": audienceBody(audienceEl("a1", true, false))},
		"repeat cursor":                          {"": `{"data":[` + audienceEl("a1", true, false) + `],"next_cursor":"c1"}`, "c1": `{"data":[` + audienceEl("a2", true, false) + `],"next_cursor":"c1"}`},
		"full page without a cursor":             {"": `{"data":[` + strings.Join(full, ",") + `]}`},
		"repeat cursor on the last allowed page": repeatOnLastPage(),
		"upstream 500":                           {},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := listAudiencesServer(t, pages)
			rows, _, err := c.ListCustomAudiences(context.Background())
			if err == nil || rows != nil {
				t.Fatalf("rows=%v err=%v, want nil and an error", rows, err)
			}
		})
	}
	// A short page with no cursor key is X's documented last page.
	c, _ := listAudiencesServer(t, map[string]string{"": `{"data":[` + audienceEl("a1", true, false) + `]}`})
	if rows, _, err := c.ListCustomAudiences(context.Background()); err != nil || len(rows) != 1 {
		t.Fatalf("short page: rows=%v err=%v", rows, err)
	}
	// An empty account is an empty, non-nil list.
	c, _ = listAudiencesServer(t, map[string]string{"": audienceBody()})
	if rows, _, err := c.ListCustomAudiences(context.Background()); err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty: rows=%v err=%v", rows, err)
	}
}

// repeatOnLastPage serves customAudienceMaxPages pages whose last one repeats an earlier cursor:
// the walk must refuse it, not report a truncated list.
func repeatOnLastPage() map[string]string {
	pages := map[string]string{}
	for i := range customAudienceMaxPages {
		cur, next := "", fmt.Sprintf("c%d", i+1)
		if i > 0 {
			cur = fmt.Sprintf("c%d", i)
		}
		if i == customAudienceMaxPages-1 {
			next = "c3"
		}
		pages[cur] = fmt.Sprintf(`{"data":[%s],"next_cursor":%q}`, audienceEl(fmt.Sprintf("r%d", i), true, false), next)
	}
	return pages
}

// A create whose lists together name more than MaxCreateTargetingCriteria criteria is refused
// before any request, since the paced writes could not finish inside the dispatch budget.
func TestCreateCampaign_RefusesMoreCriteriaThanTheBudgetAllows(t *testing.T) {
	f := newTargetingFakeX()
	c := targetingClient(t, f)
	include := make([]string, 0, MaxCreateTargetingCriteria)
	for i := range MaxCreateTargetingCriteria {
		include = append(include, fmt.Sprintf("a%d", i))
	}
	in := audienceInput(include, nil)
	in.GeoTargets = []string{"US"}
	res, err := c.CreateCampaign(context.Background(), in)
	if res != nil || !isRefusal(err) || !strings.Contains(err.Error(), fmt.Sprintf("together name %d targeting criteria; at most %d", MaxCreateTargetingCriteria+1, MaxCreateTargetingCriteria)) {
		t.Fatalf("CreateCampaign = (%v, %v), want a refusal naming the total", res, err)
	}
	if writes, lookups := f.snapshot(); len(writes) != 0 || len(lookups) != 0 {
		t.Errorf("writes=%v lookups=%v, want nothing sent", writes, lookups)
	}
	// Exactly at the cap is accepted by the count rule.
	if err := CheckCreateCriteriaCount(MaxCreateTargetingCriteria); err != nil {
		t.Errorf("CheckCreateCriteriaCount(cap) = %v, want nil", err)
	}
}

// dispatchBudgetMirror mirrors service.providerCallTimeout (internal/service/orchestrator.go),
// which this package cannot import. KEEP IN SYNC.
const dispatchBudgetMirror = 2 * time.Minute

// createOverheadWrites bounds the other paced writes of one create: the campaign, the line item,
// the media library entry, the tweet and the promoted tweet, with room for a retry's re-read.
const createOverheadWrites = 10

// TestCreateTargetingCriteriaFitTheDispatchBudget pins the margin MaxCreateTargetingCriteria is
// sized for: every paced write of a create at the cap must finish with at least a third of the
// dispatch budget left for the unpaced reads (account, lookups, custom audience check).
func TestCreateTargetingCriteriaFitTheDispatchBudget(t *testing.T) {
	// Tweet variants (#564 gap 10): each variant past the first adds a tweet and its promotion.
	variantWrites := 2 * (MaxTweetVariants - 1)
	paced := time.Duration(MaxCreateTargetingCriteria+createOverheadWrites+variantWrites) * writeDelay
	if paced > dispatchBudgetMirror*2/3 {
		t.Fatalf("%d criteria + %d other writes + %d variant writes at %v each take %v, more than two thirds of the %v dispatch budget",
			MaxCreateTargetingCriteria, createOverheadWrites, variantWrites, writeDelay, paced, dispatchBudgetMirror)
	}
	// A video (#564 gap 7): its upload writes (initialize, one append per segment of the largest
	// video, finalize, library add) and its bounded processing wait replace the criteria it leaves
	// out; the whole must still leave a quarter of the budget for the unpaced reads.
	videoWrites := 2 + (MaxVideoBytes+defaultMediaChunkSize-1)/defaultMediaChunkSize + 1
	withVideo := time.Duration(MaxCreateTargetingCriteriaWithVideo+createOverheadWrites+variantWrites+videoWrites)*writeDelay +
		time.Duration(maxVideoStatusPolls)*defaultMediaPollCap
	if withVideo > dispatchBudgetMirror*3/4 {
		t.Fatalf("a create with a video takes %v of paced writes and processing wait, more than three quarters of the %v dispatch budget", withVideo, dispatchBudgetMirror)
	}
	if MaxCreateTargetingCriteriaWithVideo > MaxCreateTargetingCriteria {
		t.Fatal("the video criteria cap exceeds the per-create criteria cap")
	}
	if MaxGeoTargets > MaxCreateTargetingCriteria || MaxLanguages > MaxCreateTargetingCriteria || MaxCustomAudienceIDs > MaxCreateTargetingCriteria || MaxCreateFollowerLookalikes > MaxCreateTargetingCriteria {
		t.Fatal("a per-list cap exceeds the per-create criteria cap")
	}
}

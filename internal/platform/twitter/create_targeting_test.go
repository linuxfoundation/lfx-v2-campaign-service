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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// targetingFakeX answers a CreateCampaign plus the two targeting lookups and the criterion POSTs,
// recording the order of every mutating request.
type targetingFakeX struct {
	mu sync.Mutex
	// countries maps a country_code to the body the locations lookup answers.
	countries map[string]string
	// languagesPages are the language lookup's pages, served in order by cursor "p<i>".
	languagesPages []string
	// failCriterion makes the n-th criterion POST (1-based) answer failStatus.
	failCriterion int
	failStatus    int
	// reuseLineItem makes the line-item lookup find a same-name line item.
	reuseLineItem string
	// liveCriteria is the body of the line item's targeting-criteria list ("" = an empty list);
	// failList makes that read answer 500.
	liveCriteria string
	failList     bool
	// reusedStatus is the reused line item's entity_status ("" = PAUSED); "fail" answers 500.
	reusedStatus string

	writes    []string // "POST <kind>[ <type>=<value>]"
	lookups   []string // "<kind>?<query>"
	criterion int
}

func (f *targetingFakeX) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && p == "/12/targeting_criteria/locations":
		f.lookups = append(f.lookups, "locations?"+r.URL.RawQuery)
		body, ok := f.countries[q.Get("country_code")]
		if !ok {
			body = `{"data":[],"next_cursor":null}`
		}
		_, _ = w.Write([]byte(body))
	case r.Method == http.MethodGet && p == "/12/targeting_criteria/languages":
		f.lookups = append(f.lookups, "languages?"+r.URL.RawQuery)
		i := 0
		if c := q.Get("cursor"); c != "" {
			_, _ = fmt.Sscanf(c, "p%d", &i)
		}
		if i >= len(f.languagesPages) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(f.languagesPages[i]))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/accounts/acc1/targeting_criteria"):
		f.lookups = append(f.lookups, "criteria?"+r.URL.RawQuery)
		if f.failList {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body := f.liveCriteria
		if body == "" {
			body = `{"data":[],"next_cursor":null}`
		}
		_, _ = w.Write([]byte(body))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/accounts/acc1"):
		_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/line_items/li9"):
		f.lookups = append(f.lookups, "line_item_status")
		switch f.reusedStatus {
		case "fail":
			w.WriteHeader(http.StatusInternalServerError)
		case "":
			_, _ = w.Write([]byte(`{"data":{"id":"li9","entity_status":"PAUSED","deleted":false}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"id":"li9","entity_status":"` + f.reusedStatus + `","deleted":false}}`))
		}
	case r.Method == http.MethodGet && strings.Contains(p, "line_items") && f.reuseLineItem != "":
		_, _ = w.Write([]byte(`{"data":[{"id":"li9","name":"` + f.reuseLineItem + `"}]}`))
	case r.Method == http.MethodGet:
		_, _ = w.Write([]byte(`{"data":[]}`))
	case strings.HasSuffix(p, "/targeting_criteria"):
		f.criterion++
		f.writes = append(f.writes, "POST targeting_criteria "+q.Get("targeting_type")+"="+q.Get("targeting_value"))
		if f.criterion == f.failCriterion {
			w.WriteHeader(f.failStatus)
			_, _ = w.Write([]byte(`{"errors":[{"code":"INVALID_TARGETING_CRITERIA"}]}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{"id":"tc%d","line_item_id":%q,"targeting_type":%q,"targeting_value":%q,"deleted":false}}`,
			f.criterion, q.Get("line_item_id"), q.Get("targeting_type"), q.Get("targeting_value"))
	case strings.HasSuffix(p, "/campaigns"):
		f.writes = append(f.writes, "POST campaigns")
		_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
	case strings.HasSuffix(p, "/line_items"):
		f.writes = append(f.writes, "POST line_items")
		_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
	case strings.HasSuffix(p, "/promoted_tweets"):
		f.writes = append(f.writes, "POST promoted_tweets")
		_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *targetingFakeX) snapshot() (writes, lookups []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...), append([]string(nil), f.lookups...)
}

func countryBody(code, value string) string {
	return `{"data":[{"name":"` + code + `","country_code":"` + code + `","location_type":"COUNTRIES","targeting_type":"LOCATION","targeting_value":"` + value + `"}],"next_cursor":null}`
}

const languagesEnJa = `{"data":[{"name":"English","targeting_type":"LANGUAGE","targeting_value":"en"},{"name":"Japanese","targeting_type":"LANGUAGE","targeting_value":"ja"}],"next_cursor":null}`

func newTargetingFakeX() *targetingFakeX {
	return &targetingFakeX{
		countries:      map[string]string{"US": countryBody("US", "96683cc9126741d1"), "CA": countryBody("CA", "3376992a082d67c7")},
		languagesPages: []string{languagesEnJa},
	}
}

func targetingClient(t *testing.T, f *targetingFakeX) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL), WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime
	return c
}

func targetingInput(geo, langs []string) CampaignInput {
	in := twitterBiddingInput(0)
	in.GeoTargets, in.Languages = geo, langs
	return in
}

// The criteria go on the line item THIS call created, after it and before the promoted tweet,
// with X's resolved location value, EQ, and the steps naming the caller's codes.
func TestCreateCampaign_TargetingCriteriaFollowTheLineItem(t *testing.T) {
	f := newTargetingFakeX()
	c := targetingClient(t, f)
	res, err := c.CreateCampaign(context.Background(), targetingInput([]string{" us", "CA", "US"}, []string{"EN"}))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, lookups := f.snapshot()
	want := []string{
		"POST campaigns",
		"POST line_items",
		"POST targeting_criteria LOCATION=96683cc9126741d1",
		"POST targeting_criteria LOCATION=3376992a082d67c7",
		"POST targeting_criteria LANGUAGE=en",
		"POST promoted_tweets",
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	if len(lookups) != 3 || !strings.Contains(lookups[0], "country_code=US") || !strings.Contains(lookups[0], "location_type=COUNTRIES") ||
		!strings.Contains(lookups[1], "country_code=CA") || !strings.HasPrefix(lookups[2], "languages?") {
		t.Errorf("lookups = %v", lookups)
	}
	steps := strings.Join(res.Steps, "\n")
	for _, s := range []string{"Targeting added to line item li1: country US", "country CA", "language en"} {
		if !strings.Contains(steps, s) {
			t.Errorf("steps lack %q: %v", s, res.Steps)
		}
	}
}

// Every criterion is sent with operator_type=EQ on the created line item.
func TestCreateTargetingCriterion_SendsEQOnTheLineItem(t *testing.T) {
	gotCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotCh <- r.URL.RawQuery:
		default:
		}
		_, _ = w.Write([]byte(`{"data":{"id":"tc1","line_item_id":"li1","targeting_type":"LANGUAGE","targeting_value":"en","operator_type":"EQ"}}`))
	}))
	defer srv.Close()
	c := NewClient(Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"}, WithBaseURL(srv.URL), WithWriteDelay(0))
	if err := c.createTargetingCriterion(context.Background(), "li1", createCriterion{targetingType: TargetingLanguage, value: "en", label: "language en"}); err != nil {
		t.Fatalf("createTargetingCriterion: %v", err)
	}
	got := <-gotCh
	for _, s := range []string{"operator_type=EQ", "line_item_id=li1", "targeting_type=LANGUAGE", "targeting_value=en"} {
		if !strings.Contains(got, s) {
			t.Errorf("query %q lacks %s", got, s)
		}
	}
}

// No targeting asked: no lookup, no criterion — the create is exactly as before.
func TestCreateCampaign_NoTargetingMakesNoLookupsOrCriteria(t *testing.T) {
	f := newTargetingFakeX()
	c := targetingClient(t, f)
	if _, err := c.CreateCampaign(context.Background(), targetingInput(nil, nil)); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, lookups := f.snapshot()
	if len(lookups) != 0 || strings.Contains(strings.Join(writes, " "), "targeting_criteria") {
		t.Fatalf("writes=%v lookups=%v, want no targeting traffic", writes, lookups)
	}
}

// A value X does not offer, an ambiguous country, or a lookup that cannot be read stops the
// create before ANY write.
func TestCreateCampaign_UnresolvableTargetingCreatesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*targetingFakeX)
		geo     []string
		langs   []string
		refusal bool
	}{
		"unknown country": {geo: []string{"ZZ"}, refusal: true},
		"ambiguous country": {mutate: func(f *targetingFakeX) {
			f.countries["US"] = `{"data":[{"country_code":"US","location_type":"COUNTRIES","targeting_value":"a1"},{"country_code":"US","location_type":"COUNTRIES","targeting_value":"b2"}],"next_cursor":null}`
		}, geo: []string{"US"}, refusal: true},
		"only a city for the code": {mutate: func(f *targetingFakeX) {
			f.countries["US"] = `{"data":[{"country_code":"US","location_type":"CITIES","targeting_value":"c1"}],"next_cursor":null}`
		}, geo: []string{"US"}, refusal: true},
		"unknown language":    {langs: []string{"xx"}, refusal: true},
		"language read fails": {mutate: func(f *targetingFakeX) { f.languagesPages = nil }, langs: []string{"en"}},
		"location read fails": {mutate: func(f *targetingFakeX) { f.countries["US"] = `{"nodata":true}` }, geo: []string{"US"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTargetingFakeX()
			if tc.mutate != nil {
				tc.mutate(f)
			}
			c := targetingClient(t, f)
			res, err := c.CreateCampaign(context.Background(), targetingInput(tc.geo, tc.langs))
			if err == nil || res != nil {
				t.Fatalf("res=%+v err=%v, want (nil, error)", res, err)
			}
			if got := isRefusal(err); got != tc.refusal {
				t.Errorf("preflight refusal = %v, want %v (%v)", got, tc.refusal, err)
			}
			if writes, _ := f.snapshot(); len(writes) != 0 {
				t.Fatalf("writes = %v, want none", writes)
			}
		})
	}
}

// The language list is walked to its end: a value on page two is found, and a full page with no
// usable cursor is not read as the end of the list.
func TestResolveLanguages_WalksPagesAndRefusesAnUnconfirmedList(t *testing.T) {
	f := newTargetingFakeX()
	f.languagesPages = []string{
		`{"data":[{"targeting_type":"LANGUAGE","targeting_value":"en"}],"next_cursor":"p1"}`,
		`{"data":[{"targeting_type":"LANGUAGE","targeting_value":"ja"}],"next_cursor":null}`,
	}
	c := targetingClient(t, f)
	if err := c.resolveLanguages(context.Background(), []string{"ja", "en"}); err != nil {
		t.Fatalf("resolveLanguages across two pages: %v", err)
	}

	full := make([]string, listPageSize)
	for i := range full {
		full[i] = fmt.Sprintf(`{"targeting_type":"LANGUAGE","targeting_value":"l%d"}`, i)
	}
	f2 := newTargetingFakeX()
	f2.languagesPages = []string{`{"data":[` + strings.Join(full, ",") + `]}`}
	c2 := targetingClient(t, f2)
	err := c2.resolveLanguages(context.Background(), []string{"l1"})
	if err == nil || isRefusal(err) {
		t.Fatalf("a full page with no cursor must not confirm the list, got %v", err)
	}
}

// A criterion POST that fails stops the create BEFORE the promoted tweet: the partial result
// names the PAUSED campaign and line item, and the error says how many criteria were applied.
func TestCreateCampaign_TargetingFailureStopsBeforeTheTweet(t *testing.T) {
	for _, tc := range []struct {
		status      int
		unconfirmed bool
	}{{http.StatusBadRequest, false}, {http.StatusBadGateway, true}, {http.StatusTooManyRequests, true}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			f := newTargetingFakeX()
			f.failCriterion, f.failStatus = 2, tc.status
			c := targetingClient(t, f)
			res, err := c.CreateCampaign(context.Background(), targetingInput([]string{"US", "CA"}, []string{"en"}))
			if err == nil || res == nil || res.CampaignID != "cmp1" || res.LineItemID != "li1" {
				t.Fatalf("res=%+v err=%v, want the partial result and an error", res, err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "1 of 3 criteria applied") || !strings.Contains(msg, "country CA") || !strings.Contains(msg, "do not enable the line item") {
				t.Errorf("error = %q", msg)
			}
			if got := strings.Contains(msg, "UNCONFIRMED"); got != tc.unconfirmed {
				t.Errorf("UNCONFIRMED wording = %v, want %v: %q", got, tc.unconfirmed, msg)
			}
			writes, _ := f.snapshot()
			if strings.Contains(strings.Join(writes, " "), "promoted_tweets") || len(writes) != 4 {
				t.Fatalf("writes = %v, want campaign, line item and two criteria, no tweet", writes)
			}
		})
	}
}

// A REUSED line item — the retry of a create whose targeting stopped part-way — gets the criteria
// it does not already hold, and only those, before the promoted tweet.
func TestCreateCampaign_ReusedLineItemGetsOnlyTheMissingCriteria(t *testing.T) {
	f := newTargetingFakeX()
	f.reuseLineItem = "Events | KubeCon | Promoted Tweets | AUTO"
	f.liveCriteria = `{"data":[{"id":"tc1","line_item_id":"li9","targeting_type":"LOCATION","targeting_value":"96683cc9126741d1","operator_type":"EQ","deleted":false},` +
		`{"id":"tc2","line_item_id":"li9","targeting_type":"LOCATION","targeting_value":"3376992a082d67c7","operator_type":"NE","deleted":false}],"next_cursor":null}`
	c := targetingClient(t, f)
	res, err := c.CreateCampaign(context.Background(), targetingInput([]string{"US", "CA"}, []string{"en"}))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, _ := f.snapshot()
	want := "POST campaigns\nPOST targeting_criteria LOCATION=3376992a082d67c7\nPOST targeting_criteria LANGUAGE=en\nPOST promoted_tweets"
	if got := strings.Join(writes, "\n"); got != want {
		t.Fatalf("writes =\n%s\nwant\n%s (a negated CA is not the CA targeting asked for)", got, want)
	}
	if !res.Reused || !strings.Contains(strings.Join(res.Steps, "\n"), "already holds 1 of the 3 requested targeting criteria") {
		t.Fatalf("Reused=%v steps=%v", res.Reused, res.Steps)
	}
}

// A reused line item whose criteria cannot be read stops the create before the tweet.
func TestCreateCampaign_ReusedLineItemUnreadableTargetingStopsBeforeTheTweet(t *testing.T) {
	f := newTargetingFakeX()
	f.reuseLineItem = "Events | KubeCon | Promoted Tweets | AUTO"
	f.failList = true
	c := targetingClient(t, f)
	res, err := c.CreateCampaign(context.Background(), targetingInput([]string{"US"}, nil))
	if err == nil || res == nil || res.LineItemID != "li9" || !strings.Contains(err.Error(), "targeting not confirmed") {
		t.Fatalf("res=%+v err=%v, want the partial result and the not-confirmed error", res, err)
	}
	writes, _ := f.snapshot()
	if strings.Contains(strings.Join(writes, " "), "promoted_tweets") || strings.Contains(strings.Join(writes, " "), "targeting_criteria") {
		t.Fatalf("writes = %v, want no criterion and no tweet", writes)
	}
}

// A reused line item that already holds every criterion gets none and goes on to the tweet.
func TestCreateCampaign_ReusedLineItemFullyTargetedPostsNoCriteria(t *testing.T) {
	f := newTargetingFakeX()
	f.reuseLineItem = "Events | KubeCon | Promoted Tweets | AUTO"
	f.liveCriteria = `{"data":[{"id":"tc1","line_item_id":"li9","targeting_type":"LOCATION","targeting_value":"96683cc9126741d1","deleted":false}],"next_cursor":null}`
	c := targetingClient(t, f)
	if _, err := c.CreateCampaign(context.Background(), targetingInput([]string{"US"}, nil)); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, _ := f.snapshot()
	if got := strings.Join(writes, ","); got != "POST campaigns,POST promoted_tweets" {
		t.Fatalf("writes = %s", got)
	}
}

// Two distinct values among three entries are counted as two.
func TestResolveCountry_CountsDistinctValues(t *testing.T) {
	f := newTargetingFakeX()
	f.countries["US"] = `{"data":[{"country_code":"US","location_type":"COUNTRIES","targeting_value":"a1"},{"country_code":"US","location_type":"COUNTRIES","targeting_value":"b2"},{"country_code":"US","location_type":"COUNTRIES","targeting_value":"b2"}],"next_cursor":null}`
	c := targetingClient(t, f)
	_, err := c.resolveCountry(context.Background(), "US")
	if err == nil || !strings.Contains(err.Error(), "2 different country locations") {
		t.Fatalf("err = %v, want the 2-value refusal", err)
	}
}

func TestNormalizeCreateTargeting(t *testing.T) {
	geo, langs, err := NormalizeCreateTargeting([]string{" us", "CA", "Us"}, []string{"EN", " ja ", "en", "zh-cn"})
	if err != nil || strings.Join(geo, ",") != "US,CA" || strings.Join(langs, ",") != "en,ja,zh-cn" {
		t.Fatalf("= %v %v %v", geo, langs, err)
	}
	if g, l, err := NormalizeCreateTargeting(nil, []string{}); g != nil || l != nil || err != nil {
		t.Fatalf("empty = %v %v %v, want nil nil nil", g, l, err)
	}
	many := make([]string, MaxGeoTargets+1)
	for i := range many {
		many[i] = fmt.Sprintf("%c%c", 'A'+i/26, 'A'+i%26)
	}
	for name, tc := range map[string]struct {
		geo, langs []string
		field      string
	}{
		"blank country":      {geo: []string{"US", " "}, field: FieldGeoTargets},
		"alpha-3 country":    {geo: []string{"USA"}, field: FieldGeoTargets},
		"digit country":      {geo: []string{"U1"}, field: FieldGeoTargets},
		"too many countries": {geo: many, field: FieldGeoTargets},
		"blank language":     {langs: []string{""}, field: FieldLanguages},
		"one-letter lang":    {langs: []string{"e"}, field: FieldLanguages},
		"lang with space":    {langs: []string{"en us"}, field: FieldLanguages},
		"lang too long":      {langs: []string{"abcdefghi"}, field: FieldLanguages},
	} {
		_, _, err := NormalizeCreateTargeting(tc.geo, tc.langs)
		var te *TargetingError
		if !errors.As(err, &te) || te.Field != tc.field {
			t.Errorf("%s: err = %v, want a TargetingError on %s", name, err, tc.field)
		}
	}
	// Duplicates do not count toward the cap.
	dups := make([]string, MaxGeoTargets+5)
	for i := range dups {
		dups[i] = "US"
	}
	if g, _, err := NormalizeCreateTargeting(dups, nil); err != nil || len(g) != 1 {
		t.Errorf("duplicates = %v %v, want one entry", g, err)
	}
}

// CreateCampaign applies the same pure rules before any request.
func TestCreateCampaign_MalformedTargetingIsRefusedBeforeAnyRequest(t *testing.T) {
	f := newTargetingFakeX()
	c := targetingClient(t, f)
	_, err := c.CreateCampaign(context.Background(), targetingInput([]string{"USA"}, nil))
	if !isRefusal(err) || !strings.Contains(err.Error(), "twitterConfig.geoTargets") {
		t.Fatalf("err = %v", err)
	}
	if writes, lookups := f.snapshot(); len(writes)+len(lookups) != 0 {
		t.Fatalf("writes=%v lookups=%v, want nothing sent", writes, lookups)
	}
}

func isRefusal(err error) bool {
	var r *domain.PreflightRefusal
	return errors.As(err, &r)
}

// A reused line item that is not PAUSED (or whose status cannot be read) is not re-targeted and
// gets no tweet: changing a possibly-serving audience is for an operator.
func TestCreateCampaign_ReusedLineItemMustBePausedToBeRetargeted(t *testing.T) {
	for status, want := range map[string]string{"ACTIVE": "it is ACTIVE, not PAUSED", "WEIRD<b>": "in an unrecognised state", "fail": "status could not be read"} {
		t.Run(status, func(t *testing.T) {
			f := newTargetingFakeX()
			f.reuseLineItem = "Events | KubeCon | Promoted Tweets | AUTO"
			f.reusedStatus = status
			c := targetingClient(t, f)
			res, err := c.CreateCampaign(context.Background(), targetingInput([]string{"US"}, nil))
			if err == nil || res == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("res=%+v err=%v, want %q", res, err, want)
			}
			if strings.Contains(err.Error(), "WEIRD") {
				t.Errorf("upstream status text leaked: %v", err)
			}
			writes, lookups := f.snapshot()
			if strings.Contains(strings.Join(writes, " "), "targeting_criteria") || strings.Contains(strings.Join(writes, " "), "promoted_tweets") {
				t.Fatalf("writes = %v, want no criterion and no tweet", writes)
			}
			if strings.Contains(strings.Join(lookups, " "), "criteria?") {
				t.Errorf("criteria were read before the status allowed it: %v", lookups)
			}
		})
	}
}

// A full country page with no cursor cannot confirm "exactly one": an unreadable lookup.
func TestResolveCountry_FullPageWithoutCursorIsUnreadable(t *testing.T) {
	entries := make([]string, listPageSize)
	for i := range entries {
		entries[i] = `{"country_code":"US","location_type":"CITIES","targeting_value":"c` + fmt.Sprint(i) + `"}`
	}
	entries[0] = `{"country_code":"US","location_type":"COUNTRIES","targeting_value":"us1"}`
	f := newTargetingFakeX()
	f.countries["US"] = `{"data":[` + strings.Join(entries, ",") + `]}`
	c := targetingClient(t, f)
	_, err := c.resolveCountry(context.Background(), "US")
	if err == nil || isRefusal(err) || !strings.Contains(err.Error(), "cannot be confirmed complete") {
		t.Fatalf("err = %v, want the unreadable-lookup error", err)
	}
}

// A 2xx whose echo does not confirm the criterion sent is UNCONFIRMED and stops the create before
// the tweet.
func TestCreateCampaign_UnprovenCriterionEchoStopsBeforeTheTweet(t *testing.T) {
	for name, body := range map[string]string{
		"no data":         `{"data":null}`,
		"no id":           `{"data":{"line_item_id":"li1","targeting_type":"LOCATION","targeting_value":"96683cc9126741d1"}}`,
		"other line item": `{"data":{"id":"tc1","line_item_id":"li2","targeting_type":"LOCATION","targeting_value":"96683cc9126741d1"}}`,
		"other type":      `{"data":{"id":"tc1","line_item_id":"li1","targeting_type":"LANGUAGE","targeting_value":"96683cc9126741d1"}}`,
		"other value":     `{"data":{"id":"tc1","line_item_id":"li1","targeting_type":"LOCATION","targeting_value":"zzz"}}`,
		"negated":         `{"data":{"id":"tc1","line_item_id":"li1","targeting_type":"LOCATION","targeting_value":"96683cc9126741d1","operator_type":"NE"}}`,
		"deleted":         `{"data":{"id":"tc1","line_item_id":"li1","targeting_type":"LOCATION","targeting_value":"96683cc9126741d1","deleted":true}}`,
		"duplicate key":   `{"data":{"id":"tc1","id":"tc2","line_item_id":"li1","targeting_type":"LOCATION","targeting_value":"96683cc9126741d1"}}`,
		"not an object":   `{"data":[1]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var writes []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/12/targeting_criteria/locations":
					_, _ = w.Write([]byte(countryBody("US", "96683cc9126741d1")))
				case r.Method == http.MethodGet:
					_, _ = w.Write([]byte(`{"data":[]}`))
				case strings.HasSuffix(r.URL.Path, "/targeting_criteria"):
					writes = append(writes, "criterion")
					_, _ = w.Write([]byte(body))
				case strings.HasSuffix(r.URL.Path, "/campaigns"):
					_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
				case strings.HasSuffix(r.URL.Path, "/line_items"):
					_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
				default:
					writes = append(writes, r.URL.Path)
					_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
				}
			}))
			defer srv.Close()
			c := NewClient(Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
				AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"}, WithBaseURL(srv.URL), WithWriteDelay(0))
			c.timeFn = staticTime
			res, err := c.CreateCampaign(context.Background(), targetingInput([]string{"US"}, nil))
			if err == nil || res == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
				t.Fatalf("res=%+v err=%v, want an UNCONFIRMED partial result", res, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(writes) != 1 || writes[0] != "criterion" {
				t.Fatalf("writes after the line item = %v, want only the criterion (no tweet)", writes)
			}
		})
	}
}

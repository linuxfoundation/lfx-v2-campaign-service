// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func goodSitelinks() []Sitelink {
	return []Sitelink{
		{Text: " Schedule ", Description1: "Five days of sessions", Description2: "Keynotes, talks and labs", FinalURL: "https://events.example.org/schedule/"},
		{Text: "Register", FinalURL: "https://events.example.org/register/"},
	}
}

func goodCallouts() []string { return []string{"Early Bird Pricing", " 300+ Sessions "} }

func TestValidateTextExtensions_AcceptsAndTrims(t *testing.T) {
	links, calls, err := ValidateTextExtensions(goodSitelinks(), goodCallouts())
	if err != nil {
		t.Fatalf("ValidateTextExtensions: %v", err)
	}
	if links[0].Text != "Schedule" || calls[1] != "300+ Sessions" {
		t.Errorf("values must be trimmed: %+v %q", links, calls)
	}
	if l, c, err := ValidateTextExtensions(nil, nil); err != nil || l != nil || c != nil {
		t.Errorf("nothing requested is valid and empty, got %v %v %v", l, c, err)
	}
	// The documented boundaries are accepted.
	edge := []Sitelink{
		{Text: strings.Repeat("a", 35), FinalURL: "http://x.example"},
		{Text: strings.Repeat("b", 25), Description1: strings.Repeat("c", 35), Description2: strings.Repeat("d", 35), FinalURL: "https://x.example/" + strings.Repeat("p", 2048-len("https://x.example/"))},
		{Text: strings.Repeat("中", 15), FinalURL: "https://x.example"},
		{Text: strings.Repeat("中", 11), Description1: strings.Repeat("文", 15), Description2: "ok", FinalURL: "https://x.example"},
	}
	// The URL limit is in characters: a 2,048-character URL with a CJK path is accepted even
	// though it is far more than 2,048 bytes.
	edge = append(edge, Sitelink{Text: "CJK path", FinalURL: "https://x.example/" + strings.Repeat("中", 2048-len("https://x.example/"))})
	if _, _, err := ValidateTextExtensions(edge, []string{strings.Repeat("e", 25), strings.Repeat("字", 12)}); err != nil {
		t.Errorf("boundary values must pass: %v", err)
	}
	max := make([]Sitelink, MaxSitelinks)
	maxC := make([]string, MaxCallouts)
	for i := range max {
		max[i] = Sitelink{Text: fmt.Sprintf("Link %d", i), FinalURL: "https://x.example"}
		maxC[i] = fmt.Sprintf("Callout %d", i)
	}
	if _, _, err := ValidateTextExtensions(max, maxC); err != nil {
		t.Errorf("%d sitelinks and %d callouts must pass: %v", MaxSitelinks, MaxCallouts, err)
	}
}

func TestValidateTextExtensions_Refusals(t *testing.T) {
	link := func(mut func(*Sitelink)) []Sitelink {
		s := Sitelink{Text: "Schedule", FinalURL: "https://x.example"}
		mut(&s)
		return []Sitelink{s}
	}
	tooMany := make([]Sitelink, MaxSitelinks+1)
	for i := range tooMany {
		tooMany[i] = Sitelink{Text: fmt.Sprintf("L%d", i), FinalURL: "https://x.example"}
	}
	tooManyC := make([]string, MaxCallouts+1)
	for i := range tooManyC {
		tooManyC[i] = fmt.Sprintf("C%d", i)
	}
	cases := map[string]struct {
		links    []Sitelink
		callouts []string
		field    string
		want     string
	}{
		"too many sitelinks":        {tooMany, nil, FieldSitelinks, "at most 20"},
		"empty text":                {link(func(s *Sitelink) { s.Text = "  " }), nil, FieldSitelinks, "[0].text is empty"},
		"control char in text":      {link(func(s *Sitelink) { s.Text = "Sched\nule" }), nil, FieldSitelinks, "control character"},
		"text over 35":              {link(func(s *Sitelink) { s.Text = strings.Repeat("a", 36) }), nil, FieldSitelinks, "limit is 35"},
		"text over 25 with desc":    {link(func(s *Sitelink) { s.Text = strings.Repeat("a", 26); s.Description1 = "x"; s.Description2 = "y" }), nil, FieldSitelinks, "limit is 25"},
		"wide text over 15":         {link(func(s *Sitelink) { s.Text = strings.Repeat("中", 16) }), nil, FieldSitelinks, "limit is 15"},
		"wide text over 11 w/ desc": {link(func(s *Sitelink) { s.Text = strings.Repeat("中", 12); s.Description1 = "x"; s.Description2 = "y" }), nil, FieldSitelinks, "limit is 11"},
		"description1 alone":        {link(func(s *Sitelink) { s.Description1 = "x" }), nil, FieldSitelinks, "set together"},
		"description2 alone":        {link(func(s *Sitelink) { s.Description2 = "x" }), nil, FieldSitelinks, "set together"},
		"description over 35":       {link(func(s *Sitelink) { s.Description1 = strings.Repeat("a", 36); s.Description2 = "y" }), nil, FieldSitelinks, "description1 is 36"},
		"description2 over 35":      {link(func(s *Sitelink) { s.Description1 = "x"; s.Description2 = strings.Repeat("a", 36) }), nil, FieldSitelinks, "description2 is 36"},
		"wide description over 15":  {link(func(s *Sitelink) { s.Description1 = "x"; s.Description2 = strings.Repeat("中", 16) }), nil, FieldSitelinks, "limit is 15"},
		"control char in desc":      {link(func(s *Sitelink) { s.Description1 = "a\tb"; s.Description2 = "y" }), nil, FieldSitelinks, "description1 contains a control character"},
		"missing url":               {link(func(s *Sitelink) { s.FinalURL = "" }), nil, FieldSitelinks, "finalUrl is required"},
		"relative url":              {link(func(s *Sitelink) { s.FinalURL = "/register" }), nil, FieldSitelinks, "absolute http or https"},
		"ftp url":                   {link(func(s *Sitelink) { s.FinalURL = "ftp://x.example" }), nil, FieldSitelinks, "absolute http or https"},
		"url with space":            {link(func(s *Sitelink) { s.FinalURL = "https://x.example/a b" }), nil, FieldSitelinks, "whitespace"},
		"url with credentials":      {link(func(s *Sitelink) { s.FinalURL = "https://user:pw@x.example/register" }), nil, FieldSitelinks, "must not carry user credentials"},
		"url user only":             {link(func(s *Sitelink) { s.FinalURL = "https://user@x.example/register" }), nil, FieldSitelinks, "must not carry user credentials"},
		"non-ascii url over 2048": {link(func(s *Sitelink) {
			s.FinalURL = "https://x.example/" + strings.Repeat("中", 2048-len("https://x.example/")+1)
		}), nil, FieldSitelinks, "is 2049 characters"},
		"url trailing newline": {link(func(s *Sitelink) { s.FinalURL = "https://x.example/register\n" }), nil, FieldSitelinks, "finalUrl contains a control character"},
		"url leading tab":      {link(func(s *Sitelink) { s.FinalURL = "\thttps://x.example/register" }), nil, FieldSitelinks, "finalUrl contains a control character"},
		"url over 2048":        {link(func(s *Sitelink) { s.FinalURL = "https://x.example/" + strings.Repeat("p", 2048) }), nil, FieldSitelinks, "limit is 2048"},
		"repeated text":        {[]Sitelink{{Text: "Register", FinalURL: "https://x.example"}, {Text: "REGISTER", FinalURL: "https://y.example"}}, nil, FieldSitelinks, "[1].text repeats"},
		"one callout":          {nil, []string{"Only one"}, FieldCallouts, "at least 2"},
		"too many callouts":    {nil, tooManyC, FieldCallouts, "at most 20"},
		"empty callout":        {nil, []string{"a", " "}, FieldCallouts, "[1] is empty"},
		"callout over 25":      {nil, []string{"a", strings.Repeat("b", 26)}, FieldCallouts, "limit is 25"},
		"wide callout over 12": {nil, []string{"a", strings.Repeat("字", 13)}, FieldCallouts, "limit is 12"},
		"callout control char": {nil, []string{"a", "b\x00"}, FieldCallouts, "control character"},
		"repeated callout":     {nil, []string{"Hands-On Labs", "hands-on labs"}, FieldCallouts, "[1] repeats"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := ValidateTextExtensions(tc.links, tc.callouts)
			var te *TextExtensionError
			if !errors.As(err, &te) {
				t.Fatalf("want a *TextExtensionError, got %v", err)
			}
			if te.Field != tc.field || !strings.Contains(te.Msg, tc.want) {
				t.Errorf("got field %q msg %q, want field %q containing %q", te.Field, te.Msg, tc.field, tc.want)
			}
		})
	}
}

// textExtAPI fakes the four endpoints, numbering created extensions from 9000 and recording
// which extension types the lookups asked about.
type textExtAPI struct {
	mu       sync.Mutex
	next     int
	existing map[string]string // AdExtensionType -> existing id
	override func(path, body string) (int, string, bool)
}

func (a *textExtAPI) handle(path, body string) (int, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.override != nil {
		if s, b, ok := a.override(path, body); ok {
			return s, b
		}
	}
	switch path {
	case "AdExtensionsAssociations/Query":
		var q struct{ AdExtensionType string }
		_ = json.Unmarshal([]byte(body), &q)
		if id, ok := a.existing[q.AdExtensionType]; ok {
			return 200, fmt.Sprintf(`{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":%s,"Type":%q},"AssociationType":"Campaign","EntityId":321}]}]}`, id, q.AdExtensionType)
		}
		return 200, noImageAssociations
	case "AdExtensions":
		a.next++
		return 200, fmt.Sprintf(`{"AdExtensionIdentities":[{"Id":%d,"Version":1}],"NestedPartialErrors":[]}`, 9000+a.next)
	case "AdExtensionsAssociations/Set":
		return 200, `{"PartialErrors":[]}`
	}
	return 404, `{}`
}

// textExtRecorder is kwRecorder with the request body handed to the fake, which the
// type-filtered lookup needs.
func textExtRecorder(t *testing.T, api *textExtAPI) (*Client, func() []kwCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []kwCall
	)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path := r.URL.Path[strings.Index(r.URL.Path, "/v13/")+len("/v13/"):]
		mu.Lock()
		calls = append(calls, kwCall{r.Method, path, string(b)})
		mu.Unlock()
		status, body := api.handle(path, string(b))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	return c, func() []kwCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]kwCall(nil), calls...)
	}
}

func TestAttachCampaignTextExtensions_Success(t *testing.T) {
	api := &textExtAPI{}
	c, calls := textExtRecorder(t, api)
	r, err := c.AttachCampaignTextExtensions(context.Background(), "321", goodSitelinks(), goodCallouts())
	if err != nil {
		t.Fatalf("AttachCampaignTextExtensions: %v", err)
	}
	if r.Degraded() {
		t.Fatalf("a clean run is not degraded: %+v", r)
	}
	if strings.Join(r.SitelinkAdExtensionIDs, ",") != "9001,9002" || strings.Join(r.CalloutAdExtensionIDs, ",") != "9003,9004" {
		t.Fatalf("ids = %v %v", r.SitelinkAdExtensionIDs, r.CalloutAdExtensionIDs)
	}
	got := calls()
	want := []string{
		"AdExtensionsAssociations/Query", "AdExtensions", "AdExtensionsAssociations/Set", "AdExtensions", "AdExtensionsAssociations/Set",
		"AdExtensionsAssociations/Query", "AdExtensions", "AdExtensionsAssociations/Set", "AdExtensions", "AdExtensionsAssociations/Set",
	}
	if strings.Join(paths(got), ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v\nwant    %v", paths(got), want)
	}
	if !strings.Contains(got[0].body, `"AdExtensionType":"SitelinkAdExtension"`) || !strings.Contains(got[0].body, `"EntityIds":[321]`) {
		t.Errorf("sitelink lookup = %s", got[0].body)
	}
	if !strings.Contains(got[5].body, `"AdExtensionType":"CalloutAdExtension"`) {
		t.Errorf("callout lookup = %s", got[5].body)
	}

	type ext struct {
		Type, DisplayText, Description1, Description2, Text string
		FinalUrls                                           []string
	}
	decode := func(body string) (json.Number, ext) {
		var req struct {
			AccountId    json.Number
			AdExtensions []ext
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil || len(req.AdExtensions) != 1 {
			t.Fatalf("AddAdExtensions body = %s (%v)", body, err)
		}
		return req.AccountId, req.AdExtensions[0]
	}
	acct, s0 := decode(got[1].body)
	if acct.String() != testAccount().AccountID {
		t.Errorf("AccountId = %s", acct)
	}
	if s0.Type != "SitelinkAdExtension" || s0.DisplayText != "Schedule" || s0.Description1 != "Five days of sessions" ||
		s0.Description2 != "Keynotes, talks and labs" || len(s0.FinalUrls) != 1 || s0.FinalUrls[0] != "https://events.example.org/schedule/" || s0.Text != "" {
		t.Errorf("sitelink 0 = %+v", s0)
	}
	_, s1 := decode(got[3].body)
	if s1.DisplayText != "Register" || strings.Contains(got[3].body, "Description1") || strings.Contains(got[3].body, "Description2") {
		t.Errorf("a sitelink without descriptions must omit both: %s", got[3].body)
	}
	_, c0 := decode(got[6].body)
	if c0.Type != "CalloutAdExtension" || c0.Text != "Early Bird Pricing" || strings.Contains(got[6].body, "DisplayText") || strings.Contains(got[6].body, "FinalUrls") {
		t.Errorf("callout 0 = %s", got[6].body)
	}
	_, c1 := decode(got[8].body)
	if c1.Text != "300+ Sessions" {
		t.Errorf("callout 1 must be trimmed: %+v", c1)
	}
	var assoc struct {
		AdExtensionIdToEntityIdAssociations []struct{ AdExtensionId, EntityId json.Number }
		AssociationType                     string
	}
	if err := json.Unmarshal([]byte(got[2].body), &assoc); err != nil {
		t.Fatal(err)
	}
	if assoc.AssociationType != "Campaign" || len(assoc.AdExtensionIdToEntityIdAssociations) != 1 ||
		assoc.AdExtensionIdToEntityIdAssociations[0].AdExtensionId.String() != "9001" ||
		assoc.AdExtensionIdToEntityIdAssociations[0].EntityId.String() != "321" {
		t.Errorf("association body = %s", got[2].body)
	}
}

// A type the campaign already has is skipped entirely (a retry never doubles it); the other
// type is still attached.
func TestAttachCampaignTextExtensions_SkipsTypeAlreadyAssociated(t *testing.T) {
	api := &textExtAPI{existing: map[string]string{"SitelinkAdExtension": "4441"}}
	c, calls := textExtRecorder(t, api)
	r, err := c.AttachCampaignTextExtensions(context.Background(), "321", goodSitelinks(), goodCallouts())
	if err != nil || r.Degraded() {
		t.Fatalf("got %+v, %v", r, err)
	}
	if len(r.SitelinkAdExtensionIDs) != 0 || len(r.CalloutAdExtensionIDs) != 2 {
		t.Errorf("ids = %v %v", r.SitelinkAdExtensionIDs, r.CalloutAdExtensionIDs)
	}
	if !strings.Contains(strings.Join(r.Steps, "\n"), "Sitelink extensions already associated with the campaign (1, e.g. 4441): none added") {
		t.Errorf("steps = %v", r.Steps)
	}
	n := 0
	for _, call := range calls() {
		if call.path == "AdExtensions" && strings.Contains(call.body, "SitelinkAdExtension") {
			n++
		}
	}
	if n != 0 {
		t.Errorf("%d sitelinks created on a campaign that already has them", n)
	}
}

// A retry that finds fewer than MinCallouts callouts on the campaign adds none (it cannot tell
// which of its own failed), but the campaign cannot serve callouts, so the result is degraded.
// Two or more found is a clean retry.
func TestAttachCampaignTextExtensions_RetryWithTooFewCalloutsIsDegraded(t *testing.T) {
	oneCallout := func(p, b string) (int, string, bool) {
		if p == "AdExtensionsAssociations/Query" && strings.Contains(b, "CalloutAdExtension") {
			return 200, `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":5001,"Type":"CalloutAdExtension"},"AssociationType":"Campaign","EntityId":321}]}]}`, true
		}
		return 0, "", false
	}
	twoCallouts := func(p, b string) (int, string, bool) {
		if p == "AdExtensionsAssociations/Query" && strings.Contains(b, "CalloutAdExtension") {
			return 200, `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":5001,"Type":"CalloutAdExtension"},"AssociationType":"Campaign","EntityId":321},{"AdExtension":{"Id":5002,"Type":"CalloutAdExtension"},"AssociationType":"Campaign","EntityId":321}]}]}`, true
		}
		return 0, "", false
	}
	c, calls := textExtRecorder(t, &textExtAPI{override: oneCallout})
	r, err := c.AttachCampaignTextExtensions(context.Background(), "321", nil, goodCallouts())
	if err != nil {
		t.Fatal(err)
	}
	if !r.CalloutsBelowMinimum || !r.Degraded() {
		t.Errorf("one callout found must degrade: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Steps, "\n"), "only 1 associated") {
		t.Errorf("steps = %v", r.Steps)
	}
	if n := len(calls()); n != 1 {
		t.Errorf("only the lookup may run, got %d calls", n)
	}
	c, _ = textExtRecorder(t, &textExtAPI{override: twoCallouts})
	if r, err := c.AttachCampaignTextExtensions(context.Background(), "321", nil, goodCallouts()); err != nil || r.Degraded() {
		t.Errorf("two callouts found is a clean retry, got %+v, %v", r, err)
	}
}

// A first run that attaches only one callout leaves the campaign unable to serve callouts.
func TestAttachCampaignTextExtensions_OneCalloutAttachedIsBelowMinimum(t *testing.T) {
	c, _ := textExtRecorder(t, &textExtAPI{override: func(p, b string) (int, string, bool) {
		if p == "AdExtensions" && strings.Contains(b, `"300+ Sessions"`) {
			return 400, `{"Code":1}`, true
		}
		return 0, "", false
	}})
	r, err := c.AttachCampaignTextExtensions(context.Background(), "321", nil, goodCallouts())
	if err != nil {
		t.Fatal(err)
	}
	if !r.CalloutsBelowMinimum || len(r.CalloutAdExtensionIDs) != 1 {
		t.Errorf("got %+v", r)
	}
}

// Each extension's failure is classified and counted, and the run carries on with the rest.
func TestAttachCampaignTextExtensions_FailuresAreClassifiedAndCounted(t *testing.T) {
	cases := []struct {
		name                   string
		override               func(path, body string) (int, string, bool)
		wantFailed, wantUnconf int
		wantStep               string
		wantSitelinkIDs        int
	}{
		{"lookup fails: nothing of that type is added", func(p, b string) (int, string, bool) {
			if p == "AdExtensionsAssociations/Query" && strings.Contains(b, "Sitelink") {
				return 503, `{"Message":"leak"}`, true
			}
			return 0, "", false
		}, 2, 0, "Sitelink extensions NOT attached: could not check", 0},
		{"create rejected definitely", func(p, b string) (int, string, bool) {
			if p == "AdExtensions" && strings.Contains(b, `"Schedule"`) {
				return 200, `{"AdExtensionIdentities":[null],"NestedPartialErrors":[{"Index":0,"Code":3001,"ErrorCode":"InvalidSitelink"}]}`, true
			}
			return 0, "", false
		}, 1, 0, "Sitelink extension 0 NOT attached", 1},
		{"create ambiguous", func(p, b string) (int, string, bool) {
			if p == "AdExtensions" && strings.Contains(b, `"Register"`) {
				return 502, `{}`, true
			}
			return 0, "", false
		}, 0, 1, "Sitelink extension 1 UNCONFIRMED", 1},
		{"association ambiguous", func(p, b string) (int, string, bool) {
			if p == "AdExtensionsAssociations/Set" {
				return 200, `{}`, true
			}
			return 0, "", false
		}, 0, 4, "UNCONFIRMED", 0},
		{"association rejected", func(p, b string) (int, string, bool) {
			if p == "AdExtensionsAssociations/Set" {
				return 200, `{"PartialErrors":[{"Index":0,"Code":1,"ErrorCode":"X"}]}`, true
			}
			return 0, "", false
		}, 4, 0, "NOT attached", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &textExtAPI{override: tc.override}
			c, _ := textExtRecorder(t, api)
			r, err := c.AttachCampaignTextExtensions(context.Background(), "321", goodSitelinks(), goodCallouts())
			if err != nil {
				t.Fatalf("a per-extension failure is reported on the result, not as an error: %v", err)
			}
			if r.Failed != tc.wantFailed || r.Unconfirmed != tc.wantUnconf || !r.Degraded() {
				t.Errorf("failed %d unconfirmed %d, want %d %d", r.Failed, r.Unconfirmed, tc.wantFailed, tc.wantUnconf)
			}
			if len(r.SitelinkAdExtensionIDs) != tc.wantSitelinkIDs {
				t.Errorf("sitelink ids = %v, want %d", r.SitelinkAdExtensionIDs, tc.wantSitelinkIDs)
			}
			joined := strings.Join(r.Steps, "\n")
			if !strings.Contains(joined, tc.wantStep) {
				t.Errorf("steps must contain %q:\n%s", tc.wantStep, joined)
			}
			if strings.Contains(joined, "leak") {
				t.Error("upstream message text leaked into a step")
			}
		})
	}
}

func TestAttachCampaignTextExtensions_RefusesBeforeAnyRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		campaign string
		links    []Sitelink
		callouts []string
	}{
		"bad campaign id": {"abc", goodSitelinks(), nil},
		"bad sitelink":    {"321", []Sitelink{{Text: "x"}}, nil},
		"one callout":     {"321", nil, []string{"x"}},
	} {
		t.Run(name, func(t *testing.T) {
			c, calls := textExtRecorder(t, &textExtAPI{})
			r, err := c.AttachCampaignTextExtensions(context.Background(), tc.campaign, tc.links, tc.callouts)
			if err == nil || r == nil {
				t.Fatalf("want a refusal with a non-nil result, got %v, %v", r, err)
			}
			if n := len(calls()); n != 0 {
				t.Errorf("%d requests sent before the refusal", n)
			}
		})
	}
}

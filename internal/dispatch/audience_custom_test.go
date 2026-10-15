// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// fakePortal is a HubSpot stand-in for the custom-audience paths: named lists,
// contact properties, and a record of every create.
type fakePortal struct {
	mu         sync.Mutex
	lists      map[string]string // name -> id
	sizes      map[string]int64  // id -> size
	properties map[string]string // name -> type
	creates    []map[string]any
	createBody string // raw reply to a create; default is a confirmed list
	// eventProps are the education event definition's properties; nil means the
	// portal has no such definition.
	eventProps []string
	// regProps are the registration event's properties, name -> type.
	regProps map[string]string
}

func (p *fakePortal) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/crm/v3/lists/search":
			var body struct {
				Query string `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var hits []string
			for name, id := range p.lists {
				if strings.Contains(strings.ToLower(name), strings.ToLower(strings.Fields(body.Query + " x")[0])) {
					hits = append(hits, `{"listId":"`+id+`","name":"`+name+`","objectTypeId":"0-1"}`)
				}
			}
			_, _ = io.WriteString(w, `{"lists":[`+strings.Join(hits, ",")+`],"hasMore":false}`)
		case strings.HasPrefix(r.URL.Path, "/crm/v3/lists/") && r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/crm/v3/lists/")
			for name, lid := range p.lists {
				if lid == id {
					size, _ := json.Marshal(p.sizes[id])
					_, _ = io.WriteString(w, `{"list":{"listId":"`+id+`","name":"`+name+`","objectTypeId":"0-1","size":`+string(size)+`}}`)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/crm/v3/lists" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			p.creates = append(p.creates, body)
			if p.createBody != "" {
				_, _ = io.WriteString(w, p.createBody)
				return
			}
			_, _ = io.WriteString(w, `{"list":{"listId":"7777","name":"`+body["name"].(string)+`","objectTypeId":"0-1"}}`)
		case strings.HasPrefix(r.URL.Path, "/crm/v3/properties/contacts/"):
			name := strings.TrimPrefix(r.URL.Path, "/crm/v3/properties/contacts/")
			typ, ok := p.properties[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"name":"`+name+`","type":"`+typ+`"}`)
		case r.URL.Path == "/events/v3/event-definitions":
			props := make([]string, 0, len(p.eventProps))
			for _, name := range p.eventProps {
				props = append(props, `{"name":"`+name+`","label":"","type":"string"}`)
			}
			reg := make([]string, 0, len(p.regProps))
			for name, typ := range p.regProps {
				reg = append(reg, `{"name":"`+name+`","label":"","type":"`+typ+`"}`)
			}
			_, _ = io.WriteString(w, `{"results":[{"objectTypeId":"6-48984571","properties":[`+strings.Join(reg, ",")+`]},{"objectTypeId":"6-58204655","properties":[`+strings.Join(props, ",")+`]}]}`)
		case r.URL.Path == hubSpotTokenInfoPath:
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
}

func explorerOver(srv *httptest.Server, llm completer) *AudienceExplorer {
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))
	return NewAudienceExplorer(builder, nil, nil, llm)
}

func seoulPortal() *fakePortal {
	return &fakePortal{
		lists: map[string]string{"26Q2 - The Linux Foundation - Master-list": "26716"},
		sizes: map[string]int64{"26716": 2500000},
		properties: map[string]string{
			"city": "string", "ip_city": "string", "country_dropdown": "enumeration", "ip_country": "string",
			"industry": "enumeration", "jobtitle": "string",
		},
	}
}

func seoulCriteria() audience.CustomCriteria {
	return audience.CustomCriteria{
		Cities: []string{"Seoul"}, ContactLocation: true, EventRegistered: true, EducationEnrolled: true,
		GateListID: "26716", Label: "OSS Korea",
	}
}

func TestInterpretCustom_WithoutAModelSaysSoBeforeTouchingThePortal(t *testing.T) {
	x := explorerWithNoPortal(nil, nil, nil)
	_, err := x.InterpretCustom(context.Background(), "proj-1", "Seoul contacts", "", nil)
	assert.ErrorIs(t, err, audience.ErrCustomInterpretUnavailable)

	_, err = x.InterpretCustom(context.Background(), "proj-1", "  ", "", nil)
	assert.ErrorIs(t, err, audience.ErrInvalidRequest, "an empty request is the caller's to fix, model or not")
}

func TestInterpretCustom_ResolvesTheGateTheModelCannotKnow(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &stubLLM{reply: `{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":true,"education_enrolled":true,"gate_event_history":false,"brand_short":"LF","property_filters":[],"speakers":null,"ambassadors":false,"label":"OSS Korea"},"questions":[{"id":"mailability","question":"Gate event history?","why":"","options":["Match reference","Stricter"]}],"notes":[]}`}

	out, err := explorerOver(srv, llm).InterpretCustom(context.Background(), "proj-1", "Mailable contacts in Seoul for OSS Korea", "", []audience.CustomAnswer{{QuestionID: "q", Answer: "city only"}})
	require.NoError(t, err)
	assert.Equal(t, audience.CustomInterpretSystemPrompt, llm.system)
	assert.Contains(t, llm.user, "BEGIN REQUEST\nMailable contacts in Seoul for OSS Korea\nEND REQUEST")
	assert.Contains(t, llm.user, "city only")

	require.NotNil(t, out.Gate)
	assert.Equal(t, "26716", out.Gate.ListID)
	assert.Equal(t, "26716", out.Draft.Criteria.GateListID, "the resolved gate is filled into the draft as a suggestion")
	require.Len(t, out.Draft.Questions, 1)
	assert.Empty(t, portal.creates, "interpretation never writes")
}

func TestPreviewCustom_NamesEveryUnknownPropertyAndWritesNothing(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	c := seoulCriteria()
	c.PropertyFilters = []audience.CustomPropertyFilter{
		{Property: "job_level", Operator: audience.CustomOpKnown},
		{Property: "seniority", Operator: audience.CustomOpKnown},
	}

	_, err := explorerOver(srv, nil).PreviewCustom(context.Background(), "proj-1", c, "")
	var unknown *audience.UnknownPropertiesError
	require.ErrorAs(t, err, &unknown)
	assert.Equal(t, []string{"job_level", "seniority"}, unknown.Names)
	assert.ErrorIs(t, err, audience.ErrInvalidRequest)
	assert.Empty(t, portal.creates)
}

func TestPreviewCustom_ResolvesTypesBoundsAndVersionsTheName(t *testing.T) {
	portal := seoulPortal()
	portal.lists["26Q4 - Seoul - Mailable Contacts - OSS Korea"] = "500"
	srv := portal.serve(t)
	defer srv.Close()
	x := explorerOver(srv, nil)
	c := seoulCriteria()
	c.GateEventHistory = true
	c.PropertyFilters = []audience.CustomPropertyFilter{{Property: "industry", Operator: audience.CustomOpAnyOf, Values: []string{"COMPUTER_SOFTWARE"}}}
	x.now = func() time.Time { return time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC) }

	p, err := x.PreviewCustom(context.Background(), "proj-1", c, "")
	require.NoError(t, err)
	assert.Equal(t, "26Q4 - Seoul - Mailable Contacts - OSS Korea (v2)", p.Name, "a taken name is versioned, never reused")
	assert.True(t, p.Criteria.PropertyFilters[0].Enumeration, "the operation type comes from the portal's definition")
	assert.Equal(t, 4, p.BranchCount)
	require.NotNil(t, p.UpperBound)
	assert.Equal(t, 2500000, *p.UpperBound, "every branch is gated, so the gate bounds the audience")
	assert.Empty(t, portal.creates)
}

func TestPreviewCustom_RefusesAGateThatIsNotThere(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	c := seoulCriteria()
	c.GateListID = "404404"
	_, err := explorerOver(srv, nil).PreviewCustom(context.Background(), "proj-1", c, "")
	assert.ErrorIs(t, err, audience.ErrListNotFound)
}

func TestCreateCustom_RebuildsFromCriteriaAndCreatesOnce(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	c := seoulCriteria()
	c.PropertyFilters = []audience.CustomPropertyFilter{{Property: "industry", Operator: audience.CustomOpAnyOf, Values: []string{"X"}, Enumeration: false}}

	list, err := explorerOver(srv, nil).CreateCustom(context.Background(), "proj-1", c, "Seoul test")
	require.NoError(t, err)
	assert.Equal(t, "7777", list.ListID)
	require.Len(t, portal.creates, 1)
	assert.Equal(t, "Seoul test", portal.creates[0]["name"])
	raw, _ := json.Marshal(portal.creates[0]["filterBranch"])
	assert.Contains(t, string(raw), `"operationType":"ENUMERATION","operator":"IS_ANY_OF","values":["X"]},"property":"industry"`,
		"a client-supplied Enumeration=false is overridden by the portal's type")
}

func TestCreateCustom_UnconfirmedIsAPartialWithTheName(t *testing.T) {
	portal := seoulPortal()
	portal.createBody = `{}`
	srv := portal.serve(t)
	defer srv.Close()

	_, err := explorerOver(srv, nil).CreateCustom(context.Background(), "proj-1", seoulCriteria(), "Seoul test")
	var partial *audience.ComposePartialError
	require.ErrorAs(t, err, &partial)
	assert.Equal(t, "Seoul test", partial.MasterName, "the name is the only reconcile key an unconfirmed create has")
}

func TestCreateCustom_InvalidCriteriaNeverReachThePortal(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	_, err := explorerOver(srv, nil).CreateCustom(context.Background(), "proj-1", audience.CustomCriteria{Cities: []string{"Seoul"}, ContactLocation: true}, "")
	assert.ErrorIs(t, err, audience.ErrInvalidRequest)
	assert.Empty(t, portal.creates)
}

// seqLLM answers each call with the next reply, recording every prompt.
type seqLLM struct {
	replies []string
	systems []string
}

func (s *seqLLM) Complete(_ context.Context, system, _ string) (string, error) {
	s.systems = append(s.systems, system)
	if len(s.replies) == 0 {
		return "", errors.New("no reply queued")
	}
	reply := s.replies[0]
	s.replies = s.replies[1:]
	return reply, nil
}

func TestInterpretCustom_RanksExistingListsButNeverInventsOne(t *testing.T) {
	portal := seoulPortal()
	portal.lists["25Q3 - Seoul - Mailable Contacts - OSS Korea"] = "411"
	portal.lists["26Q3 - Seoul - Mailable Contacts - Master"] = "412"
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":true,"education_enrolled":false,"gate_event_history":false,"brand_short":"LF","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":[],"registered_for":[],"label":"OSS Korea"},"questions":[],"notes":[]}`,
		`{"lists":[{"list_id":"411","recommendation":"update","reason":"last year's quarter"},{"list_id":"999","recommendation":"reuse","reason":"invented"}]}`,
	}}

	out, err := explorerOver(srv, llm).InterpretCustom(context.Background(), "proj-1", "Mailable contacts in Seoul", "", nil)
	require.NoError(t, err)

	require.Len(t, out.Similar, 1, "the master list is never offered, and an invented id is never added")
	assert.Equal(t, "411", out.Similar[0].ListID)
	assert.Equal(t, audience.CustomReuseUpdate, out.Similar[0].Recommendation)
	assert.Equal(t, []string{audience.CustomInterpretSystemPrompt, audience.CustomReuseSystemPrompt}, llm.systems)
	assert.Empty(t, portal.creates)
}

func TestInterpretCustom_AFailedRankingStillShowsTheCandidates(t *testing.T) {
	portal := seoulPortal()
	portal.lists["25Q3 - Seoul - Mailable Contacts - OSS Korea"] = "411"
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":false,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":[],"registered_for":[],"label":""},"questions":[],"notes":[]}`,
	}}

	out, err := explorerOver(srv, llm).InterpretCustom(context.Background(), "proj-1", "Seoul", "", nil)
	require.NoError(t, err, "a failed ranking costs a suggestion, never the draft")
	require.Len(t, out.Similar, 1)
	assert.Empty(t, out.Similar[0].Recommendation)
}

func TestPreviewCustom_ResolvesTheCourseTopicPropertyFromThePortal(t *testing.T) {
	portal := seoulPortal()
	portal.eventProps = []string{"user_city", "course_name"}
	srv := portal.serve(t)
	defer srv.Close()
	c := audience.CustomCriteria{EducationEnrolled: true, EducationTopics: []string{"Kubernetes"}, EducationTopicProperty: "made_up_by_client", Label: "KubeCon"}

	p, err := explorerOver(srv, nil).PreviewCustom(context.Background(), "proj-1", c, "")
	require.NoError(t, err)
	assert.Equal(t, "course_name", p.Criteria.EducationTopicProperty, "a client-supplied property is discarded for the portal's")
}

func TestPreviewCustom_RefusesTopicsWhenThePortalHasNoTopicProperty(t *testing.T) {
	portal := seoulPortal()
	portal.eventProps = []string{"user_city"}
	srv := portal.serve(t)
	defer srv.Close()
	c := audience.CustomCriteria{EducationEnrolled: true, EducationTopics: []string{"Kubernetes"}}

	_, err := explorerOver(srv, nil).PreviewCustom(context.Background(), "proj-1", c, "")
	assert.ErrorIs(t, err, audience.ErrInvalidRequest)
}

func TestPreviewCustom_HonoursAPortalCandidateTheOperatorPicked(t *testing.T) {
	portal := seoulPortal()
	portal.eventProps = []string{"course_code", "course_name"}
	srv := portal.serve(t)
	defer srv.Close()
	x := explorerOver(srv, nil)
	c := audience.CustomCriteria{EducationEnrolled: true, EducationTopics: []string{"Kubernetes"}, Label: "KubeCon"}

	p, err := x.PreviewCustom(context.Background(), "proj-1", c, "")
	require.NoError(t, err)
	assert.Equal(t, "course_name", p.Criteria.EducationTopicProperty)
	assert.Equal(t, []string{"course_name", "course_code"}, p.EducationTopicCandidates)

	c.EducationTopicProperty = "course_code"
	p, err = x.PreviewCustom(context.Background(), "proj-1", c, "")
	require.NoError(t, err)
	assert.Equal(t, "course_code", p.Criteria.EducationTopicProperty, "a portal candidate the operator chose is honoured")
}

func TestInterpretCustom_AnEventURLQuotesThePageToTheModel(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":["Seoul"],"countries":["South Korea"],"contact_location":true,"event_registered":true,"education_enrolled":true,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":["Synthetics"],"page_view_urls":["events.example.org/synthetic-summit"],"registered_for":[],"label":""},"questions":[],"notes":[]}`,
	}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit", Location: "Seoul, South Korea", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Equal(t, "Synthetic Summit", out.Draft.Criteria.Label, "the page names the event when the model does not")
	assert.Equal(t, "SYN", out.Draft.BrandShort)
	assert.Equal(t, []string{"South Korea"}, out.Draft.Criteria.Countries)
}

// A brand the model named is kept, with its gate: the lists-filed-under fallback
// runs only when neither the page nor the model named one.
func TestInterpretCustom_TheModelsBrandBeatsTheListsFallback(t *testing.T) {
	portal := seoulPortal()
	portal.lists["26Q2 - SYN - Master-list"] = "801"
	portal.lists["26Q2 - OSS - Master-list"] = "802"
	portal.lists["26Q3 - OSS - Synthetic Summit 2026 Registrants"] = "803"
	portal.lists["26Q3 - OSS - Synthetic Summit 2026 Web Visitors"] = "804"
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"","eventDates":[]}`,
		`{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":false,"education_enrolled":false,"gate_event_history":false,"brand_short":"SYN","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":[],"registered_for":[],"label":""},"questions":[],"notes":[]}`,
	}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Equal(t, "SYN", out.Draft.BrandShort, "the model's brand is not replaced by the lists' segment")
	assert.Equal(t, "801", out.Draft.Criteria.GateListID, "the model's brand keeps its own master gate")
}

func TestCreateCustom_TypesSpeakerSignalsFromThePortal(t *testing.T) {
	portal := seoulPortal()
	portal.regProps = map[string]string{"registration_type": "string", "is_event_speaker_bool": "bool"}
	srv := portal.serve(t)
	defer srv.Close()
	c := audience.CustomCriteria{Label: "KubeCon", Speakers: &audience.CustomSpeakers{Scope: audience.SpeakerScopeCurrent, EventNames: []string{"KubeCon Europe 2026"},
		PropertyTypes: map[string]string{"is_event_speaker_bool": "string"}}}

	_, err := explorerOver(srv, nil).CreateCustom(context.Background(), "proj-1", c, "")
	require.NoError(t, err)
	require.Len(t, portal.creates, 1)
	raw, _ := json.Marshal(portal.creates[0]["filterBranch"])
	assert.Contains(t, string(raw), `"operationType":"BOOL"`, "the portal's type wins over anything the caller sent")
	assert.NotContains(t, string(raw), "is_event_speaker\"", "a property the portal lacks is not filtered on")
}

// TestRunQA_CountsSendTimeSuppression is the custom-list case: the list carries no
// exclusion of its own and the send applies the suppression lists.
func TestRunQA_CountsSendTimeSuppression(t *testing.T) {
	portal := seoulPortal()
	portal.lists["26Q4 - Seoul - Mailable Contacts"] = "500"
	portal.lists["LF Europe GDPR Suppression"] = "601"
	portal.lists["LF Global Opt-Outs"] = "602"
	srv := portal.serve(t)
	defer srv.Close()
	x := explorerOver(srv, nil)

	without, err := x.RunQA(context.Background(), "proj-1", "500", "", true, false, nil)
	require.NoError(t, err)
	assert.Equal(t, audience.VerdictFail, without.Checks.ExclusionCompleteness.Verdict, "judged on its own filters, a custom list has no exclusion")

	with, err := x.RunQA(context.Background(), "proj-1", "500", "", true, false, []string{"601", "602", "404404"})
	require.NoError(t, err)
	assert.Equal(t, audience.VerdictPass, with.Checks.ExclusionCompleteness.Verdict)
	assert.Equal(t, 2, with.Checks.ExclusionCompleteness.SendTimeCount, "an unreadable id counts for nothing")
	assert.True(t, with.Checks.Suppression.AppliedGDPR, "the GDPR list applied at send time satisfies the EU check")
	assert.True(t, with.Checks.Suppression.AppliedOptOut)
	assert.Empty(t, portal.creates, "QA never writes")

	self, err := x.RunQA(context.Background(), "proj-1", "500", "", true, false, []string{"500"})
	require.NoError(t, err)
	assert.Equal(t, audience.VerdictFail, self.Checks.ExclusionCompleteness.Verdict, "the audited list sent as its own suppression is no exclusion")
	assert.Zero(t, self.Checks.ExclusionCompleteness.SendTimeCount)
}

// TestDiscover_HidesWhatMustNotBeIncludedAndInfersTheBrand replays the live AgntCon
// Japan discovery: opt-out, master and test lists were offered for INCLUSION, and the
// page named no brand.
func TestDiscover_HidesWhatMustNotBeIncludedAndInfersTheBrand(t *testing.T) {
	portal := seoulPortal()
	portal.lists = map[string]string{
		"26Q3 - SYN - Synthetic Summit 2026 Registrants":          "701",
		"Synthetic Summit Global Opt-Outs":                        "702",
		"26Q3 - SYN - Synthetic Summit - Master":                  "703",
		"26Q3 - SYN - Synthetic Summit Web Visitors [psh-test]":   "704",
		"26Q3 - SYN - Synthetic Summit 2026 - Education Enrolled": "705",
	}
	srv := portal.serve(t)
	defer srv.Close()
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit", StartDate: "2026-03-02"}}, nil)

	out, err := x.Discover(context.Background(), "proj-1", "https://events.example.org/synthetic-summit")
	require.NoError(t, err)

	offered := map[string]bool{}
	for _, l := range out.Lists {
		offered[l.ListID] = true
	}
	assert.False(t, offered["702"], "an opt-out list is never offered for inclusion")
	assert.False(t, offered["703"], "a master belongs to the Reuse step")
	assert.False(t, offered["704"], "a test list is not an audience")
	assert.True(t, offered["701"])
	assert.Equal(t, 3, out.Hidden)
	assert.Equal(t, "SYN", out.Event.BrandShort, "the brand the event's own lists are filed under")
}

// TestRunQA_FlagsTheEventsOwnRegistrantsAuditedDirectly is the live miss: the
// registrants list itself, picked as the send list, passed check 4.
func TestRunQA_FlagsTheEventsOwnRegistrantsAuditedDirectly(t *testing.T) {
	portal := seoulPortal()
	portal.lists["26Q3 - AGNTCon + MCPCon Japan 2026 Registrants"] = "29895"
	srv := portal.serve(t)
	defer srv.Close()

	out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "29895", "AGNTCon + MCPCon Japan 2026", false, false, nil)
	require.NoError(t, err)
	assert.NotEqual(t, audience.VerdictPass, out.Checks.CurrentRegistrants.Verdict,
		"sending a registration push to this edition's own registrants must not pass")
}

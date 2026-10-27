// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/snowflake"
)

// fakePortal is a HubSpot stand-in for the custom-audience paths: named lists,
// contact properties, and a record of every create.
type fakePortal struct {
	mu         sync.Mutex
	lists      map[string]string // name -> id
	sizes      map[string]int64  // id -> size
	objTypes   map[string]string // id -> objectTypeId; absent is a contact list ("0-1")
	branches   map[string]string // id -> raw filterBranch JSON; absent is none
	properties map[string]string // name -> type
	// propOptions are an enumeration property's option values, by property name.
	propOptions map[string][]string
	creates     []map[string]any
	createBody  string // raw reply to a create; default is a confirmed list
	// eventProps are the education event definition's properties; nil means the
	// portal has no such definition.
	eventProps []string
	// eventPropTypes overrides an education event property's type; the default is string.
	eventPropTypes map[string]string
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
					objType := "0-1"
					if t, ok := p.objTypes[id]; ok {
						objType = t
					}
					branch := ""
					if b, ok := p.branches[id]; ok {
						branch = `,"filterBranch":` + b
					}
					_, _ = io.WriteString(w, `{"list":{"listId":"`+id+`","name":"`+name+`","objectTypeId":"`+objType+`","size":`+string(size)+branch+`}}`)
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
			opts := make([]string, 0, len(p.propOptions[name]))
			for _, v := range p.propOptions[name] {
				opts = append(opts, `{"label":"`+v+`","value":"`+v+`"}`)
			}
			_, _ = io.WriteString(w, `{"name":"`+name+`","type":"`+typ+`","options":[`+strings.Join(opts, ",")+`]}`)
		case r.URL.Path == "/events/v3/event-definitions":
			props := make([]string, 0, len(p.eventProps))
			for _, name := range p.eventProps {
				typ := "string"
				if t, ok := p.eventPropTypes[name]; ok {
					typ = t
				}
				props = append(props, `{"name":"`+name+`","label":"","type":"`+typ+`"}`)
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

// grpcDraft is the model naming a hosted project, not its foundation, as the brand.
const grpcDraft = `{"criteria":{"cities":["Bengaluru"],"countries":[],"contact_location":true,"event_registered":false,"education_enrolled":false,"gate_event_history":false,"brand_short":"gRPC","property_filters":[],"speakers":null,"ambassadors":false,"label":"gRPC India 2026"},"questions":[],"notes":[]}`

func TestInterpretCustom_InfersTheFoundationWhenTheProjectHasNoMasterList(t *testing.T) {
	portal := &fakePortal{lists: map[string]string{
		"26Q2 - CNCF - Foundation - Master-list":      "26724",
		"25Q2 - CNCF - Events - gRPConf - AOI":        "20643",
		"25Q2 - CNCF - Events - gRPConf - Web Visits": "17133",
		"gRPConf India — Master (11df46d4)":           "31136",
	}}
	srv := portal.serve(t)
	defer srv.Close()

	out, err := explorerOver(srv, &stubLLM{reply: grpcDraft}).InterpretCustom(context.Background(), "proj-1", "gRPC India 2026: contacts in Bengaluru", "", nil)
	require.NoError(t, err)

	require.NotNil(t, out.Gate, "a project with no master list is gated by its foundation's")
	assert.Equal(t, "26724", out.Gate.ListID)
	assert.Equal(t, "26724", out.Draft.Criteria.GateListID)
	assert.Equal(t, "CNCF", out.Draft.BrandShort)
	notes := strings.Join(out.Draft.Notes, "\n")
	assert.Contains(t, notes, "Suggested CNCF's master list (26Q2 - CNCF - Foundation - Master-list)", "an inferred gate is said, not silently applied")
	assert.NotContains(t, notes, "No brand master list was found")
	assert.Empty(t, portal.creates, "interpretation never writes")
}

func TestInterpretCustom_NeverGuessesBetweenTwoBrands(t *testing.T) {
	portal := &fakePortal{lists: map[string]string{
		"26Q2 - CNCF - Foundation - Master-list":           "26724",
		"26Q2 - The Linux Foundation - Master-list":        "26716",
		"25Q2 - CNCF - Events - gRPConf - AOI":             "20643",
		"25Q2 - CNCF - Events - gRPConf - Web Visits":      "17133",
		"25Q2 - The Linux Foundation - gRPConf - Speakers": "1",
		"25Q2 - The Linux Foundation - gRPConf - Sponsors": "2",
	}}
	srv := portal.serve(t)
	defer srv.Close()

	out, err := explorerOver(srv, &stubLLM{reply: grpcDraft}).InterpretCustom(context.Background(), "proj-1", "gRPC India 2026: contacts in Bengaluru", "", nil)
	require.NoError(t, err)

	assert.Nil(t, out.Gate, "a tie between brands is no brand: a guessed gate is the wrong audience")
	assert.Empty(t, out.Draft.Criteria.GateListID)
	assert.Equal(t, "gRPC", out.Draft.BrandShort)
	assert.Contains(t, strings.Join(out.Draft.Notes, "\n"), "No brand master list was found")
}

// A name HubSpot's list search cannot take (over 200 characters) is cut to fit, so the
// taken-name check is a search HubSpot answers rather than a 500.
func TestPreviewCustom_CutsANameHubSpotCannotSearch(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	x := explorerOver(srv, nil)

	cut, err := x.PreviewCustom(context.Background(), "proj-1", seoulCriteria(), strings.Repeat("n", audience.MaxListNameLength+1))
	require.NoError(t, err)
	assert.Len(t, cut.Name, audience.MaxListNameLength)

	preview, err := x.PreviewCustom(context.Background(), "proj-1", seoulCriteria(), strings.Repeat("n", audience.MaxListNameLength))
	require.NoError(t, err)
	assert.Len(t, preview.Name, audience.MaxListNameLength)
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
	prompts []string
}

func (s *seqLLM) Complete(_ context.Context, system, prompt string) (string, error) {
	s.systems = append(s.systems, system)
	s.prompts = append(s.prompts, prompt)
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
	assert.Equal(t, "Synthetic Summit 2026", out.Draft.Criteria.Label, "the page names the event, with its year, when the model does not")
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

// An existing speakers list replaces the scope and edition filters once filled in, so a
// past-speakers list is never auto-picked for a current-only request, and a fitting one is.
func TestInterpretCustom_PicksOnlyASpeakersListThatFitsTheRestriction(t *testing.T) {
	for name, tc := range map[string]struct {
		list string
		want string
	}{
		"a past list for a current request": {list: "26Q2 - SYN - Synthetic Summit 2026 Speakers - Past", want: ""},
		"a test list":                       {list: "26Q2 - SYN - Synthetic Summit 2026 Speakers [psh-test]", want: ""},
		"the edition's own list":            {list: "26Q2 - SYN - Synthetic Summit 2026 Speakers", want: "901"},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists[tc.list] = "901"
			srv := portal.serve(t)
			defer srv.Close()
			llm := &seqLLM{replies: []string{
				`{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":false,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":{"scope":"current","event_names":["Synthetic Summit 2026"]},"ambassadors":false,"education_topics":[],"page_view_urls":[],"registered_for":[],"label":"Synthetic Summit"},"questions":[],"notes":[]}`,
			}}

			out, err := explorerOver(srv, llm).InterpretCustom(context.Background(), "proj-1", "Synthetic Summit 2026 speakers", "", nil)
			require.NoError(t, err)
			require.NotNil(t, out.Draft.Criteria.Speakers)
			assert.Equal(t, tc.want, out.Draft.Criteria.Speakers.ListID)
		})
	}
}

// A company or deal list cannot exclude a contact, so it never counts as a send-time suppression.
func TestRunQA_ANonContactSendTimeListIsNoSuppression(t *testing.T) {
	portal := seoulPortal()
	portal.lists["26Q2 - Synthetic Summit Custom"] = "500"
	portal.lists["LF Global Opt-Outs (companies)"] = "650"
	portal.objTypes = map[string]string{"650": "0-2"}
	srv := portal.serve(t)
	defer srv.Close()

	out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "", false, false, []string{"650"})
	require.NoError(t, err)
	assert.Zero(t, out.Checks.ExclusionCompleteness.SendTimeCount, "a company list excludes no contact")
	assert.False(t, out.Checks.Suppression.AppliedOptOut, "its name must not satisfy the opt-out check")
}

// A value comparison the builder cannot express for the property's type fails preview,
// before create could send a filter HubSpot rejects or that matches nobody.
func TestPreviewCustom_RefusesAComparisonThePropertyTypeCannotTake(t *testing.T) {
	cases := map[string]audience.CustomPropertyFilter{
		"a value on a boolean":   {Property: "hs_is_speaker", Operator: audience.CustomOpEquals, Values: []string{"true"}},
		"a value on a number":    {Property: "num_events", Operator: audience.CustomOpAnyOf, Values: []string{"3"}},
		"contains on a picklist": {Property: "industry", Operator: audience.CustomOpContains, Values: []string{"Soft"}},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.properties["hs_is_speaker"] = "bool"
			portal.properties["num_events"] = "number"
			srv := portal.serve(t)
			defer srv.Close()
			c := seoulCriteria()
			c.PropertyFilters = []audience.CustomPropertyFilter{f}

			_, err := explorerOver(srv, nil).PreviewCustom(context.Background(), "proj-1", c, "")
			require.ErrorIs(t, err, audience.ErrInvalidRequest)
			assert.Contains(t, err.Error(), f.Property)
		})
	}

	portal := seoulPortal()
	portal.properties["hs_is_speaker"] = "bool"
	srv := portal.serve(t)
	defer srv.Close()
	c := seoulCriteria()
	c.PropertyFilters = []audience.CustomPropertyFilter{{Property: "hs_is_speaker", Operator: audience.CustomOpKnown}}
	_, err := explorerOver(srv, nil).PreviewCustom(context.Background(), "proj-1", c, "")
	require.NoError(t, err, "is known works on any property type")
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

// An event family with more past editions than registered_for's contract cap must
// still produce a draft preview accepts unedited: the most recent are kept, every
// array fits the design's bounds, and a note says how many were left out.
func TestInterpretCustom_CapsPastEditionsToTheContract(t *testing.T) {
	portal := seoulPortal()
	portal.lists["26Q2 - SYN - Master-list"] = "801"
	srv := portal.serve(t)
	defer srv.Close()
	urls := make([]string, 0, 15)
	for i := range 15 {
		urls = append(urls, fmt.Sprintf(`"events.example.org/page-%d"`, i))
	}
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":["Seoul"],"countries":["Korea"],"contact_location":true,"event_registered":true,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":[` + strings.Join(urls, ",") + `],"registered_for":[],"label":""},"questions":[],"notes":[]}`,
	}}
	events := make([]snowflake.Event, 0, 40)
	for year := 1990; year < 2026; year++ {
		events = append(events, snowflake.Event{EventName: fmt.Sprintf("Synthetic Summit %d", year)})
	}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: events}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit 2026", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	c := out.Draft.Criteria
	require.Len(t, c.RegisteredFor, audience.CustomMaxRegisteredFor, "registered_for is MaxLength(25) in the contract")
	assert.Equal(t, "Synthetic Summit 2025", c.RegisteredFor[0], "the most recent editions are kept")
	assert.Equal(t, "Synthetic Summit 2001", c.RegisteredFor[len(c.RegisteredFor)-1])
	assert.Len(t, c.PageViewURLs, audience.CustomMaxPageViewURLs, "page_view_urls is MaxLength(10) in the contract")
	assert.Equal(t, []string{"South Korea"}, c.Countries, "a country alias is canonicalized on entry")
	assert.Contains(t, strings.Join(out.Draft.Notes, "\n"), "11 older ones were left out")
	require.NoError(t, audience.ValidateCustomCriteria(c), "the unedited draft is one preview accepts")
}

// A country alias builds on the stored form, and a country an enumeration
// country_dropdown has no option for is reported rather than silently matching nobody.
func TestPreviewCustom_CanonicalizesCountriesAndWarnsOnUnknownOptions(t *testing.T) {
	portal := seoulPortal()
	portal.propOptions = map[string][]string{"country_dropdown": {"United States", "South Korea"}}
	srv := portal.serve(t)
	defer srv.Close()
	c := seoulCriteria()
	c.Countries = []string{"USA", "Narnia"}

	out, err := explorerOver(srv, nil).PreviewCustom(context.Background(), "proj-1", c, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"United States", "Narnia"}, out.Criteria.Countries, "preview shows the value actually filtered on")
	joined := strings.Join(out.Warnings, "\n")
	assert.Contains(t, joined, `"Narnia" is not one of country_dropdown's options`)
	assert.NotContains(t, joined, `"United States" is not one`, "a canonical option is not reported")
}

// The topic filter is a string CONTAINS, so a number or enumeration event property
// is never a candidate: not picked automatically, and not honoured when sent.
func TestPreviewCustom_OnlyAStringPropertyCarriesTheTopic(t *testing.T) {
	portal := seoulPortal()
	portal.eventProps = []string{"course_number", "course_topic", "course_name"}
	portal.eventPropTypes = map[string]string{"course_number": "number", "course_topic": "enumeration"}
	srv := portal.serve(t)
	defer srv.Close()
	x := explorerOver(srv, nil)
	c := audience.CustomCriteria{EducationEnrolled: true, EducationTopics: []string{"Kubernetes"}, Label: "KubeCon"}

	p, err := x.PreviewCustom(context.Background(), "proj-1", c, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"course_name"}, p.EducationTopicCandidates)
	assert.Equal(t, "course_name", p.Criteria.EducationTopicProperty)

	c.EducationTopicProperty = "course_topic"
	p, err = x.PreviewCustom(context.Background(), "proj-1", c, "")
	require.NoError(t, err)
	assert.Equal(t, "course_name", p.Criteria.EducationTopicProperty, "an enumeration the caller picked is replaced")
}

// A portal whose only topic-named property is not a string is told so: the
// refusal names the type, not a property that is missing.
func TestPreviewCustom_ANonStringTopicPropertyIsRefusedForItsType(t *testing.T) {
	portal := seoulPortal()
	portal.eventProps = []string{"course_topic"}
	portal.eventPropTypes = map[string]string{"course_topic": "enumeration"}
	srv := portal.serve(t)
	defer srv.Close()
	c := audience.CustomCriteria{EducationEnrolled: true, EducationTopics: []string{"Kubernetes"}, Label: "KubeCon"}

	_, err := explorerOver(srv, nil).PreviewCustom(context.Background(), "proj-1", c, "")
	require.ErrorIs(t, err, audience.ErrInvalidRequest)
	assert.Contains(t, err.Error(), "no text (string) course or topic property")
}

// A taken name at the input limit previews as a version create accepts back.
func TestPreviewCustom_AVersionedNameFitsTheCreateLimit(t *testing.T) {
	portal := seoulPortal()
	long := strings.Repeat("n", audience.MaxListNameLength)
	portal.lists[long] = "990"
	srv := portal.serve(t)
	defer srv.Close()

	p, err := explorerOver(srv, nil).PreviewCustom(context.Background(), "proj-1", seoulCriteria(), long)
	require.NoError(t, err)
	assert.LessOrEqual(t, len([]rune(p.Name)), audience.CustomNameMaxLen)
	assert.True(t, strings.HasSuffix(p.Name, " (v2)"), "the taken name is versioned, not reused: %q", p.Name)
}

// An answer that narrows the audience reaches the reuse ranking, and a general list
// the model still calls "reuse" is held to "update": using it as-is would reach the
// people the answer ruled out.
func TestInterpretCustom_ReuseIsJudgedAgainstTheAnswers(t *testing.T) {
	portal := seoulPortal()
	portal.lists["26Q3 - Seoul - Mailable Contacts - OSS Korea"] = "411"
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":false,"education_enrolled":false,"gate_event_history":false,"brand_short":"LF","property_filters":[],"speakers":{"scope":"current","event_names":["OSS Korea 2026"]},"ambassadors":false,"education_topics":[],"page_view_urls":[],"registered_for":[],"label":"OSS Korea"},"questions":[],"notes":[]}`,
		`{"lists":[{"list_id":"411","recommendation":"reuse","reason":"same city"}]}`,
	}}
	answers := []audience.CustomAnswer{{QuestionID: "who", Question: "Everyone in Seoul, or only speakers?", Answer: "Only speakers"}}

	out, err := explorerOver(srv, llm).InterpretCustom(context.Background(), "proj-1", "Seoul contacts", "", answers)
	require.NoError(t, err)
	require.Len(t, llm.prompts, 2)
	assert.Contains(t, llm.prompts[1], "Everyone in Seoul, or only speakers? => Only speakers")
	assert.Contains(t, llm.prompts[1], "restriction: only event speakers (Current)")
	require.Len(t, out.Similar, 1)
	assert.Equal(t, audience.CustomReuseUpdate, out.Similar[0].Recommendation)
}

// A derived name joining several long locations is cut to the create limit too.
func TestPreviewCustom_ADerivedNameFitsTheCreateLimit(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	c := seoulCriteria()
	c.Cities = []string{strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100)}

	p, err := explorerOver(srv, nil).PreviewCustom(context.Background(), "proj-1", c, "")
	require.NoError(t, err)
	assert.LessOrEqual(t, len([]rune(p.Name)), audience.CustomNameMaxLen)
}

// The warehouse's past editions are registered_for's default, not an override: an
// operator who named editions gets exactly those (in the warehouse's spelling), one
// who excluded past registrants gets none, and a guessed name never reaches a filter.
func TestInterpretCustom_TheRequestedEditionsWinOverTheWarehouseDefault(t *testing.T) {
	events := []snowflake.Event{{EventName: "Synthetic Summit 2024"}, {EventName: "Synthetic Summit 2025"}}
	for name, tc := range map[string]struct {
		reply   string // the model's registered_for
		exclude bool   // the model set include_past_editions to false
		want    []string
		note    string
	}{
		"nothing named takes the default":  {reply: `"registered_for":[]`, want: []string{"Synthetic Summit 2024", "Synthetic Summit 2025"}},
		"a named edition replaces it":      {reply: `"registered_for":["synthetic  summit 2025"]`, want: []string{"Synthetic Summit 2025"}},
		"the event's own edition is known": {reply: `"registered_for":["Synthetic Summit 2026"]`, want: []string{"Synthetic Summit 2026"}},
		"excluded past registrants":        {reply: `"registered_for":[]`, exclude: true, want: nil},
		"only a guessed name":              {reply: `"registered_for":["Synthetic Summit Tokyo 2025"]`, want: nil, note: "Synthetic Summit Tokyo 2025"},
		"a guessed name beside a real one": {reply: `"registered_for":["Synthetic Summit 2024","Synth Summit 24"]`, want: []string{"Synthetic Summit 2024"}, note: "Synth Summit 24"},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			srv := portal.serve(t)
			defer srv.Close()
			extra := ""
			if tc.exclude {
				extra = `"include_past_editions":false,`
			}
			criteria := `{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":true,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":[],` + tc.reply + `,"label":""},` + extra + `"questions":[],"notes":[]}`
			llm := &seqLLM{replies: []string{`{"eventName":"","brandShort":"SYN","eventDates":[]}`, criteria}}
			repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
			builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: events}, hubspot.WithBaseURL(srv.URL))
			x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit 2026", StartDate: "2026-03-02"}}, llm)

			out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, out.Draft.Criteria.RegisteredFor)
			if tc.note != "" {
				assert.Contains(t, strings.Join(out.Draft.Notes, "\n"), tc.note, "a dropped name is reported")
			}
		})
	}
}

// A page title without its year names this edition by its full stored name, so an
// exact registered_for filter for this edition's registrants matches them.
func TestInterpretCustom_ThisEditionIsNamedWithItsYear(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":true,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":[],"registered_for":["Synthetic Summit 2026"],"label":""},"questions":[],"notes":[]}`,
	}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: []snowflake.Event{{EventName: "Synthetic Summit 2025"}}}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Synthetic Summit 2026"}, out.Draft.Criteria.RegisteredFor, "this edition's full name is a known edition")
	require.NotNil(t, out.Event)
	assert.Equal(t, "Synthetic Summit", out.Event.Name, "the API reports the event as the page declares it, as discover does")
}

// With no past editions from the warehouse (unconfigured, failed, or a first
// edition) a named edition is still checked: only this edition's own name is
// known, and a guessed one is dropped with a note saying it could not be verified.
func TestInterpretCustom_NamedEditionsAreCheckedWithoutAWarehouse(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":true,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":[],"registered_for":["Synthetic Summit 2026","Synthetic Summit Tokyo 2025"],"label":""},"questions":[],"notes":[]}`,
	}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Synthetic Summit 2026"}, out.Draft.Criteria.RegisteredFor, "a guessed edition never reaches the exact filter")
	notes := strings.Join(out.Draft.Notes, "\n")
	assert.Contains(t, notes, "Synthetic Summit Tokyo 2025")
	assert.Contains(t, notes, "could not be verified")
}

// The warehouse's past editions are a REGIONAL audience's default. For speakers alone
// they would AND every speaker with having registered for a past edition, dropping the
// first-time speakers, so a roles-only audience gets none.
func TestInterpretCustom_ASpeakersOnlyAudienceGetsNoPastEditions(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":[],"countries":[],"contact_location":false,"event_registered":false,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":{"scope":"current","event_names":["Synthetic Summit 2026"]},"ambassadors":false,"education_topics":[],"page_view_urls":[],"registered_for":[],"label":""},"questions":[],"notes":[]}`,
	}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: []snowflake.Event{{EventName: "Synthetic Summit 2025"}}}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Empty(t, out.Draft.Criteria.RegisteredFor, "a speakers-only audience is not narrowed to past registrants")
}

// A topic-only education audience has no location, so it is not regional: past
// editions would add registrants who never enrolled.
func TestInterpretCustom_ATopicOnlyEducationAudienceGetsNoPastEditions(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":[],"countries":[],"contact_location":false,"event_registered":false,"education_enrolled":true,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":["Synthetics"],"page_view_urls":[],"registered_for":[],"label":""},"questions":[],"notes":[]}`,
	}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: []snowflake.Event{{EventName: "Synthetic Summit 2025"}}}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Empty(t, out.Draft.Criteria.RegisteredFor, "a topic-only education audience is not widened with past registrants")
}

// The event's default audience has no location, yet still gets the event's past
// editions -- added by the service, newest first and capped, never left to the
// model to copy from a cut-short, alphabetical EVENT block.
func TestInterpretCustom_TheDefaultEventAudienceGetsItsPastEditionsNewestFirst(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":[],"countries":[],"contact_location":false,"event_registered":false,"education_enrolled":true,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":["Synthetics"],"page_view_urls":["events.example.org/synthetic-summit"],"registered_for":[],"label":""},"include_past_editions":true,"questions":[],"notes":[]}`,
	}}
	events := make([]snowflake.Event, 0, 40)
	for year := 1990; year < 2026; year++ {
		events = append(events, snowflake.Event{EventName: fmt.Sprintf("Synthetic Summit %d", year)})
	}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: events}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit 2026", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	c := out.Draft.Criteria
	assert.Empty(t, c.Cities, "an event audience is not location-scoped by default")
	require.Len(t, c.RegisteredFor, audience.CustomMaxRegisteredFor)
	assert.Equal(t, "Synthetic Summit 2025", c.RegisteredFor[0], "the most recent editions are kept")
	assert.Contains(t, strings.Join(out.Draft.Notes, "\n"), "11 older ones were left out")
}

// An event URL reached through a tracking link reaches the prompt as the page alone, so the
// copied page_view_urls entry is the event page and the default still gets its past editions.
func TestInterpretCustom_ATrackingLinkEventURLStillGetsTheDefault(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":[],"countries":[],"contact_location":false,"event_registered":false,"education_enrolled":true,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":["Synthetics"],"page_view_urls":["https://events.example.org/synthetic-summit"],"registered_for":[],"label":""},"include_past_editions":true,"questions":[],"notes":[]}`,
	}}
	events := []snowflake.Event{{EventName: "Synthetic Summit 2025"}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: events}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit 2026", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit?utm_source=newsletter#agenda", nil)
	require.NoError(t, err)
	require.Len(t, llm.prompts, 2)
	assert.Contains(t, llm.prompts[1], "url: https://events.example.org/synthetic-summit\n")
	assert.NotContains(t, llm.prompts[1], "utm_source")
	assert.Equal(t, []string{"Synthetic Summit 2025"}, out.Draft.Criteria.RegisteredFor)
}

// A label filled in from a long page title is cut to the contract's 200 runes, so
// the unedited draft still passes preview.
func TestInterpretCustom_ALongPageTitleIsCutToTheLabelLimit(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":false,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":[],"registered_for":[],"label":""},"questions":[],"notes":[]}`,
	}}
	x := NewAudienceExplorer(NewAudienceBuilder(&scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL)),
		&stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: strings.Repeat("é", audience.CustomLabelMaxLen), StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.LessOrEqual(t, len([]rune(out.Draft.Criteria.Label)), audience.CustomLabelMaxLen, "the year pushed the title past the limit")
}

// An edition name longer than a display field survives the prompt and the parser
// whole, so the model copying it back still matches the known edition.
func TestInterpretCustom_ALongEditionNameStaysWhole(t *testing.T) {
	title := strings.Repeat("Synthetic ", 22) + "Summit"
	edition := title + " 2026"
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":["Seoul"],"countries":[],"contact_location":true,"event_registered":true,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":[],"registered_for":["` + edition + `"],"label":""},"questions":[],"notes":[]}`,
	}}
	x := NewAudienceExplorer(NewAudienceBuilder(&scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}, identityEncryptor{}, &recordingResolver{events: []snowflake.Event{{EventName: title + " 2025"}}}, hubspot.WithBaseURL(srv.URL)),
		&stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: title, StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	require.Greater(t, len([]rune(edition)), 200)
	assert.Equal(t, []string{edition}, out.Draft.Criteria.RegisteredFor, "the long edition name is kept whole and known")
	require.Len(t, llm.prompts, 2)
	assert.Contains(t, llm.prompts[1], "name: "+edition, "the model is shown the whole edition name, or it cannot copy it")
}

// A list only some branches include is a real exclusion at send time: for "A OR B",
// suppressing A still leaves B minus A. Only a list every branch requires (the whole
// audience is inside it) is no suppression.
func TestRunQA_ASendTimeListOneBranchIncludesStillExcludes(t *testing.T) {
	portal := seoulPortal()
	portal.lists["26Q4 - Seoul - Combined"] = "500"
	portal.lists["LF Global Opt-Outs"] = "602"
	portal.lists["26Q4 - Seoul - Web Visitors"] = "603"
	inList := func(id string) string {
		return `{"filterBranchType":"AND","filterBranches":[],"filters":[{"filterType":"IN_LIST","operator":"IN_LIST","listId":` + id + `}]}`
	}
	portal.branches = map[string]string{
		"500": `{"filterBranchType":"OR","filters":[],"filterBranches":[` + inList("602") + `,` + inList("603") + `]}`,
	}
	srv := portal.serve(t)
	defer srv.Close()
	x := explorerOver(srv, nil)

	out, err := x.RunQA(context.Background(), "proj-1", "500", "", false, false, []string{"602"})
	require.NoError(t, err)
	assert.Equal(t, 1, out.Checks.ExclusionCompleteness.SendTimeCount, "suppressing one OR branch still excludes contacts")
	assert.True(t, out.Checks.Suppression.AppliedOptOut, "and its name still satisfies the opt-out check")

	// When every branch requires the list, the whole audience is inside it.
	portal.branches["500"] = `{"filterBranchType":"OR","filters":[],"filterBranches":[` + inList("602") + `]}`
	out, err = x.RunQA(context.Background(), "proj-1", "500", "", false, false, []string{"602"})
	require.NoError(t, err)
	assert.Zero(t, out.Checks.ExclusionCompleteness.SendTimeCount, "a list the whole audience is inside is no suppression")
}

// This edition's registrants included as one branch and suppressed at send time are
// not sent to, so check 4 does not fail on them; an unverifiable send-time id does
// not count, and the registrants list is still flagged.
func TestRunQA_ASendTimeSuppressedRegistrantsListPassesCheckFour(t *testing.T) {
	portal := seoulPortal()
	portal.lists["26Q3 - Seoul - Registration Push"] = "500"
	portal.lists["26Q3 - AGNTCon + MCPCon Japan 2026 Registrants"] = "29895"
	portal.lists["26Q3 - Seoul - Prospects"] = "603"
	inList := func(id string) string {
		return `{"filterBranchType":"AND","filterBranches":[],"filters":[{"filterType":"IN_LIST","operator":"IN_LIST","listId":` + id + `}]}`
	}
	portal.branches = map[string]string{
		"500": `{"filterBranchType":"OR","filters":[],"filterBranches":[` + inList("29895") + `,` + inList("603") + `]}`,
	}
	srv := portal.serve(t)
	defer srv.Close()
	x := explorerOver(srv, nil)
	const event = "AGNTCon + MCPCon Japan 2026"

	suppressed, err := x.RunQA(context.Background(), "proj-1", "500", event, false, false, []string{"29895"})
	require.NoError(t, err)
	assert.NotEqual(t, audience.VerdictFail, suppressed.Checks.CurrentRegistrants.Verdict, "the send excludes this edition's registrants")

	unverified, err := x.RunQA(context.Background(), "proj-1", "500", event, false, false, []string{"404404"})
	require.NoError(t, err)
	assert.Equal(t, audience.VerdictFail, unverified.Checks.CurrentRegistrants.Verdict, "an unreadable send-time id proves nothing")
}

// Check 4 reads what the send provably does not reach: a verified send-time
// suppression even when one branch of the list already excludes it, and every list a
// suppression wrapper fully covers. A wrapper that only intersects a list covers none
// of it.
func TestRunQA_CheckFourCreditsEverythingTheSendExcludes(t *testing.T) {
	const event = "AGNTCon + MCPCon Japan 2026"
	in := func(id string) string { return `{"filterType":"IN_LIST","operator":"IN_LIST","listId":` + id + `}` }
	notIn := func(id string) string { return `{"filterType":"IN_LIST","operator":"NOT_IN_LIST","listId":` + id + `}` }
	and := func(f ...string) string {
		return `{"filterBranchType":"AND","filterBranches":[],"filters":[` + strings.Join(f, ",") + `]}`
	}
	or := func(b ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	for name, tc := range map[string]struct {
		audience, wrapper string
		send              string
		want              audience.Verdict
		sameVerdict       bool
	}{
		"R OR (P AND NOT R), R suppressed": {audience: or(and(in("29895")), and(in("603"), notIn("29895"))), send: "29895"},
		"R OR P, wrapper R OR OptOut":      {audience: or(and(in("29895")), and(in("603"))), wrapper: or(and(in("29895")), and(in("602"))), send: "700"},
		"R OR P, wrapper R AND OptOut":     {audience: or(and(in("29895")), and(in("603"))), wrapper: and(in("29895"), in("602")), send: "700", want: audience.VerdictFail, sameVerdict: true},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists["26Q3 - Seoul - Registration Push"] = "500"
			portal.lists["26Q3 - AGNTCon + MCPCon Japan 2026 Registrants"] = "29895"
			portal.lists["26Q3 - Seoul - Prospects"] = "603"
			portal.lists["LF Global Opt-Outs"] = "602"
			portal.lists["26Q3 - Combined Suppression"] = "700"
			portal.branches = map[string]string{"500": tc.audience}
			if tc.wrapper != "" {
				portal.branches["700"] = tc.wrapper
			}
			srv := portal.serve(t)
			defer srv.Close()

			out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", event, false, false, []string{tc.send})
			require.NoError(t, err)
			if tc.sameVerdict {
				assert.Equal(t, tc.want, out.Checks.CurrentRegistrants.Verdict, "an intersection covers none of the registrants")
				return
			}
			assert.NotEqual(t, audience.VerdictFail, out.Checks.CurrentRegistrants.Verdict, "the send excludes every current registrant")
		})
	}
}

// A suppression wrapper credits only the children it fully covers: "GDPR OR Opt-Outs"
// applies both protections, "GDPR AND Opt-Outs" applies neither, since a contact in
// only one child stays eligible.
func TestRunQA_ASuppressionWrapperCreditsOnlyWhatItFullyCovers(t *testing.T) {
	in := func(id string) string { return `{"filterType":"IN_LIST","operator":"IN_LIST","listId":` + id + `}` }
	and := func(f ...string) string {
		return `{"filterBranchType":"AND","filterBranches":[],"filters":[` + strings.Join(f, ",") + `]}`
	}
	or := func(b ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	for name, tc := range map[string]struct {
		wrapper string
		applied bool
	}{
		"union":        {wrapper: or(and(in("601")), and(in("602"))), applied: true},
		"intersection": {wrapper: or(and(in("601"), in("602"))), applied: false},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists["26Q4 - Seoul - Mailable Contacts"] = "500"
			portal.lists["LF Europe GDPR Suppression"] = "601"
			portal.lists["LF Global Opt-Outs"] = "602"
			portal.lists["26Q4 - Combined Suppression"] = "700"
			portal.branches = map[string]string{"700": tc.wrapper}
			srv := portal.serve(t)
			defer srv.Close()

			out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "", true, false, []string{"700"})
			require.NoError(t, err)
			assert.Equal(t, tc.applied, out.Checks.Suppression.AppliedGDPR, "GDPR credited")
			assert.Equal(t, tc.applied, out.Checks.Suppression.AppliedOptOut, "opt-out credited")
		})
	}
}

// A send-time wrapper that fully covers the audience itself excludes every contact,
// so, like the audited list sent directly, it is no suppression: neither for the
// audited list nor for a list every branch requires.
func TestRunQA_ASuppressionWrapperCoveringTheAudienceIsNoSuppression(t *testing.T) {
	in := func(id string) string { return `{"filterType":"IN_LIST","operator":"IN_LIST","listId":` + id + `}` }
	and := func(f ...string) string {
		return `{"filterBranchType":"AND","filterBranches":[],"filters":[` + strings.Join(f, ",") + `]}`
	}
	or := func(b ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	for name, tc := range map[string]struct {
		audience, wrapper string
	}{
		"wrapper covers the audited list":        {wrapper: or(and(in("500")), and(in("602")))},
		"wrapper covers a list every branch has": {audience: or(and(in("610"), in("611")), and(in("610"))), wrapper: or(and(in("610")), and(in("602")))},
		"wrapper covers every OR alternative":    {audience: or(and(in("610")), and(in("611"))), wrapper: or(and(in("610")), and(in("611")))},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists["26Q4 - Seoul - Mailable Contacts"] = "500"
			portal.lists["LF Global Opt-Outs"] = "602"
			portal.lists["26Q4 - Seoul - Base"] = "610"
			portal.lists["26Q4 - Seoul - Extra"] = "611"
			portal.lists["26Q4 - Combined Suppression"] = "700"
			portal.branches = map[string]string{"700": tc.wrapper}
			if tc.audience != "" {
				portal.branches["500"] = tc.audience
			}
			srv := portal.serve(t)
			defer srv.Close()

			out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "", false, false, []string{"700"})
			require.NoError(t, err)
			assert.Zero(t, out.Checks.ExclusionCompleteness.SendTimeCount, "a wrapper that excludes the whole audience is no suppression")
			assert.NotEqual(t, audience.VerdictPass, out.Checks.ExclusionCompleteness.Verdict)
		})
	}
}

// A suppression covering only one alternative of an OR audience still leaves the rest,
// so it is credited.
func TestRunQA_ASuppressionWrapperCoveringOneAlternativeStillCounts(t *testing.T) {
	in := func(id string) string { return `{"filterType":"IN_LIST","operator":"IN_LIST","listId":` + id + `}` }
	and := func(f ...string) string {
		return `{"filterBranchType":"AND","filterBranches":[],"filters":[` + strings.Join(f, ",") + `]}`
	}
	or := func(b ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	portal := seoulPortal()
	portal.lists["26Q4 - Seoul - Mailable Contacts"] = "500"
	portal.lists["LF Global Opt-Outs"] = "602"
	portal.lists["26Q4 - Seoul - Base"] = "610"
	portal.lists["26Q4 - Seoul - Extra"] = "611"
	portal.lists["26Q4 - Combined Suppression"] = "700"
	portal.branches = map[string]string{"500": or(and(in("610")), and(in("611"))), "700": or(and(in("610")), and(in("602")))}
	srv := portal.serve(t)
	defer srv.Close()

	out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "", false, false, []string{"700"})
	require.NoError(t, err)
	assert.Equal(t, 1, out.Checks.ExclusionCompleteness.SendTimeCount, "611 remains, so the wrapper is a real suppression")
}

// Separate send-time suppressions that together cover every path through the audience
// leave no recipients, so, like one wrapper doing the same, their credit is withheld.
func TestRunQA_SeparateSuppressionsCoveringTheAudienceTogetherAreNoSuppression(t *testing.T) {
	in := func(id string) string { return `{"filterType":"IN_LIST","operator":"IN_LIST","listId":` + id + `}` }
	and := func(f ...string) string {
		return `{"filterBranchType":"AND","filterBranches":[],"filters":[` + strings.Join(f, ",") + `]}`
	}
	or := func(b ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	portal := seoulPortal()
	portal.lists["26Q4 - Seoul - Mailable Contacts"] = "500"
	portal.lists["26Q4 - Seoul - Base"] = "610"
	portal.lists["26Q4 - Seoul - Extra"] = "611"
	portal.branches = map[string]string{"500": or(and(in("610")), and(in("611")))}
	srv := portal.serve(t)
	defer srv.Close()
	x := explorerOver(srv, nil)

	both, err := x.RunQA(context.Background(), "proj-1", "500", "", false, false, []string{"610", "611"})
	require.NoError(t, err)
	assert.Zero(t, both.Checks.ExclusionCompleteness.SendTimeCount, "together they exclude every recipient")
	assert.NotEqual(t, audience.VerdictPass, both.Checks.ExclusionCompleteness.Verdict)

	one, err := x.RunQA(context.Background(), "proj-1", "500", "", false, false, []string{"610"})
	require.NoError(t, err)
	assert.Equal(t, 1, one.Checks.ExclusionCompleteness.SendTimeCount, "one alternative alone leaves the other")
}

// Check 4 reads the editions a list's registration predicates select, not its name: a
// list named for "KubeCon 2026" that selects only 2025's registrants is a historical
// audience, while one whose predicate names 2026 is this edition's own registrants.
func TestRunQA_CheckFourReadsTheRegistrationPredicates(t *testing.T) {
	branch := func(eventOp, nameOp, edition string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[{"filterBranchType":"AND","filters":[],"filterBranches":[` +
			`{"filterBranchType":"UNIFIED_EVENTS","eventTypeId":"6-48984571","operator":"` + eventOp + `","filterBranches":[],` +
			`"filters":[{"filterType":"PROPERTY","property":"event_name","operation":{"operationType":"MULTISTRING","operator":"` + nameOp + `","values":["` + edition + `"]}}]}]}]}`
	}
	for name, tc := range map[string]struct {
		listName, eventOp, nameOp, edition string
		fail                               bool
	}{
		"a past edition under this edition's name": {listName: "26Q4 - KubeCon 2026 - Event Registration", eventOp: "HAS_COMPLETED", nameOp: "IS_EQUAL_TO", edition: "KubeCon 2025", fail: false},
		"this edition's own registrants":           {listName: "26Q4 - KubeCon 2026 - Event Registration", eventOp: "HAS_COMPLETED", nameOp: "IS_EQUAL_TO", edition: "KubeCon 2026", fail: true},
		// Negated predicates select the people who did NOT register; they are not this
		// edition's registrants, and the neutral name leaves nothing to flag.
		"not completed registration for 2026": {listName: "26Q4 - Seoul - Prospects", eventOp: "HAS_NOT_COMPLETED", nameOp: "IS_EQUAL_TO", edition: "KubeCon 2026", fail: false},
		"event name is not 2026":              {listName: "26Q4 - Seoul - Prospects", eventOp: "HAS_COMPLETED", nameOp: "IS_NOT_EQUAL_TO", edition: "KubeCon 2026", fail: false},
		// A substring names no edition, so the year-qualified name still decides.
		"a substring on this edition's list": {listName: "26Q4 - KubeCon 2026 - Event Registration", eventOp: "HAS_COMPLETED", nameOp: "CONTAINS", edition: "KubeCon", fail: true},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists[tc.listName] = "500"
			portal.branches = map[string]string{"500": branch(tc.eventOp, tc.nameOp, tc.edition)}
			srv := portal.serve(t)
			defer srv.Close()

			out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "KubeCon 2026", false, false, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.fail, out.Checks.CurrentRegistrants.Verdict == audience.VerdictFail)
		})
	}
}

// Edition evidence must hold on every registration path, and a verified suppression's
// inline registration predicate excludes its edition whole only when nothing else
// narrows it.
func TestRunQA_CheckFourEditionsAcrossPathsAndInlineSuppressions(t *testing.T) {
	ue := func(nameOp string, editions []string, extra string) string {
		filters := ""
		if len(editions) > 0 {
			filters = `{"filterType":"PROPERTY","property":"event_name","operation":{"operationType":"MULTISTRING","operator":"` + nameOp + `","values":["` + strings.Join(editions, `","`) + `"]}}`
		}
		if extra != "" {
			if filters != "" {
				filters += ","
			}
			filters += extra
		}
		return `{"filterBranchType":"UNIFIED_EVENTS","eventTypeId":"6-48984571","operator":"HAS_COMPLETED","filterBranches":[],"filters":[` + filters + `]}`
	}
	and := func(b ...string) string {
		return `{"filterBranchType":"AND","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	or := func(b ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	narrowing := `{"filterType":"PROPERTY","property":"registration_type","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["Speaker"]}}`
	for name, tc := range map[string]struct {
		listName, audience, wrapper string
		send                        []string
		fail                        bool
	}{
		"a constrained and an unrestricted path": {
			listName: "26Q4 - KubeCon 2026 Registrants",
			audience: or(and(ue("IS_EQUAL_TO", []string{"KubeCon 2025"}, "")), and(ue("", nil, ""))),
			fail:     true,
		},
		"this edition excluded by an inline suppression": {
			listName: "26Q4 - KubeCon Registrants",
			audience: or(and(ue("IS_ANY_OF", []string{"KubeCon 2025", "KubeCon 2026"}, ""))),
			wrapper:  or(and(ue("IS_EQUAL_TO", []string{"KubeCon 2026"}, ""))),
			send:     []string{"700"},
			fail:     false,
		},
		"an inline suppression narrowed by another condition": {
			listName: "26Q4 - KubeCon Registrants",
			audience: or(and(ue("IS_ANY_OF", []string{"KubeCon 2025", "KubeCon 2026"}, ""))),
			wrapper:  or(and(ue("IS_EQUAL_TO", []string{"KubeCon 2026"}, narrowing))),
			send:     []string{"700"},
			fail:     true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists[tc.listName] = "500"
			portal.lists["26Q4 - Combined Suppression"] = "700"
			portal.branches = map[string]string{"500": tc.audience}
			if tc.wrapper != "" {
				portal.branches["700"] = tc.wrapper
			}
			srv := portal.serve(t)
			defer srv.Close()

			out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "KubeCon 2026", false, false, tc.send)
			require.NoError(t, err)
			assert.Equal(t, tc.fail, out.Checks.CurrentRegistrants.Verdict == audience.VerdictFail)
		})
	}
}

// A referenced registration list is judged by its own exact predicates, less the
// editions a verified inline suppression excludes whole; a referenced list with no
// such predicates keeps its name as evidence.
func TestRunQA_CheckFourAppliesInlineSuppressionToReferencedLists(t *testing.T) {
	ue := func(editions ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[{"filterBranchType":"AND","filters":[],"filterBranches":[` +
			`{"filterBranchType":"UNIFIED_EVENTS","eventTypeId":"6-48984571","operator":"HAS_COMPLETED","filterBranches":[],"filters":[` +
			`{"filterType":"PROPERTY","property":"event_name","operation":{"operationType":"MULTISTRING","operator":"IS_ANY_OF","values":["` + strings.Join(editions, `","`) + `"]}}]}]}]}`
	}
	in := func(id string) string { return `{"filterType":"IN_LIST","operator":"IN_LIST","listId":` + id + `}` }
	audienceTree := `{"filterBranchType":"OR","filters":[],"filterBranches":[` +
		`{"filterBranchType":"AND","filterBranches":[],"filters":[` + in("801") + `]},` +
		`{"filterBranchType":"AND","filterBranches":[],"filters":[` + in("603") + `]}]}`
	for name, tc := range map[string]struct {
		send []string
		fail bool
	}{
		"2026 suppressed inline": {send: []string{"700"}, fail: false},
		"nothing suppressed":     {send: nil, fail: true},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists["26Q4 - Seoul - Registration Push"] = "500"
			portal.lists["26Q4 - KubeCon 2026 Registrants"] = "801"
			portal.lists["26Q4 - Seoul - Prospects"] = "603"
			portal.lists["26Q4 - Combined Suppression"] = "700"
			portal.branches = map[string]string{"500": audienceTree, "801": ue("KubeCon 2026"), "700": ue("KubeCon 2026")}
			srv := portal.serve(t)
			defer srv.Close()

			out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "KubeCon 2026", false, false, tc.send)
			require.NoError(t, err)
			assert.Equal(t, tc.fail, out.Checks.CurrentRegistrants.Verdict == audience.VerdictFail)
		})
	}
}

// Edition evidence replaces a list's name only when every path runs through an exact
// registration branch: an OR alternative that is a list membership can hold this
// edition's registrants, so the name decides.
func TestRunQA_EditionsReplaceTheNameOnlyWhenEveryPathIsARegistration(t *testing.T) {
	ue := `{"filterBranchType":"UNIFIED_EVENTS","eventTypeId":"6-48984571","operator":"HAS_COMPLETED","filterBranches":[],"filters":[` +
		`{"filterType":"PROPERTY","property":"event_name","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["KubeCon 2025"]}}]}`
	tree := `{"filterBranchType":"OR","filters":[],"filterBranches":[` +
		`{"filterBranchType":"AND","filters":[],"filterBranches":[` + ue + `]},` +
		`{"filterBranchType":"AND","filterBranches":[],"filters":[{"filterType":"IN_LIST","operator":"IN_LIST","listId":905}]}]}`
	for name, audited := range map[string]bool{"the audited list": true, "a referenced list": false} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists["26Q4 - KubeCon 2026 Registrants"] = "801"
			portal.lists["26Q4 - Imported Contacts"] = "905"
			portal.lists["26Q4 - Seoul - Registration Push"] = "500"
			portal.branches = map[string]string{"801": tree}
			id := "801"
			if !audited {
				portal.branches["500"] = `{"filterBranchType":"OR","filters":[],"filterBranches":[{"filterBranchType":"AND","filterBranches":[],"filters":[{"filterType":"IN_LIST","operator":"IN_LIST","listId":801}]}]}`
				id = "500"
			}
			srv := portal.serve(t)
			defer srv.Close()

			out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", id, "KubeCon 2026", false, false, nil)
			require.NoError(t, err)
			assert.Equal(t, audience.VerdictFail, out.Checks.CurrentRegistrants.Verdict, "the membership branch may hold 2026 registrants; the name decides")
		})
	}
}

// A registration branch is positive evidence wherever it sits: a mixed list that
// explicitly includes this edition's registrants fails check 4 even when its name is
// neutral. And an audience made only of a registration predicate is empty when a
// suppression with the same predicate excludes it, so that suppression earns no credit.
func TestRunQA_MixedListsAndInlineEmptyAudience(t *testing.T) {
	ue := `{"filterBranchType":"UNIFIED_EVENTS","eventTypeId":"6-48984571","operator":"HAS_COMPLETED","filterBranches":[],"filters":[` +
		`{"filterType":"PROPERTY","property":"event_name","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["KubeCon 2026"]}}]}`
	regPath := `{"filterBranchType":"AND","filters":[],"filterBranches":[` + ue + `]}`
	membership := `{"filterBranchType":"AND","filterBranches":[],"filters":[{"filterType":"IN_LIST","operator":"IN_LIST","listId":905}]}`
	location := `{"filterBranchType":"AND","filterBranches":[],"filters":[{"filterType":"PROPERTY","property":"city","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["Seoul"]}},{"filterType":"IN_LIST","operator":"IN_LIST","listId":26716}]}`
	or := func(b ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	for name, tc := range map[string]struct {
		listName, tree string
	}{
		"registration OR a membership list": {listName: "26Q4 - Seoul Registration Push", tree: or(regPath, membership)},
		"location OR registered_for":        {listName: "26Q4 - Seoul - Mailable Contacts - KubeCon 2026", tree: or(location, regPath)},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists[tc.listName] = "500"
			portal.lists["26Q4 - Imported Contacts"] = "905"
			portal.branches = map[string]string{"500": tc.tree}
			srv := portal.serve(t)
			defer srv.Close()

			out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "KubeCon 2026", false, false, nil)
			require.NoError(t, err)
			assert.Equal(t, audience.VerdictFail, out.Checks.CurrentRegistrants.Verdict, "the explicit 2026 registration branch is included")
		})
	}

	t.Run("an inline suppression emptying the audience", func(t *testing.T) {
		portal := seoulPortal()
		portal.lists["26Q4 - KubeCon Registrants"] = "500"
		portal.lists["26Q4 - Combined Suppression"] = "700"
		portal.branches = map[string]string{"500": or(regPath), "700": or(regPath)}
		srv := portal.serve(t)
		defer srv.Close()

		out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "KubeCon 2026", false, false, []string{"700"})
		require.NoError(t, err)
		assert.Zero(t, out.Checks.ExclusionCompleteness.SendTimeCount, "a suppression leaving no recipients is no suppression")
		assert.NotEqual(t, audience.VerdictPass, out.Checks.ExclusionCompleteness.Verdict)
	})
}

// A composed master that excludes this edition's registrants through its combined
// suppression on every branch does not fail check 4 because an included list carries
// a registration branch for them; the same exclusion on one branch only still fails.
func TestRunQA_TheMastersOwnSuppressionClearsCheckFour(t *testing.T) {
	ue := `{"filterBranchType":"UNIFIED_EVENTS","eventTypeId":"6-48984571","operator":"HAS_COMPLETED","filterBranches":[],"filters":[` +
		`{"filterType":"PROPERTY","property":"event_name","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["KubeCon 2026"]}}]}`
	or := func(b ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	f := func(op, id string) string {
		return `{"filterType":"IN_LIST","operator":"` + op + `","listId":` + id + `}`
	}
	and := func(fs ...string) string {
		return `{"filterBranchType":"AND","filterBranches":[],"filters":[` + strings.Join(fs, ",") + `]}`
	}
	mixed := or(`{"filterBranchType":"AND","filterBranches":[],"filters":[{"filterType":"PROPERTY","property":"city","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["Seoul"]}}]}`,
		`{"filterBranchType":"AND","filters":[],"filterBranches":[`+ue+`]}`)
	suppression := or(`{"filterBranchType":"AND","filters":[],"filterBranches":[` + ue + `]}`)
	for name, tc := range map[string]struct {
		master string
		fail   bool
	}{
		"excluded on every branch": {master: or(and(f("IN_LIST", "801"), f("NOT_IN_LIST", "700")), and(f("IN_LIST", "603"), f("NOT_IN_LIST", "700"))), fail: false},
		"excluded on one branch":   {master: or(and(f("IN_LIST", "801"), f("NOT_IN_LIST", "700")), and(f("IN_LIST", "603"))), fail: true},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists["26Q4 - Seoul - Master"] = "500"
			portal.lists["26Q4 - Seoul - Mixed"] = "801"
			portal.lists["26Q4 - Seoul - Prospects"] = "603"
			portal.lists["26Q4 - Combined Suppression"] = "700"
			portal.branches = map[string]string{"500": tc.master, "801": mixed, "700": suppression}
			srv := portal.serve(t)
			defer srv.Close()

			out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "KubeCon 2026", false, false, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.fail, out.Checks.CurrentRegistrants.Verdict == audience.VerdictFail)
		})
	}
}

// A master whose own exclusion removes every contact it includes has no recipients;
// check 4 reports that rather than passing because this edition's registrants are gone.
func TestRunQA_AnEmptiedMasterIsNotAPass(t *testing.T) {
	ue := `{"filterBranchType":"UNIFIED_EVENTS","eventTypeId":"6-48984571","operator":"HAS_COMPLETED","filterBranches":[],"filters":[` +
		`{"filterType":"PROPERTY","property":"event_name","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["KubeCon 2026"]}}]}`
	or := func(b ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	reg := or(`{"filterBranchType":"AND","filters":[],"filterBranches":[` + ue + `]}`)
	portal := seoulPortal()
	portal.lists["26Q4 - KubeCon 2026 - Master"] = "500"
	portal.lists["26Q4 - KubeCon 2026 Registrants"] = "801"
	portal.lists["26Q4 - Combined Suppression"] = "700"
	portal.branches = map[string]string{
		"500": or(`{"filterBranchType":"AND","filterBranches":[],"filters":[{"filterType":"IN_LIST","operator":"IN_LIST","listId":801},{"filterType":"IN_LIST","operator":"NOT_IN_LIST","listId":700}]}`),
		"801": reg,
		"700": reg,
	}
	srv := portal.serve(t)
	defer srv.Close()

	out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "KubeCon 2026", false, false, nil)
	require.NoError(t, err)
	assert.Equal(t, audience.VerdictNeedsVerify, out.Checks.CurrentRegistrants.Verdict)
	require.NotEmpty(t, out.Checks.CurrentRegistrants.Findings)
	assert.Contains(t, out.Checks.CurrentRegistrants.Findings[0].Message, "reach no one")
}

// An included list that excludes this edition's registrants on every one of its own
// paths has no current registrants to contribute, so a master including it passes
// check 4. The credit stays with that list: a sibling branch of the master that
// includes the registrants directly still fails.
func TestRunQA_AnIncludedListsOwnExclusionClearsItsEvidence(t *testing.T) {
	ue := `{"filterBranchType":"UNIFIED_EVENTS","eventTypeId":"6-48984571","operator":"HAS_COMPLETED","filterBranches":[],"filters":[` +
		`{"filterType":"PROPERTY","property":"event_name","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["KubeCon 2026"]}}]}`
	or := func(b ...string) string {
		return `{"filterBranchType":"OR","filters":[],"filterBranches":[` + strings.Join(b, ",") + `]}`
	}
	f := func(op, id string) string {
		return `{"filterType":"IN_LIST","operator":"` + op + `","listId":` + id + `}`
	}
	and := func(fs ...string) string {
		return `{"filterBranchType":"AND","filterBranches":[],"filters":[` + strings.Join(fs, ",") + `]}`
	}
	city := `{"filterType":"PROPERTY","property":"city","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["Seoul"]}}`
	// Seoul contacts OR this edition's registrants, excluding the combined suppression on both paths.
	mixed := or(and(city, f("NOT_IN_LIST", "700")),
		`{"filterBranchType":"AND","filters":[`+f("NOT_IN_LIST", "700")+`],"filterBranches":[`+ue+`]}`)
	suppression := or(`{"filterBranchType":"AND","filters":[],"filterBranches":[` + ue + `]}`)
	for name, tc := range map[string]struct {
		master string
		fail   bool
	}{
		"the master includes only that list":            {master: or(and(f("IN_LIST", "801"))), fail: false},
		"a sibling branch includes the registrants too": {master: or(and(f("IN_LIST", "801")), and(f("IN_LIST", "802"))), fail: true},
	} {
		t.Run(name, func(t *testing.T) {
			portal := seoulPortal()
			portal.lists["26Q4 - Seoul - Master"] = "500"
			portal.lists["26Q4 - Seoul - Mixed"] = "801"
			portal.lists["26Q4 - Seoul - Registration Push"] = "802"
			portal.lists["26Q4 - Combined Suppression"] = "700"
			portal.branches = map[string]string{"500": tc.master, "801": mixed, "802": suppression, "700": suppression}
			srv := portal.serve(t)
			defer srv.Close()

			out, err := explorerOver(srv, nil).RunQA(context.Background(), "proj-1", "500", "KubeCon 2026", false, false, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.fail, out.Checks.CurrentRegistrants.Verdict == audience.VerdictFail)
		})
	}
}

// Visitors of a page that is NOT the event's page are a different segment: they get no past
// editions ORed in, which would reach people outside the requested visitors.
func TestInterpretCustom_VisitorsOfAnotherPageGetNoPastEditions(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":[],"countries":[],"contact_location":false,"event_registered":false,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":["events.example.org/synthetic-summit/cfp"],"registered_for":[],"label":""},"include_past_editions":true,"questions":[],"notes":[]}`,
	}}
	events := []snowflake.Event{{EventName: "Synthetic Summit 2025"}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: events}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit 2026", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"events.example.org/synthetic-summit/cfp"}, out.Draft.Criteria.PageViewURLs)
	assert.Empty(t, out.Draft.Criteria.RegisteredFor, "another page's visitors are not the event's default audience")
}

func TestInterpretCustom_VisitorsOnlyOfTheEventPageGetNoPastEditions(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":[],"countries":[],"contact_location":false,"event_registered":false,"education_enrolled":false,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":[],"page_view_urls":["events.example.org/synthetic-summit"],"registered_for":[],"label":""},"include_past_editions":true,"questions":[],"notes":[]}`,
	}}
	events := []snowflake.Event{{EventName: "Synthetic Summit 2025"}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: events}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit 2026", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "visitors of this event page only", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"events.example.org/synthetic-summit"}, out.Draft.Criteria.PageViewURLs)
	assert.Empty(t, out.Draft.Criteria.RegisteredFor, "a visitors-only draft is not the event's default audience")
}

func TestSelectsPage(t *testing.T) {
	const event = "https://events.example.org/synthetic-summit/"
	assert.True(t, selectsPage([]string{"events.example.org/synthetic-summit"}, event))
	assert.True(t, selectsPage([]string{"https://www.Events.example.org/synthetic-summit"}, event))
	assert.True(t, selectsPage([]string{"events.example.org/synthetic-summit"}, event+"?utm_source=newsletter"),
		"the event URL's own query only says how the operator reached the page")
	// HubSpot's page-view filter keeps the whole string, so a query narrows it to the visits
	// made with that query: not the page's visitors. utm_* tags included -- "?utm_source=x"
	// matches only the visits that arrived with that tag.
	assert.False(t, selectsPage([]string{"events.example.org/synthetic-summit?ticket=vip"}, event))
	assert.False(t, selectsPage([]string{"events.example.org/synthetic-summit?utm_source=x"}, event))
	assert.False(t, selectsPage([]string{"events.example.org/synthetic-summit#agenda"}, event))
	assert.False(t, selectsPage([]string{"events.example.org/synthetic-summit/cfp"}, event))
	assert.False(t, selectsPage([]string{"events.example.org/synthetic-summit"}, ""))
	assert.False(t, selectsPage(nil, event))
}

func TestSameEventEditions(t *testing.T) {
	same, others := sameEventEditions([]string{
		"Synthetic Summit 2025", "Synthetic Summit Workshop 2025", "synthetic  summit 2024", "Synthetic Summit Tokyo 2025",
	}, "Synthetic Summit 2026")
	assert.Equal(t, []string{"Synthetic Summit 2025", "synthetic  summit 2024"}, same, "kept verbatim, in order")
	assert.Equal(t, []string{"Synthetic Summit Workshop 2025", "Synthetic Summit Tokyo 2025"}, others)

	// The resolver's own year rule: a year against letters is still a year, a longer number is not.
	same, others = sameEventEditions([]string{"Synthetic Summit2025", "Synthetic Summit 202512"}, "Synthetic Summit 2026")
	assert.Equal(t, []string{"Synthetic Summit2025"}, same, "a year written against the name is that edition's year")
	assert.Equal(t, []string{"Synthetic Summit 202512"}, others, "six digits are not a year")
	same, _ = sameEventEditions([]string{"Synthetic Summit 2025"}, "Synthetic Summit2026")
	assert.Equal(t, []string{"Synthetic Summit 2025"}, same, "the event name's own adjacent year is removed too")

	same, others = sameEventEditions([]string{"2025"}, "2026")
	assert.Empty(t, same, "a name that is only a year has no family to match")
	assert.Equal(t, []string{"2025"}, others)
}

// The warehouse matches the event name as a substring, so it returns other events sharing it.
// The default adds only this event's own editions and names the rest in a note.
func TestInterpretCustom_DefaultLeavesOutOtherEventsTheWarehouseMatched(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":[],"countries":[],"contact_location":false,"event_registered":false,"education_enrolled":true,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":["Synthetics"],"page_view_urls":["events.example.org/synthetic-summit"],"registered_for":[],"label":""},"include_past_editions":true,"questions":[],"notes":[]}`,
	}}
	events := []snowflake.Event{{EventName: "Synthetic Summit 2025"}, {EventName: "Synthetic Summit Workshop 2025"}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: events}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit 2026", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Synthetic Summit 2025"}, out.Draft.Criteria.RegisteredFor)
	assert.Contains(t, strings.Join(out.Draft.Notes, "\n"), "Synthetic Summit Workshop 2025")
}

// The event page's visitors plus enrollees in some OTHER course carry the same two signals as
// the default audience. Without the model's explicit include_past_editions: true, no past
// editions are added.
func TestInterpretCustom_DefaultNeedsAnExplicitIncludePastEditions(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":[],"countries":[],"contact_location":false,"event_registered":false,"education_enrolled":true,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":["Unrelated Course"],"page_view_urls":["events.example.org/synthetic-summit"],"registered_for":[],"label":""},"questions":[],"notes":[]}`,
	}}
	events := []snowflake.Event{{EventName: "Synthetic Summit 2025"}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: events}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit 2026", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "event page visitors and Unrelated Course enrollees", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Empty(t, out.Draft.Criteria.RegisteredFor, "unset include_past_editions is not the default audience")
}

// A warehouse edition spelled with a double space is returned verbatim, so the exact
// event_name filter built from it matches its registrations.
func TestInterpretCustom_DoubleSpacedWarehouseEditionIsKeptVerbatim(t *testing.T) {
	portal := seoulPortal()
	srv := portal.serve(t)
	defer srv.Close()
	llm := &seqLLM{replies: []string{
		`{"eventName":"","brandShort":"SYN","eventDates":[]}`,
		`{"criteria":{"cities":[],"countries":[],"contact_location":false,"event_registered":false,"education_enrolled":true,"gate_event_history":false,"brand_short":"","property_filters":[],"speakers":null,"ambassadors":false,"education_topics":["Synthetics"],"page_view_urls":["events.example.org/synthetic-summit"],"registered_for":["Synthetic Summit 2024"],"label":""},"include_past_editions":true,"questions":[],"notes":[]}`,
	}}
	events := []snowflake.Event{{EventName: "Synthetic  Summit 2024"}}
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-1": activeHubSpotConn(goodHubSpotCreds)}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, &recordingResolver{events: events}, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, &stubFetcher{body: []byte("<html/>")}, stubParser{details: eventurl.EventDetails{Name: "Synthetic Summit 2026", StartDate: "2026-03-02"}}, llm)

	out, err := x.InterpretCustom(context.Background(), "proj-1", "", "https://events.example.org/synthetic-summit", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"Synthetic  Summit 2024"}, out.Draft.Criteria.RegisteredFor, "a named edition comes back as the warehouse spells it")
	criteria := out.Draft.Criteria
	criteria.EducationTopicProperty = "course_name" // preview resolves this from the portal
	raw, err := audience.CustomListFilter(criteria)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"Synthetic  Summit 2024"`, "and reaches the filter verbatim")
}

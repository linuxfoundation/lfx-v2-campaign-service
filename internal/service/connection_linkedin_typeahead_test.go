// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	connsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_connections/server"
	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// mockEntitySearcher is a LinkedIn dispatcher with only the typeahead capability.
type mockEntitySearcher struct {
	rows      []model.TargetingEntity
	err       error
	notCalled bool
	calls     int
	got       model.TargetingEntityQuery
	deadline  time.Duration
}

func (m *mockEntitySearcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockEntitySearcher) SearchTargetingEntities(ctx context.Context, _ string, _ model.Provider, q model.TargetingEntityQuery) ([]model.TargetingEntity, bool, error) {
	m.calls++
	m.got = q
	if dl, ok := ctx.Deadline(); ok {
		m.deadline = time.Until(dl)
	}
	return m.rows, !m.notCalled, m.err
}

func typeaheadService(d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: d}})
	return svc
}

func TestSearchLinkedinAdsTargetingEntities_MapsAndTrims(t *testing.T) {
	d := &mockEntitySearcher{rows: []model.TargetingEntity{
		{URN: "urn:li:skill:17", Name: "Kubernetes"},
		{URN: "urn:li:skill:900", Name: "Kubernetes Operators"},
	}}
	res, err := typeaheadService(d).SearchLinkedinAdsTargetingEntities(context.Background(), &conn.SearchLinkedinAdsTargetingEntitiesPayload{
		ProjectID: "cncf", Facet: "skills", Query: "  kube  ",
	})
	if err != nil {
		t.Fatalf("SearchLinkedinAdsTargetingEntities: %v", err)
	}
	if d.got != (model.TargetingEntityQuery{Facet: "skills", Query: "kube"}) {
		t.Errorf("query passed down = %+v, want the trimmed text", d.got)
	}
	if d.deadline <= 0 || d.deadline > targetingEntityCallTimeout {
		t.Errorf("dispatcher deadline %v, want within %v", d.deadline, targetingEntityCallTimeout)
	}
	if res.Facet != "skills" || res.EntityCount != 2 || len(res.Entities) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if *res.Entities[0] != (conn.LinkedinAdsTargetingEntity{Urn: "urn:li:skill:17", Name: "Kubernetes"}) ||
		*res.Entities[1] != (conn.LinkedinAdsTargetingEntity{Urn: "urn:li:skill:900", Name: "Kubernetes Operators"}) {
		t.Errorf("entities out of order or mis-mapped: %+v %+v", *res.Entities[0], *res.Entities[1])
	}
	// The wire body is the shape self-serve consumes.
	body, _ := json.Marshal(connsvr.NewSearchLinkedinAdsTargetingEntitiesResponseBody(res))
	want := `{"facet":"skills","entities":[{"urn":"urn:li:skill:17","name":"Kubernetes"},{"urn":"urn:li:skill:900","name":"Kubernetes Operators"}],"entity_count":2}`
	if string(body) != want {
		t.Errorf("wire body\n got %s\nwant %s", body, want)
	}
}

func TestSearchLinkedinAdsTargetingEntities_NoMatchIsAnEmptyArray(t *testing.T) {
	d := &mockEntitySearcher{rows: []model.TargetingEntity{}}
	res, err := typeaheadService(d).SearchLinkedinAdsTargetingEntities(context.Background(), &conn.SearchLinkedinAdsTargetingEntitiesPayload{
		ProjectID: "cncf", Facet: "locations", Query: "zzzz",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(connsvr.NewSearchLinkedinAdsTargetingEntitiesResponseBody(res))
	if !strings.Contains(string(body), `"entities":[]`) || res.EntityCount != 0 {
		t.Errorf("an empty answer must be entities [] with count 0: %s", body)
	}
}

// A caller that bypasses the Goa decoder is refused the same way, before the dispatcher.
func TestSearchLinkedinAdsTargetingEntities_RefusesBadInputBeforeTheDispatcher(t *testing.T) {
	cases := []struct {
		name, project, facet, query string
	}{
		{"reserved LF scope", model.SystemProjectID, "skills", "go"},
		{"segment facet", "cncf", "audienceMatchingSegments", "go"},
		{"non-typeahead facet", "cncf", "seniorities", "senior"},
		{"blank query", "cncf", "skills", "   "},
		{"one character after trimming", "cncf", "skills", " g "},
		{"long query", "cncf", "skills", strings.Repeat("x", linkedin.MaxTypeaheadQueryLen+1)},
		{"control character", "cncf", "skills", "go\r\nlang"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &mockEntitySearcher{rows: []model.TargetingEntity{}}
			_, err := typeaheadService(d).SearchLinkedinAdsTargetingEntities(context.Background(), &conn.SearchLinkedinAdsTargetingEntitiesPayload{
				ProjectID: tc.project, Facet: tc.facet, Query: tc.query,
			})
			if err == nil || d.calls != 0 {
				t.Errorf("err=%v calls=%d, want a refusal before the dispatcher", err, d.calls)
			}
			var bad *conn.BadRequestError
			if tc.project != model.SystemProjectID && !errors.As(err, &bad) {
				t.Errorf("err = %T %v, want 400", err, err)
			}
			if bad != nil && strings.Contains(bad.Message, tc.query) && strings.TrimSpace(tc.query) != "" {
				t.Errorf("the refusal echoes the query: %q", bad.Message)
			}
		})
	}
}

func TestSearchLinkedinAdsTargetingEntities_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"no connection and no LF fallback", domain.ErrNotFound, "404"},
		{"unusable connection", fmt.Errorf("%w: %w: x", domain.ErrConnectionNotUsable, domain.ErrCredentialsExpired), "400"},
		{"LF system row unusable", fmt.Errorf("%w: %w: x", domain.ErrSystemConnectionNotUsable, domain.ErrConnectionNotUsable), "500"},
		{"dispatcher refusal", fmt.Errorf("%w: x", domain.ErrTargetingQueryInvalid), "400"},
		{"upstream failure", errors.New("LinkedIn API GET adTargetingEntities -> 429"), "503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &mockEntitySearcher{err: tc.err}
			_, err := typeaheadService(d).SearchLinkedinAdsTargetingEntities(context.Background(), &conn.SearchLinkedinAdsTargetingEntitiesPayload{
				ProjectID: "cncf", Facet: "titles", Query: "engineer",
			})
			if got := errorCode(err); got != tc.want {
				t.Errorf("status = %s (%T %v), want %s", got, err, err, tc.want)
			}
		})
	}
	// A platform without the capability is a 400 and is never contacted.
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{}})
	_, err := svc.SearchLinkedinAdsTargetingEntities(context.Background(), &conn.SearchLinkedinAdsTargetingEntitiesPayload{ProjectID: "cncf", Facet: "titles", Query: "eng"})
	if got := errorCode(err); got != "400" {
		t.Errorf("no searcher = %s, want 400", got)
	}
}

func errorCode(err error) string {
	var (
		bad  *conn.BadRequestError
		nf   *conn.NotFoundError
		ise  *conn.InternalServerError
		unav *conn.ConnServiceUnavailableError
	)
	switch {
	case errors.As(err, &bad):
		return bad.Code
	case errors.As(err, &nf):
		return nf.Code
	case errors.As(err, &ise):
		return ise.Code
	case errors.As(err, &unav):
		return unav.Code
	}
	return fmt.Sprintf("%T", err)
}

func TestReadTargetingEntities_RecordsOnlyCallsThatReachedLinkedIn(t *testing.T) {
	cases := []struct {
		name string
		d    *mockEntitySearcher
		want int
	}{
		{"cache hit", &mockEntitySearcher{rows: []model.TargetingEntity{}, notCalled: true}, 0},
		{"no connection", &mockEntitySearcher{err: domain.ErrNotFound, notCalled: true}, 0},
		{"miss", &mockEntitySearcher{rows: []model.TargetingEntity{}}, 1},
		{"upstream failure", &mockEntitySearcher{err: errors.New("linkedin 500")}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: tc.d})
			o.SetMetrics(rec)
			_, _ = o.ReadTargetingEntities(context.Background(), "cncf", model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: "skills", Query: "a"})
			if got := len(rec.upstreamCalls()); got != tc.want {
				t.Errorf("%d upstream calls recorded, want %d", got, tc.want)
			}
		})
	}
	// nil with no error is a contract violation, not "no matches".
	o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: &mockEntitySearcher{}})
	if _, err := o.ReadTargetingEntities(context.Background(), "cncf", model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: "skills", Query: "a"}); err == nil {
		t.Error("a nil result with no error must fail")
	}
}

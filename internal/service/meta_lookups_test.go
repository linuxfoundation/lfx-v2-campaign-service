// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

type mockMetaAudienceDispatcher struct {
	deadline  time.Time
	res       *model.MetaCustomAudiences
	err       error
	notCalled bool
	calls     int
}

func (m *mockMetaAudienceDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockMetaAudienceDispatcher) ListMetaCustomAudiences(ctx context.Context, _ string, _ model.Provider) (*model.MetaCustomAudiences, bool, error) {
	m.calls++
	m.deadline, _ = ctx.Deadline()
	return m.res, !m.notCalled, m.err
}

func metaLookupService(d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderMetaAds: d}})
	return svc
}

func i64p(v int64) *int64 { return &v }

// Every field is mapped; an absent description and absent counts stay absent.
func TestListMetaCustomAudiences_MapsEveryField(t *testing.T) {
	d := &mockMetaAudienceDispatcher{res: &model.MetaCustomAudiences{AccountID: "act_777", Audiences: []model.MetaCustomAudience{
		{ID: "111", Name: "KubeCon registrants", Subtype: "CUSTOM", Description: "HubSpot upload", CountLower: i64p(12000), CountUpper: i64p(14100)},
		{ID: "222", Name: "Site visitors", Subtype: "WEBSITE"},
	}}}
	res, err := metaLookupService(d).ListMetaCustomAudiences(context.Background(), &conn.ListMetaCustomAudiencesPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListMetaCustomAudiences: %v", err)
	}
	if res.AccountID != "act_777" || res.AudienceCount != 2 || res.Truncated {
		t.Fatalf("res = %+v", res)
	}
	b, _ := json.Marshal(res.Audiences)
	const want = `[{"ID":"111","Name":"KubeCon registrants","Subtype":"CUSTOM","Description":"HubSpot upload","ApproximateCountLowerBound":12000,"ApproximateCountUpperBound":14100},` +
		`{"ID":"222","Name":"Site visitors","Subtype":"WEBSITE","Description":null,"ApproximateCountLowerBound":null,"ApproximateCountUpperBound":null}]`
	if string(b) != want {
		t.Errorf("rows = %s\nwant   %s", b, want)
	}
}

// q keeps names containing it (case and surrounding space ignored); results are sorted by name
// ignoring case, then id; the 500 cap applies after the filter, with truncated set.
func TestListMetaCustomAudiences_FiltersSortsThenCaps(t *testing.T) {
	rows := []model.MetaCustomAudience{{ID: "9", Name: "open source summit", Subtype: "CUSTOM"}, {ID: "3", Name: "Alpha", Subtype: "CUSTOM"}, {ID: "2", Name: "alpha", Subtype: "LOOKALIKE"}}
	for i := 0; i < metaCustomAudienceMax+1; i++ {
		rows = append(rows, model.MetaCustomAudience{ID: fmt.Sprint(1000 + i), Name: fmt.Sprintf("KubeCon %04d", metaCustomAudienceMax-i), Subtype: "WEBSITE"})
	}
	d := &mockMetaAudienceDispatcher{res: &model.MetaCustomAudiences{AccountID: "act_1", Audiences: rows}}
	svc := metaLookupService(d)

	res, err := svc.ListMetaCustomAudiences(context.Background(), &conn.ListMetaCustomAudiencesPayload{ProjectID: "cncf", Q: strp("  ALPHA ")})
	if err != nil || res.AudienceCount != 2 || res.Audiences[0].ID != "2" || res.Audiences[1].ID != "3" || res.Truncated {
		t.Fatalf("q+sort: err=%v res=%+v, want ids 2 then 3", err, res)
	}
	res, err = svc.ListMetaCustomAudiences(context.Background(), &conn.ListMetaCustomAudiencesPayload{ProjectID: "cncf", Q: strp("kubecon")})
	if err != nil || res.AudienceCount != metaCustomAudienceMax || !res.Truncated || res.Audiences[0].Name != "KubeCon 0000" {
		t.Fatalf("cap: err=%v count=%d truncated=%v first=%v, want %d sorted and truncated", err, res.AudienceCount, res.Truncated, res.Audiences[0].Name, metaCustomAudienceMax)
	}
	res, err = svc.ListMetaCustomAudiences(context.Background(), &conn.ListMetaCustomAudiencesPayload{ProjectID: "cncf"})
	if err != nil || res.AudienceCount != metaCustomAudienceMax || !res.Truncated || res.Audiences[0].ID != "2" {
		t.Fatalf("no q: err=%v count=%d first=%+v", err, res.AudienceCount, res.Audiences[0])
	}
}

// An account with no audience is an empty array, not null.
func TestListMetaCustomAudiences_EmptyIsAnEmptyArray(t *testing.T) {
	d := &mockMetaAudienceDispatcher{res: &model.MetaCustomAudiences{AccountID: "act_1", Audiences: []model.MetaCustomAudience{}}}
	res, err := metaLookupService(d).ListMetaCustomAudiences(context.Background(), &conn.ListMetaCustomAudiencesPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListMetaCustomAudiences: %v", err)
	}
	if b, _ := json.Marshal(res.Audiences); string(b) != "[]" || res.AudienceCount != 0 || res.Truncated {
		t.Errorf("audiences = %s count=%d truncated=%v", b, res.AudienceCount, res.Truncated)
	}
}

// A bad q is a 400 before the dispatcher is asked; the reserved scope is refused too.
func TestListMetaCustomAudiences_RejectsBadInputBeforeAnyUpstreamCall(t *testing.T) {
	for _, q := range []string{"", "   ", string(make([]rune, metaCustomAudienceQueryMaxLen+1))} {
		d := &mockMetaAudienceDispatcher{}
		_, err := metaLookupService(d).ListMetaCustomAudiences(context.Background(), &conn.ListMetaCustomAudiencesPayload{ProjectID: "cncf", Q: strp(q)})
		var bad *conn.BadRequestError
		if !errors.As(err, &bad) || d.calls != 0 {
			t.Errorf("q=%q: err=%v calls=%d, want a 400 and no dispatcher call", q, err, d.calls)
		}
	}
	d := &mockMetaAudienceDispatcher{}
	if _, err := metaLookupService(d).ListMetaCustomAudiences(context.Background(), &conn.ListMetaCustomAudiencesPayload{ProjectID: model.SystemProjectID}); err == nil || d.calls != 0 {
		t.Errorf("reserved scope: err=%v calls=%d, want a refusal and no call", err, d.calls)
	}
}

// The connection-state arms: no connection 404, unusable 400, an upstream failure 503.
func TestListMetaCustomAudiences_ClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"no connection", domain.ErrNotFound, "404"},
		{"unusable", fmt.Errorf("%w: no ad account", domain.ErrConnectionNotUsable), "400"},
		{"upstream", errors.New("meta: 500"), "503"},
		{"more audiences than the read publishes", fmt.Errorf("list: %w", meta.ErrCustomAudienceListTooLarge), "503"},
		{"the LF system row is unusable", fmt.Errorf("%w: no ad account", domain.ErrSystemConnectionNotUsable), "500"},
		{"credentials fail to decrypt", domain.ErrCredentialDecryptionFailed, "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &mockMetaAudienceDispatcher{err: tc.err, notCalled: tc.want == "404" || tc.want == "400"}
			_, err := metaLookupService(d).ListMetaCustomAudiences(context.Background(), &conn.ListMetaCustomAudiencesPayload{ProjectID: "cncf"})
			var got string
			var nf *conn.NotFoundError
			var bad *conn.BadRequestError
			var su *conn.ConnServiceUnavailableError
			var ise *conn.InternalServerError
			switch {
			case errors.As(err, &ise):
				got = ise.Code
			case errors.As(err, &nf):
				got = nf.Code
			case errors.As(err, &bad):
				got = bad.Code
			case errors.As(err, &su):
				got = su.Code
			}
			if got != tc.want {
				t.Fatalf("err = %T %v, want %s", err, err, tc.want)
			}
		})
	}
}

// A platform whose dispatcher cannot list custom audiences is a 400, not a 503.
func TestListMetaCustomAudiences_UnsupportedPlatform(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderMetaAds: &mockLookupDispatcher{}}})
	_, err := svc.ListMetaCustomAudiences(context.Background(), &conn.ListMetaCustomAudiencesPayload{ProjectID: "cncf"})
	var bad *conn.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %T %v, want a 400", err, err)
	}
}

// The read gets its own budget, longer than accountsCallTimeout, for its paged walk.
func TestReadMetaCustomAudiences_HasItsOwnTimeout(t *testing.T) {
	d := &mockMetaAudienceDispatcher{res: &model.MetaCustomAudiences{AccountID: "act_1", Audiences: []model.MetaCustomAudience{}}}
	o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderMetaAds: d}}
	before := time.Now()
	if _, err := o.ReadMetaCustomAudiences(context.Background(), "cncf", model.ProviderMetaAds); err != nil {
		t.Fatalf("ReadMetaCustomAudiences: %v", err)
	}
	if lo, hi := before.Add(metaCustomAudiencesCallTimeout), time.Now().Add(metaCustomAudiencesCallTimeout); d.deadline.Before(lo) || d.deadline.After(hi) {
		t.Fatalf("deadline %v not within [%v, %v]", d.deadline, lo, hi)
	}
	if metaCustomAudiencesCallTimeout <= accountsCallTimeout {
		t.Fatalf("metaCustomAudiencesCallTimeout %v must exceed accountsCallTimeout %v", metaCustomAudiencesCallTimeout, accountsCallTimeout)
	}
}

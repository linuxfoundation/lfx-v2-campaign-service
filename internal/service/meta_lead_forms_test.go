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

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

type mockMetaLeadFormDispatcher struct {
	deadline time.Time
	res      *model.MetaLeadForms
	err      error
	calls    int
}

func (m *mockMetaLeadFormDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockMetaLeadFormDispatcher) ListMetaLeadForms(ctx context.Context, _ string, _ model.Provider) (*model.MetaLeadForms, bool, error) {
	m.calls++
	m.deadline, _ = ctx.Deadline()
	return m.res, true, m.err
}

// Forms are sorted by name ignoring case, then id; every field is mapped; the call is bounded.
func TestListMetaLeadForms_SortsAndMaps(t *testing.T) {
	t.Setenv(constants.EnvMetaLeadFormsEnabled, "true")
	d := &mockMetaLeadFormDispatcher{res: &model.MetaLeadForms{PageID: "987", Forms: []model.MetaLeadForm{
		{ID: "3", Name: "beta", Status: "ACTIVE"}, {ID: "2", Name: "Alpha", Status: "DRAFT"}, {ID: "1", Name: "alpha", Status: "ARCHIVED"},
	}}}
	start := time.Now()
	res, err := metaLookupService(d).ListMetaLeadForms(context.Background(), &conn.ListMetaLeadFormsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListMetaLeadForms: %v", err)
	}
	b, _ := json.Marshal(res)
	const want = `{"PageID":"987","Forms":[{"ID":"1","Name":"alpha","Status":"ARCHIVED"},{"ID":"2","Name":"Alpha","Status":"DRAFT"},{"ID":"3","Name":"beta","Status":"ACTIVE"}],"FormCount":3}`
	if string(b) != want {
		t.Errorf("res = %s\nwant  %s", b, want)
	}
	if d.deadline.IsZero() || d.deadline.Sub(start) > metaLeadFormsCallTimeout+time.Second {
		t.Errorf("deadline = %v, want about %v from now", d.deadline, metaLeadFormsCallTimeout)
	}
}

// Off, and the reserved scope, are refused before the dispatcher.
func TestListMetaLeadForms_RefusesBeforeTheDispatcher(t *testing.T) {
	t.Setenv(constants.EnvMetaLeadFormsEnabled, "TRUE")
	d := &mockMetaLeadFormDispatcher{res: &model.MetaLeadForms{Forms: []model.MetaLeadForm{}}}
	_, err := metaLookupService(d).ListMetaLeadForms(context.Background(), &conn.ListMetaLeadFormsPayload{ProjectID: "cncf"})
	var bad *conn.BadRequestError
	if !errors.As(err, &bad) || !strings.Contains(bad.Message, "META_LEAD_FORMS_ENABLED is not \"true\"") || d.calls != 0 {
		t.Fatalf("flag off: err %v calls %d, want a 400 naming the flag", err, d.calls)
	}
	t.Setenv(constants.EnvMetaLeadFormsEnabled, "true")
	if _, err := metaLookupService(d).ListMetaLeadForms(context.Background(), &conn.ListMetaLeadFormsPayload{ProjectID: model.SystemProjectID}); err == nil || d.calls != 0 {
		t.Fatalf("reserved scope: err %v calls %d, want a refusal", err, d.calls)
	}
}

// The dispatcher's errors map to the shared lookup classes, and a nil result is not an answer.
func TestListMetaLeadForms_ClassifiesErrors(t *testing.T) {
	t.Setenv(constants.EnvMetaLeadFormsEnabled, "true")
	for _, tc := range []struct {
		name string
		d    *mockMetaLeadFormDispatcher
		want any
	}{
		{"no connection", &mockMetaLeadFormDispatcher{err: domain.ErrNotFound}, &conn.NotFoundError{}},
		{"no page", &mockMetaLeadFormDispatcher{err: fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid)}, &conn.BadRequestError{}},
		{"upstream", &mockMetaLeadFormDispatcher{err: errors.New("list meta lead forms: HTTP 403")}, &conn.ConnServiceUnavailableError{}},
		{"nil result", &mockMetaLeadFormDispatcher{}, &conn.ConnServiceUnavailableError{}},
		{"nil forms", &mockMetaLeadFormDispatcher{res: &model.MetaLeadForms{PageID: "987"}}, &conn.ConnServiceUnavailableError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := metaLookupService(tc.d).ListMetaLeadForms(context.Background(), &conn.ListMetaLeadFormsPayload{ProjectID: "cncf"})
			if fmt.Sprintf("%T", err) != fmt.Sprintf("%T", tc.want) {
				t.Fatalf("err = %T %v, want %T", err, err, tc.want)
			}
		})
	}
}

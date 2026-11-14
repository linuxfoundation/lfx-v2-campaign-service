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

type mockMetaSearchDispatcher struct {
	deadline time.Time
	rows     []model.MetaTargetingOption
	err      error
	got      model.MetaTargetingQuery
	calls    int
}

func (m *mockMetaSearchDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockMetaSearchDispatcher) SearchMetaTargeting(ctx context.Context, _ string, _ model.Provider, q model.MetaTargetingQuery) ([]model.MetaTargetingOption, bool, error) {
	m.calls++
	m.got = q
	m.deadline, _ = ctx.Deadline()
	return m.rows, true, m.err
}

// Every field is mapped; the query reaches the dispatcher trimmed; the call is bounded.
func TestSearchMetaTargetingOptions_MapsEveryField(t *testing.T) {
	t.Setenv(constants.EnvMetaTargetingSearchEnabled, "true")
	d := &mockMetaSearchDispatcher{rows: []model.MetaTargetingOption{
		{ID: "6003", Name: "Kubernetes", Path: []string{"Interests", "Tech"}, Description: "Container orchestration", AudienceLower: i64p(10), AudienceUpper: i64p(20)},
		{ID: "1057", Name: "Engineer", Coverage: i64p(56000)},
	}}
	start := time.Now()
	res, err := metaLookupService(d).SearchMetaTargetingOptions(context.Background(), &conn.SearchMetaTargetingOptionsPayload{ProjectID: "cncf", Type: "interest", Query: "  kube "})
	if err != nil {
		t.Fatalf("SearchMetaTargetingOptions: %v", err)
	}
	if d.got != (model.MetaTargetingQuery{Kind: "interest", Query: "kube"}) {
		t.Errorf("dispatcher query = %+v", d.got)
	}
	if d.deadline.IsZero() || d.deadline.Sub(start) > metaTargetingSearchCallTimeout+time.Second {
		t.Errorf("deadline = %v, want about %v from now", d.deadline, metaTargetingSearchCallTimeout)
	}
	b, _ := json.Marshal(res)
	const want = `{"Type":"interest","Options":[{"ID":"6003","Name":"Kubernetes","Path":["Interests","Tech"],"Description":"Container orchestration","AudienceSizeLowerBound":10,"AudienceSizeUpperBound":20,"Coverage":null},` +
		`{"ID":"1057","Name":"Engineer","Path":null,"Description":null,"AudienceSizeLowerBound":null,"AudienceSizeUpperBound":null,"Coverage":56000}],"OptionCount":2}`
	if string(b) != want {
		t.Errorf("res = %s\nwant  %s", b, want)
	}
}

// Off, a bad type or query, and the reserved scope are refused before the dispatcher is called.
func TestSearchMetaTargetingOptions_RefusesBeforeTheDispatcher(t *testing.T) {
	for _, tc := range []struct {
		name, flag, project, kind, query, want string
	}{
		{"flag off", "yes", "cncf", "interest", "kube", "META_TARGETING_SEARCH_ENABLED is not \"true\""},
		{"unknown type", "true", "cncf", "interests", "kube", "type must be one of interest, behavior, work_position"},
		{"short query", "true", "cncf", "interest", " k ", "query must be 2-100 characters"},
		{"control character", "true", "cncf", "interest", "ku\tbe", "query must be"},
		{"long query", "true", "cncf", "interest", strings.Repeat("x", 101), "query must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvMetaTargetingSearchEnabled, tc.flag)
			d := &mockMetaSearchDispatcher{rows: []model.MetaTargetingOption{}}
			_, err := metaLookupService(d).SearchMetaTargetingOptions(context.Background(), &conn.SearchMetaTargetingOptionsPayload{ProjectID: tc.project, Type: tc.kind, Query: tc.query})
			var bad *conn.BadRequestError
			if !errors.As(err, &bad) || !strings.Contains(bad.Message, tc.want) || d.calls != 0 {
				t.Fatalf("err = %v (calls %d), want a 400 containing %q and no dispatch", err, d.calls, tc.want)
			}
		})
	}
	t.Setenv(constants.EnvMetaTargetingSearchEnabled, "true")
	d := &mockMetaSearchDispatcher{rows: []model.MetaTargetingOption{}}
	if _, err := metaLookupService(d).SearchMetaTargetingOptions(context.Background(), &conn.SearchMetaTargetingOptionsPayload{ProjectID: model.SystemProjectID, Type: "interest", Query: "kube"}); err == nil || d.calls != 0 {
		t.Errorf("reserved scope: err %v calls %d, want a refusal", err, d.calls)
	}
}

// The dispatcher's errors map to the shared lookup classes, and none echoes the query.
func TestSearchMetaTargetingOptions_ClassifiesErrors(t *testing.T) {
	t.Setenv(constants.EnvMetaTargetingSearchEnabled, "true")
	for _, tc := range []struct {
		name string
		err  error
		want any
	}{
		{"invalid", fmt.Errorf("%w: x", domain.ErrMetaTargetingSearchInvalid), &conn.BadRequestError{}},
		{"no connection", domain.ErrNotFound, &conn.NotFoundError{}},
		{"upstream", errors.New("meta targeting search (interest): Meta is rate-limiting the search; it was not retried"), &conn.ConnServiceUnavailableError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := metaLookupService(&mockMetaSearchDispatcher{err: tc.err}).SearchMetaTargetingOptions(context.Background(), &conn.SearchMetaTargetingOptionsPayload{ProjectID: "cncf", Type: "interest", Query: "zebracorn"})
			if err == nil || fmt.Sprintf("%T", err) != fmt.Sprintf("%T", tc.want) || strings.Contains(err.Error(), "zebracorn") {
				t.Fatalf("err = %T %v, want %T without the query", err, err, tc.want)
			}
		})
	}
}

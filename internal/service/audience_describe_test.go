// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	explore "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

type describeExplorer struct {
	AudienceExplorer
	out    []audience.ListDescription
	err    error
	gotIDs []string
}

func (d *describeExplorer) DescribeLists(_ context.Context, _ string, ids []string) ([]audience.ListDescription, error) {
	d.gotIDs = ids
	return d.out, d.err
}

func TestDescribeAudienceLists_MapsEveryField(t *testing.T) {
	x := &describeExplorer{out: []audience.ListDescription{
		{ListID: "1", Name: "Master", Size: 4200, ProcessingType: "DYNAMIC", HubSpotURL: "https://h/1", Status: audience.DescribeStatusDescribed,
			Summary: "s", Match: audience.DescribeMatchAny, Groups: []audience.RuleGroup{{Conditions: []string{"a", "b"}}}, Complete: true},
		{ListID: "2", Size: -1, Status: audience.DescribeStatusNotFound, Summary: "gone", Match: audience.DescribeMatchAny, Groups: []audience.RuleGroup{}},
	}}
	svc := NewAudienceExploreService(x)
	res, err := svc.DescribeAudienceLists(context.Background(), &explore.DescribeAudienceListsPayload{ProjectID: "p", ListIds: []string{"1", "2"}})
	if err != nil {
		t.Fatalf("DescribeAudienceLists: %v", err)
	}
	if len(x.gotIDs) != 2 || len(res.Lists) != 2 {
		t.Fatalf("ids=%v lists=%d", x.gotIDs, len(res.Lists))
	}
	first := res.Lists[0]
	if first.Size == nil || *first.Size != 4200 || first.ProcessingType == nil || *first.ProcessingType != "DYNAMIC" {
		t.Errorf("size/processing type not carried: %+v", first)
	}
	if len(first.Groups) != 1 || len(first.Groups[0].Conditions) != 2 || !first.Complete {
		t.Errorf("groups not carried: %+v", first.Groups)
	}
	// An UNREPORTED size is absent, never 0: a list HubSpot did not size is not an empty list.
	if second := res.Lists[1]; second.Size != nil || second.ProcessingType != nil || second.Status != audience.DescribeStatusNotFound {
		t.Errorf("unread list mapped wrongly: %+v", second)
	}
}

func TestDescribeAudienceLists_MapsAConnectionFailure(t *testing.T) {
	svc := NewAudienceExploreService(&describeExplorer{err: domain.ErrConnectionNotUsable})
	_, err := svc.DescribeAudienceLists(context.Background(), &explore.DescribeAudienceListsPayload{ProjectID: "p", ListIds: []string{"1"}})
	var unavailable *explore.ConnServiceUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("err = %T %v, want the contract's 503", err, err)
	}
}

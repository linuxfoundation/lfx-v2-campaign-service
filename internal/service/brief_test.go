// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
)

// A BriefService built with nil repos (DATABASE_URL unset) must return the typed
// 503 ServiceUnavailable for every route rather than panicking on a nil repo, so
// runtime matches the published OpenAPI contract (mirrors the connection service).
func TestBriefService_NilRepo_ReturnsServiceUnavailable(t *testing.T) {
	s := NewBriefService(nil, nil, nil, nil)
	ctx := context.Background()

	if _, err := s.GetBrief(ctx, &briefs.GetBriefPayload{ProjectID: "cncf", BriefID: "b1"}); !isBriefUnavailable(err) {
		t.Errorf("GetBrief: expected *briefs.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
	if _, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{ProjectID: "cncf", Brief: &briefs.BriefInput{}}); !isBriefUnavailable(err) {
		t.Errorf("CreateBrief: expected *briefs.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
	if _, err := s.GetJob(ctx, &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j1"}); !isBriefUnavailable(err) {
		t.Errorf("GetJob: expected *briefs.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
	if err := s.DeleteBrief(ctx, &briefs.DeleteBriefPayload{ProjectID: "cncf", BriefID: "b1"}); !isBriefUnavailable(err) {
		t.Errorf("DeleteBrief: expected *briefs.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
}

// A missing bearer token is a client-side problem and must map to 400, not 500
// (a 500 misrepresents it as a server fault and can trigger ops alerting).
func TestBriefService_JWTAuth_EmptyTokenIsBadRequest(t *testing.T) {
	s := NewBriefService(nil, nil, nil, nil)
	_, err := s.JWTAuth(context.Background(), "", nil)
	if _, ok := err.(*briefs.BadRequestError); !ok {
		t.Fatalf("expected *briefs.BadRequestError for empty token, got %T (%v)", err, err)
	}
}

func isBriefUnavailable(err error) bool {
	_, ok := err.(*briefs.ConnServiceUnavailableError)
	return ok
}

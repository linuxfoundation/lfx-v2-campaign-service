// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	exploresvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	audiencesvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// TestErrorEncoder_ConcurrentNonNamedErrors_NoRace drives many concurrent NON-named errors
// through ONE endpoint of the REAL mux. Each generated handler builds its error encoder once
// (Encode<Method>Error -> goahttp.ErrorEncoder(encoder, formatter)) and every server is built
// with a nil formatter (see buildMux). Before goa v3.29, goahttp.ErrorEncoder assigned its
// default formatter to the captured variable INSIDE the returned closure, on every non-named
// error, so concurrent error responses on one endpoint wrote it unsynchronized. Its value is
// the same every time, so only the race detector sees it: run under -race, this test fails on
// goa v3.25.3 ("WARNING: DATA RACE" in goahttp.ErrorEncoder.func1) and passes on v3.29+.
//
// The error is the decoder's UUID-format rejection of brief_id, which reaches the encoder as
// a plain *goa.ServiceError (not a design-declared error), i.e. exactly the default-formatter
// path. Without -race the test still pins that each such response is a well-formed 400.
func TestErrorEncoder_ConcurrentNonNamedErrors_NoRace(t *testing.T) {
	mux, err := buildMux(context.Background(), &config.Config{},
		svc.NewEndpoints(service.NewCampaignService(nil)),
		connsvc.NewEndpoints(service.NewConnectionService(nil, nil)),
		briefsvc.NewEndpoints(service.NewBriefService(nil, nil, nil, nil)),
		audiencesvc.NewEndpoints(service.NewAudienceService(nil)),
		exploresvc.NewEndpoints(service.NewAudienceExploreService(nil)),
		nil, nil)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}

	const workers = 16
	const perWorker = 8
	start := make(chan struct{})
	codes := make(chan int, workers*perWorker)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			for range perWorker {
				req := httptest.NewRequest(http.MethodGet, "/projects/cncf/briefs/not-a-uuid", nil)
				req.Header.Set("Authorization", "Bearer test-token")
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				codes <- rec.Code
			}
		})
	}
	close(start)
	wg.Wait()
	close(codes)

	n := 0
	for code := range codes {
		n++
		if code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 from the generated decoder's format rejection", code)
		}
	}
	if n != workers*perWorker {
		t.Fatalf("got %d responses, want %d", n, workers*perWorker)
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	exploresvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	audiencesvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"

	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"
)

// TestCampaignRefDecoderRejection_DoesNotEchoTheID drives a malformed platform_campaign_id
// through the REAL mux — the generated decoder runs the design's Pattern/MaxLength before the
// service method, so platformCampaignIDRule's fixed message is never reached for these — and
// asserts the 400 body names the field without carrying any part of the rejected value. Goa's
// own validation errors format the target into their message ("... but got value %q"), so this
// fails whenever buildMux mounts a server without the non-echoing formatter.
func TestCampaignRefDecoderRejection_DoesNotEchoTheID(t *testing.T) {
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

	const marker = "MARKER9"
	ids := map[string]string{
		"bad charset":       "zz<script>" + marker,
		"too long":          marker + strings.Repeat("7", 200),
		"leading space":     " " + marker,
		"non-ascii letter":  marker + "\u00e9",
		"punctuation only":  marker + "-/.",
		"quote and newline": marker + "\"\n",
	}
	for _, slug := range []string{"google-ads", "microsoft-ads", "meta-ads", "reddit-ads", "twitter-ads"} {
		for name, id := range ids {
			t.Run(slug+"/"+name, func(t *testing.T) {
				q := url.Values{"platform_campaign_id": {id}}
				req := httptest.NewRequest(http.MethodGet, "/projects/cncf/"+slug+"/campaign-ref?"+q.Encode(), nil)
				req.Header.Set("Authorization", "Bearer test-token")
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 from the generated decoder; body %s", rec.Code, rec.Body.String())
				}
				body := rec.Body.String()
				if strings.Contains(body, marker) {
					t.Fatalf("400 body echoes the rejected platform_campaign_id: %s", body)
				}
				var resp struct {
					Name    string `json:"name"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("400 body is not the JSON error shape: %v; body %s", err, body)
				}
				if !strings.Contains(resp.Message, "platform_campaign_id") {
					t.Errorf("400 message %q does not name the rejected field", resp.Message)
				}
			})
		}
	}
}

// TestNonEchoingErrorFormatter pins the formatter's own rules: every value-echoing validation
// part of a merged error is rewritten, a part that does not echo keeps Goa's message, and an
// error that is not a validation error passes through exactly as goahttp.NewErrorResponse
// renders it.
func TestNonEchoingErrorFormatter(t *testing.T) {
	const marker = "MARKER9"
	merged := goa.MergeErrors(
		goa.InvalidPatternError("platform_campaign_id", marker, "^[0-9]+$"),
		goa.MergeErrors(
			goa.MissingFieldError("project_id", "path"),
			goa.InvalidLengthError("platform_campaign_id", marker, 7, 5, false)))
	cases := []struct {
		name       string
		err        error
		wantName   string
		wantStatus int
		wantMsg    string
	}{
		{"pattern", goa.InvalidPatternError("platform_campaign_id", marker, "^[0-9]+$"), goa.InvalidPattern, http.StatusBadRequest,
			"platform_campaign_id does not match the pattern this field requires"},
		{"enum", goa.InvalidEnumValueError("status", marker, []any{"a", "b"}), goa.InvalidEnumValue, http.StatusBadRequest,
			"status is not one of the values this field allows"},
		{"field type", goa.InvalidFieldTypeError("limit", marker, "integer"), goa.InvalidFieldType, http.StatusBadRequest,
			"limit is not of the type this field requires"},
		{"range", goa.InvalidRangeError("limit", 9000, 100, false), goa.InvalidRange, http.StatusBadRequest,
			"limit is outside the range this field allows"},
		{"format", goa.InvalidFormatError("starts_at", marker, goa.FormatDateTime, errors.New(marker)), goa.InvalidFormat, http.StatusBadRequest,
			"starts_at is not in the format this field requires"},
		{"merged", merged, goa.InvalidPattern, http.StatusBadRequest,
			`platform_campaign_id does not match the pattern this field requires; "project_id" is missing from path; platform_campaign_id is outside the length this field allows`},
		// The order the generated decoders merge in: the first failure is the accumulator, so its
		// own history entry carries every later part's message, echoing ones included.
		{"accumulator first", goa.MergeErrors(goa.MissingFieldError("project_id", "path"),
			goa.InvalidPatternError("platform_campaign_id", marker, "^[0-9]+$")), goa.MissingField, http.StatusBadRequest,
			`"project_id" is missing from path; platform_campaign_id does not match the pattern this field requires`},
		{"no field", goa.PermanentError(goa.InvalidPattern, "%s", marker), goa.InvalidPattern, http.StatusBadRequest,
			"a field does not match the pattern this field requires"},
		{"missing field untouched", goa.MissingFieldError("project_id", "path"), goa.MissingField, http.StatusBadRequest,
			`"project_id" is missing from path`},
		{"plain error untouched", errors.New("boom"), "fault", http.StatusInternalServerError, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, ok := nonEchoingErrorFormatter(context.Background(), tc.err).(*goahttp.ErrorResponse)
			if !ok {
				t.Fatalf("formatter returned %T, want *goahttp.ErrorResponse", resp)
			}
			if resp.Name != tc.wantName || resp.StatusCode() != tc.wantStatus || resp.Message != tc.wantMsg {
				t.Errorf("got name=%q status=%d message=%q; want name=%q status=%d message=%q",
					resp.Name, resp.StatusCode(), resp.Message, tc.wantName, tc.wantStatus, tc.wantMsg)
			}
			if tc.name != "plain error untouched" && strings.Contains(resp.Message, marker) {
				t.Errorf("message echoes the rejected value: %q", resp.Message)
			}
		})
	}
}

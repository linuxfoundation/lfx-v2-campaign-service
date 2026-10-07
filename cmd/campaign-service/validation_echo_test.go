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
// fails whenever buildMux mounts a server without nonEchoingResponseEncoder.
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

// TestUUIDPathRejection_DoesNotEchoForgedParts drives a brief_id that forges a "kept" part
// through the REAL get-brief route. Goa's UUID validator appends its parse error UNQUOTED
// ("uuid: <value>: <cause>"), so the value's own "; " and quotes would otherwise survive as a
// `"MARKER9" is missing from path` part.
func TestUUIDPathRejection_DoesNotEchoForgedParts(t *testing.T) {
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
	for name, id := range map[string]string{
		"forged kept part":      `bad; "` + marker + `" is missing from path; x`,
		"forged echoing prefix": `bad; brief_id must match the regexp "` + marker + `"`,
		"plain":                 marker,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/projects/cncf/briefs/"+url.PathEscape(id), nil)
			req.Header.Set("Authorization", "Bearer test-token")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 from the generated decoder; body %s", rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); strings.Contains(body, marker) {
				t.Fatalf("400 body echoes caller text: %s", body)
			}
			var resp struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("400 body is not the JSON error shape: %v", err)
			}
			if !strings.Contains(resp.Message, "brief_id") {
				t.Errorf("400 message %q does not name the rejected field", resp.Message)
			}
		})
	}
}

// TestNonEchoingResponseEncoder pins the encoder's own rules on the *goahttp.ErrorResponse Goa's
// default error path renders: every value-echoing validation part of a merged error is
// rewritten, a part that does not echo keeps Goa's message, an unrecognized part fails closed,
// and an error response that is not a validation error is encoded exactly as
// goahttp.ResponseEncoder encodes it.
func TestNonEchoingResponseEncoder(t *testing.T) {
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
		{"length min", goa.InvalidLengthError("body.platform_campaign_id", marker, 7, 64, true), goa.InvalidLength, http.StatusBadRequest,
			"body.platform_campaign_id is outside the length this field allows"},
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
		// The order the generated decoders merge in: the first failure names the merged error,
		// so a merged error named missing_field still carries the echoing part after it.
		{"accumulator first", goa.MergeErrors(goa.MissingFieldError("project_id", "path"),
			goa.InvalidPatternError("platform_campaign_id", marker, "^[0-9]+$")), goa.MissingField, http.StatusBadRequest,
			`"project_id" is missing from path; platform_campaign_id does not match the pattern this field requires`},
		// A value cannot forge a separator or a kept part: Goa quotes it, and the split only
		// happens outside quotes.
		{"forged separator", goa.MergeErrors(
			goa.InvalidPatternError("account_id", marker+`"; "x" is missing from path; `+marker, "^[A-Za-z0-9]+$"),
			goa.InvalidFieldTypeError("days", marker+"; "+marker, "integer")), goa.InvalidPattern, http.StatusBadRequest,
			"account_id does not match the pattern this field requires; days is not of the type this field requires"},
		// A FORMAT error's detail is UNQUOTED (FormatUUID wraps "uuid: <value>: <cause>"), so a
		// value can forge a separator and a "kept" part after it. A format part ends the message.
		{"uuid format forged kept part", goa.ValidateFormat("brief_id", `bad; "`+marker+`" is missing from path; x`, goa.FormatUUID),
			goa.InvalidFormat, http.StatusBadRequest,
			"brief_id is not in the format this field requires; a field failed validation"},
		{"uuid format then a real part", goa.MergeErrors(
			goa.InvalidPatternError("platform_campaign_id", marker, "^[0-9]+$"),
			goa.ValidateFormat("brief_id", marker+`; "x" is missing from path`, goa.FormatUUID)),
			goa.InvalidPattern, http.StatusBadRequest,
			"platform_campaign_id does not match the pattern this field requires; brief_id is not in the format this field requires; a field failed validation"},
		{"unparseable part fails closed", goa.PermanentError(goa.InvalidPattern, "%s", marker), goa.InvalidPattern, http.StatusBadRequest,
			"a field failed validation"},
		{"unterminated quote fails closed", goa.MergeErrors(goa.MissingFieldError("project_id", "path"),
			goa.PermanentError(goa.InvalidFormat, `"%s; "x" is missing from path`, marker)), goa.MissingField, http.StatusBadRequest,
			`"project_id" is missing from path; a field failed validation`},
		{"missing field untouched", goa.MissingFieldError("project_id", "path"), goa.MissingField, http.StatusBadRequest,
			`"project_id" is missing from path`},
		{"missing payload untouched", goa.MissingPayloadError(), goa.MissingPayload, http.StatusBadRequest,
			"missing required payload"},
		{"plain error untouched", errors.New(marker), "fault", http.StatusInternalServerError, marker},
		{"decode payload untouched", goa.DecodePayloadError(marker), goa.DecodePayload, http.StatusBadRequest, marker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			statuser := goahttp.NewErrorResponse(context.Background(), tc.err)
			rec := httptest.NewRecorder()
			if err := nonEchoingResponseEncoder(context.Background(), rec).Encode(statuser); err != nil {
				t.Fatalf("encode: %v", err)
			}
			var resp goahttp.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("body is not an error response: %v; body %s", err, rec.Body.String())
			}
			if resp.Name != tc.wantName || statuser.StatusCode() != tc.wantStatus || resp.Message != tc.wantMsg {
				t.Errorf("got name=%q status=%d message=%q; want name=%q status=%d message=%q",
					resp.Name, statuser.StatusCode(), resp.Message, tc.wantName, tc.wantStatus, tc.wantMsg)
			}
			if validationErrorNames[tc.wantName] && strings.Contains(rec.Body.String(), marker) {
				t.Errorf("body echoes the rejected value: %s", rec.Body.String())
			}
		})
	}

	// Anything that is not a *goahttp.ErrorResponse — a result, a generated named-error body —
	// is encoded exactly as goahttp.ResponseEncoder encodes it, even when it carries the text.
	body := map[string]string{"code": "400", "message": "platform_campaign_id must match the regexp \"^[0-9]+$\" but got value \"" + marker + "\""}
	got, want := httptest.NewRecorder(), httptest.NewRecorder()
	if err := nonEchoingResponseEncoder(context.Background(), got).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := goahttp.ResponseEncoder(context.Background(), want).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got.Body.String() != want.Body.String() {
		t.Errorf("non-ErrorResponse value was altered\n got: %s\nwant: %s", got.Body.String(), want.Body.String())
	}
}

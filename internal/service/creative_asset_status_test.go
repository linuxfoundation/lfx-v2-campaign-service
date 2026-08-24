// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	goahttp "goa.design/goa/v3/http"

	briefssrv "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
)

// TestUploadCreativeAsset_StatusDistinguishesCreationFromRetry asserts the STATUS CODE a client
// actually receives, by driving the GENERATED response encoder rather than inspecting the service
// result.
//
// This is the assertion that matters for the finding. The service-level test pins the `created`
// attribute, but a client never sees an attribute — it sees a status line, and the mapping from
// one to the other lives in generated code that no service-level test exercises. Asserting the
// attribute alone would leave the design's Tag wiring unproven: a Tag on the wrong value, or a
// Response ordering that makes 200 unreachable, would pass every service test and still ship an
// endpoint that answers 201 for a retry.
//
// The body is asserted too, because the status is only half the contract: both arms must carry the
// same CreativeAsset shape, or splitting the status would have silently changed the payload.
func TestUploadCreativeAsset_StatusDistinguishesCreationFromRetry(t *testing.T) {
	encode := briefssrv.EncodeUploadCreativeAssetResponse(goahttp.ResponseEncoder)

	for _, tc := range []struct {
		name    string
		created string
		want    int
		why     string
	}{
		{
			name: "first upload", created: "true", want: http.StatusCreated,
			why: "this request stored the asset, so 201 Created is accurate",
		},
		{
			name: "idempotent re-upload", created: "false", want: http.StatusOK,
			why: "the existing asset was returned and nothing was created, so 201 would be a lie",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := tc.created
			res := &briefs.CreativeAsset{
				ID: "a1", ProjectID: "cncf", BriefID: "b1",
				MimeType: "image/png", ByteSize: 42, Checksum: "abc",
				Created: &created,
			}

			rec := httptest.NewRecorder()
			if err := encode(context.Background(), rec, res); err != nil {
				t.Fatalf("encode response: %v", err)
			}

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d — %s", rec.Code, tc.want, tc.why)
			}

			// The body must be the same CreativeAsset shape on BOTH arms.
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			for _, k := range []string{"id", "project_id", "brief_id", "mime_type", "byte_size", "checksum", "created"} {
				if _, ok := body[k]; !ok {
					t.Errorf("body is missing %q; the two status arms must carry the same asset shape", k)
				}
			}
			if body["id"] != "a1" {
				t.Errorf("body id = %v, want a1", body["id"])
			}

			// The DISCRIMINATOR itself, asserted by VALUE rather than by presence.
			//
			// `created` is what selects 201 from 200, so a body that carries the wrong one
			// contradicts its own status line: a client reading the attribute (rather than the
			// status) would conclude the opposite of what happened. Checking only that the key
			// EXISTS leaves that inversion undetected, and checking only the status leaves the
			// attribute free to drift from it — the two are one contract and are asserted
			// together here.
			if body["created"] != tc.created {
				t.Errorf("body created = %v, want %q — the attribute must agree with the %d status it selected",
					body["created"], tc.created, tc.want)
			}
		})
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	explore "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
	"github.com/stretchr/testify/assert"
)

// TestAudienceExploreErr_ArmOrdering pins the switch's documented precedence
// rather than each arm in isolation: audienceExploreErr's own comments argue for
// a specific order (ErrNotFound/ErrConnectionNotUsable sit BELOW the
// system-connection and decryption arms), and a case reordering would compile,
// pass a per-error test, and still silently misreport a request that wraps more
// than one of these sentinels -- which creds.go does by design (a project-less
// fallback to the shared LF row wraps ErrSystemConnectionMissing alongside
// domain.ErrNotFound).
func TestAudienceExploreErr_ArmOrdering(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus string
		wantType   string
	}{
		{
			name:       "list not found wins even wrapped",
			err:        fmt.Errorf("wrap: %w", audience.ErrListNotFound),
			wantStatus: "404",
			wantType:   "*lfxv2campaignserviceaudiencebuilder.NotFoundError",
		},
		{
			name:       "invalid request maps to 400",
			err:        audience.ErrInvalidRequest,
			wantStatus: "400",
			wantType:   "*lfxv2campaignserviceaudiencebuilder.BadRequestError",
		},
		{
			name:       "event name unresolved maps to the same 400 as invalid request",
			err:        audience.ErrEventNameUnresolved,
			wantStatus: "400",
			wantType:   "*lfxv2campaignserviceaudiencebuilder.BadRequestError",
		},
		{
			name:       "event URL invalid maps to 400",
			err:        eventurl.ErrEventURLInvalid,
			wantStatus: "400",
			wantType:   "*lfxv2campaignserviceaudiencebuilder.BadRequestError",
		},
		{
			name:       "event page unavailable is a distinct 503, not the connection one",
			err:        audience.ErrEventPageUnavailable,
			wantStatus: "503",
			wantType:   "*lfxv2campaignserviceaudiencebuilder.ConnServiceUnavailableError",
		},
		{
			name: "system connection missing wins over a co-wrapped domain.ErrNotFound",
			err: fmt.Errorf("creds: %w / %w",
				domain.ErrSystemConnectionMissing, domain.ErrNotFound),
			wantStatus: "503",
			wantType:   "*lfxv2campaignserviceaudiencebuilder.ConnServiceUnavailableError",
		},
		{
			name:       "credential decryption failure is a 500, not the connection 503",
			err:        domain.ErrCredentialDecryptionFailed,
			wantStatus: "500",
			wantType:   "*lfxv2campaignserviceaudiencebuilder.InternalServerError",
		},
		{
			name:       "plain domain.ErrNotFound falls to the connection 503",
			err:        domain.ErrNotFound,
			wantStatus: "503",
			wantType:   "*lfxv2campaignserviceaudiencebuilder.ConnServiceUnavailableError",
		},
		{
			name:       "connection not usable falls to the same 503",
			err:        domain.ErrConnectionNotUsable,
			wantStatus: "503",
			wantType:   "*lfxv2campaignserviceaudiencebuilder.ConnServiceUnavailableError",
		},
		{
			name:       "an unrecognized error falls through to the generic 500",
			err:        errors.New("hubspot: boom"),
			wantStatus: "500",
			wantType:   "*lfxv2campaignserviceaudiencebuilder.InternalServerError",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := audienceExploreErr(context.Background(), "op", "proj-1", tc.err)
			assert.Equal(t, tc.wantType, fmt.Sprintf("%T", got))

			var code string
			switch e := got.(type) {
			case *explore.NotFoundError:
				code = e.Code
			case *explore.BadRequestError:
				code = e.Code
			case *explore.ConnServiceUnavailableError:
				code = e.Code
			case *explore.InternalServerError:
				code = e.Code
			default:
				t.Fatalf("unhandled result type %T", got)
			}
			assert.Equal(t, tc.wantStatus, code)
		})
	}
}

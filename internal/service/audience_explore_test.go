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
	"github.com/stretchr/testify/require"
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

// TestComposeErr_DistinguishesConfirmedFromUnconfirmedMaster pins that composeErr's
// message tells the operator which half of the partial state is CONFIRMED (the
// suppression list, reported by CheckSuppression's own create) versus merely
// UNCONFIRMED (the master, which may or may not exist in HubSpot). Collapsing these
// into one message would tell an operator "the suppression list was created but the
// master was not" in a case where the master might actually exist too -- sending them
// to compose a duplicate under a different name instead of searching for the
// deterministic one first.
func TestComposeErr_DistinguishesConfirmedFromUnconfirmedMaster(t *testing.T) {
	cause := errors.New("hubspot: 503 upstream timeout")

	cases := []struct {
		name              string
		partial           *audience.ComposePartialError
		wantMasterNameSet bool
		wantSuppression   bool
		wantSubstrings    []string
	}{
		{
			name: "definite master failure reports only the suppression orphan",
			partial: &audience.ComposePartialError{
				Suppression: audience.ComposedList{ListRow: audience.ListRow{ListID: "555", Name: "Combined Suppression"}},
				Err:         cause,
			},
			wantMasterNameSet: false,
			wantSuppression:   true,
			wantSubstrings:    []string{"combined suppression list was created but the master list was not"},
		},
		{
			name: "unconfirmed master with no suppression names only the master and omits the suppression object",
			partial: &audience.ComposePartialError{
				MasterName: "KubeCon NA 2026 — master",
				Err:        cause,
			},
			wantMasterNameSet: true,
			wantSuppression:   false,
			wantSubstrings:    []string{"master list creation is unconfirmed", "search HubSpot for it by name"},
		},
		{
			name: "unconfirmed master alongside a confirmed suppression names both",
			partial: &audience.ComposePartialError{
				Suppression: audience.ComposedList{ListRow: audience.ListRow{ListID: "555", Name: "Combined Suppression"}},
				MasterName:  "KubeCon NA 2026 — master",
				Err:         cause,
			},
			wantMasterNameSet: true,
			wantSuppression:   true,
			wantSubstrings:    []string{"suppression list was created and the master list creation is unconfirmed", "verify both in HubSpot"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composeErr(context.Background(), "proj-1", tc.partial)

			var partialErr *explore.AudienceComposePartialError
			require.ErrorAs(t, got, &partialErr)

			for _, sub := range tc.wantSubstrings {
				assert.Contains(t, partialErr.Message, sub)
			}
			if tc.wantMasterNameSet {
				require.NotNil(t, partialErr.MasterName)
				assert.Equal(t, tc.partial.MasterName, *partialErr.MasterName)
			} else {
				assert.Nil(t, partialErr.MasterName)
			}
			if tc.wantSuppression {
				require.NotNil(t, partialErr.Suppression)
				assert.Equal(t, tc.partial.Suppression.ListID, partialErr.Suppression.ListID)
			} else {
				assert.Nil(t, partialErr.Suppression)
			}
		})
	}
}

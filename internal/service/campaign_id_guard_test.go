// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
)

// TestGoogleAdsCampaignIDRejectsNonCanonicalIDs pins the shapes that a digits-only length check
// lets through.
//
// These are not merely untidy inputs. platform_campaign_id is a TEXT column compared as a string,
// so "007" is a different row from "7": a leading-zero spelling of a real campaign matches nothing
// and the caller is told 200 "not your campaign" for a campaign that IS theirs. The 19-digit
// values above math.MaxInt64 are unrepresentable rather than absent. Both reach SQL and come back
// as a confident answer to a question the caller did not ask, which is worse than the declared 400.
func TestGoogleAdsCampaignIDRejectsNonCanonicalIDs(t *testing.T) {
	rejected := []struct {
		name string
		id   string
	}{
		{"zero is not a campaign id", "0"},
		{"padded zeros", "0000"},
		{"leading zero on a real id", "024183781329"},
		{"single leading zero", "07"},
		{"19 digits above MaxInt64", "9999999999999999999"},
		{"19 digits just above MaxInt64", "9223372036854775808"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGoogleAdsCampaignID(tc.id)
			if err == nil {
				t.Fatalf("validateGoogleAdsCampaignID(%q) = nil; want a 400. This id reaches SQL and "+
					"returns a misleading 200 'unowned' answer instead of the declared 400.", tc.id)
			}
			// Read Message, not Error(): Goa's generated BadRequestError.Error() returns the
			// empty string, so asserting on err.Error() here would pass against any message at
			// all — including none.
			bad, ok := err.(*conn.BadRequestError)
			if !ok {
				t.Fatalf("validateGoogleAdsCampaignID(%q) returned %T; want *conn.BadRequestError", tc.id, err)
			}
			if bad.Code != "400" {
				t.Errorf("code = %q; want 400", bad.Code)
			}
			if !strings.Contains(bad.Message, "leading zero") {
				t.Errorf("validateGoogleAdsCampaignID(%q) message = %q; want it to name the constraint", tc.id, bad.Message)
			}
		})
	}
}

// TestGoogleAdsCampaignIDAcceptsRealIDs guards the other direction: the tightened rule must not
// start refusing ordinary ids, including the widest value that genuinely fits in an int64.
func TestGoogleAdsCampaignIDAcceptsRealIDs(t *testing.T) {
	accepted := []struct {
		name string
		id   string
	}{
		{"a real campaign id", "24183781329"},
		{"single digit", "7"},
		{"MaxInt64 itself", "9223372036854775807"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateGoogleAdsCampaignID(tc.id); err != nil {
				t.Fatalf("validateGoogleAdsCampaignID(%q) = %v; want nil", tc.id, err)
			}
		})
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package apivalidation

import (
	"regexp"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// TestMonitorAccountIDPatterns_MatchPlatformValidators guards against the two account_id
// shape checks for the account-monitor endpoints — the design-layer Pattern
// (design/connection.go's monitor-google-ads-account/monitor-reddit-ads-account methods,
// generated into gen/http/.../server/encode_decode.go's DecodeMonitor*AccountRequest) and the
// platform client's own runtime check (googleads.ValidateCustomerID/customerIDRE,
// reddit.ValidateAccountID/accountIDRe) — drifting apart silently. The two exist for
// different reasons (Goa rejects a malformed id at the transport before dispatch; the client
// re-checks because a non-HTTP caller skips Goa entirely — see ValidateCustomerID's and
// ValidateAccountID's doc comments) but must accept and reject the same ids, or one layer's
// "valid" becomes the other's runtime error.
//
// The design Pattern strings are copied here as literals rather than parsed out of generated
// code, so this test cannot read the wrong constant — it must be kept in sync by hand with
// design/connection.go whenever either method's account_id Pattern changes.
func TestMonitorAccountIDPatterns_MatchPlatformValidators(t *testing.T) {
	t.Run("google ads", func(t *testing.T) {
		designPattern := regexp.MustCompile(`^[0-9]+$`) // design/connection.go: monitor-google-ads-account account_id
		cases := []string{"8666746580", "0", "", "12-34", "abc123", "8666746580 ", " 8666746580", "123abc", "8666746580/"}
		for _, id := range cases {
			designOK := designPattern.MatchString(id)
			platformOK := googleads.ValidateCustomerID(id) == nil
			if designOK != platformOK {
				t.Errorf("account_id %q: design Pattern accepts=%v, googleads.ValidateCustomerID accepts=%v — the two shape checks have drifted apart", id, designOK, platformOK)
			}
		}
	})

	t.Run("reddit", func(t *testing.T) {
		designPattern := regexp.MustCompile(`^[A-Za-z0-9_]+$`) // design/connection.go: monitor-reddit-ads-account account_id
		cases := []string{"t2_gv9wtbfa", "T2_ABC123", "", "t2/../abc", "t2 gv9wtbfa", "t2-gv9wtbfa", "12345"}
		for _, id := range cases {
			designOK := designPattern.MatchString(id)
			platformOK := reddit.ValidateAccountID(id) == nil
			if designOK != platformOK {
				t.Errorf("account_id %q: design Pattern accepts=%v, reddit.ValidateAccountID accepts=%v — the two shape checks have drifted apart", id, designOK, platformOK)
			}
		}
	})
}

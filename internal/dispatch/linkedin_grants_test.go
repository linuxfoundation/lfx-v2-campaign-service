// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// TestVerifyLinkedInGrant pins the install-time check (#581): the pairing LinkedIn records must
// match, a confirmed disagreement refuses, and a credential that cannot act refuses before any
// request is made.
func TestVerifyLinkedInGrant(t *testing.T) {
	ctx := context.Background()
	t.Run("agreement passes", func(t *testing.T) {
		srv := linkedInAccountsServer(t, &requestRecorder{}, `{"id":123456789,"reference":"urn:li:organization:987654321"}`)
		if err := VerifyLinkedInGrant(ctx, []byte(goodLinkedInCreds), "123456789", "987654321", linkedin.WithBaseURL(srv.URL)); err != nil {
			t.Fatalf("VerifyLinkedInGrant: %v, want nil", err)
		}
	})
	t.Run("a different org refuses", func(t *testing.T) {
		srv := linkedInAccountsServer(t, &requestRecorder{}, `{"id":123456789,"reference":"urn:li:organization:111111111"}`)
		if err := VerifyLinkedInGrant(ctx, []byte(goodLinkedInCreds), "123456789", "987654321", linkedin.WithBaseURL(srv.URL)); err == nil {
			t.Fatal("VerifyLinkedInGrant accepted an account whose page is another org's")
		}
	})
	for name, plaintext := range map[string]string{"not json": "nope", "no token": `{"AccessToken":"  "}`} {
		t.Run(name, func(t *testing.T) {
			if err := VerifyLinkedInGrant(ctx, []byte(plaintext), "1", "2"); err == nil {
				t.Fatal("an unusable credential verified a grant")
			}
		})
	}
}

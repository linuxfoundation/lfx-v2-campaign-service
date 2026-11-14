// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// VerifyLinkedInGrant is the install-time check behind bootstrap.LinkedInGrantVerifier (#581):
// LinkedIn's own record of which organization accountID advertises for must be orgID. plaintext
// is the system row's decrypted credential document, decoded exactly as every dispatch path
// decodes it (linkedinCreds), so a grant is verified with the token that will later act on it.
//
// It runs VerifyAccountOrgReference, the connection test's check, and refuses on EVERY non-nil
// outcome, inconclusive included: the connection test may report "could not confirm" and move
// on, but a grant is trusted for the life of the row, so an unverifiable pairing is retried
// rather than written. The client's chain is not wrapped for the inconclusive case, for the
// redaction reason VerifyAccountOrg records; its fixed classification is used instead.
func VerifyLinkedInGrant(ctx context.Context, plaintext []byte, accountID, orgID string, opts ...linkedin.Option) error {
	var creds linkedinCreds
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		return errors.New("the system linkedin credential is not valid JSON")
	}
	creds.AccessToken = strings.TrimSpace(creds.AccessToken)
	if creds.AccessToken == "" {
		return errors.New("the system linkedin credential has no access token")
	}
	client := linkedin.NewClient(linkedinCredentials(creds, "Linux Foundation system account", ""), linkedin.RuntimeConfig{}, opts...)
	if err := client.VerifyAccountOrgReference(ctx, accountID, orgID); err != nil {
		if errors.Is(err, linkedin.ErrOrgVerificationInconclusive) {
			return fmt.Errorf("verification was inconclusive (%s); retry later", linkedin.SafeInconclusiveDetail(err))
		}
		return linkedinExpiry(err)
	}
	return nil
}

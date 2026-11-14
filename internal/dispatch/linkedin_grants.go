// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
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

// grantScoped returns a copy of r addressing the granted account accountID instead of the row's
// default (#581): accountID and providerConfig["org_id"] are the grant's, so every downstream
// reader of res.accountID (the provenance guards, the client's RuntimeConfig) and of the org id
// sees the granted pair. ok is false when r is not from the LF system row, or accountID is not one
// of its grants. Only the system row's grants are honoured: a project-owned row never carries
// any (the installer refuses every other scope), and honouring one there would let a row the
// project's campaign_manager can edit widen its own reach.
//
// The copy is deep for providerConfig, because the resolved value comes out of a shared cache and
// a write through the original map would change every other caller's org id.
func (r *resolved) grantScoped(accountID string) (*resolved, bool) {
	if r == nil || !r.fromSystem {
		return nil, false
	}
	g, ok := model.FindLinkedInGrant(r.grants, accountID)
	if !ok {
		return nil, false
	}
	c := r.clone()
	c.accountID = g.AccountID
	cfg := make(map[string]string, len(r.providerConfig)+1)
	for k, v := range r.providerConfig {
		cfg[k] = v
	}
	cfg["org_id"] = g.OrgID
	c.providerConfig = cfg
	if g.Label != "" {
		c.label = g.Label
	}
	c.grantAccount = g.AccountID
	return c, true
}

// existingOnSystem returns the system resolution sys addressing creationAccountID: sys itself
// when the row's own account created the campaign, a grant-scoped copy when one of the row's
// grants did (#581), and ok=false otherwise. It is the ONE place resolveExisting asks "can the
// system row address this campaign?", so the default account and the grants cannot drift apart
// across its arms.
func existingOnSystem(sys *resolved, creationAccountID string) (*resolved, bool) {
	if sys == nil {
		return nil, false
	}
	if matchesAccount(sys.accountID, creationAccountID) {
		return sys, true
	}
	return sys.grantScoped(creationAccountID)
}

// linkedinAccountSelectionEnabled reports whether this deployment lets linkedInConfig.adAccountId
// name one of the LF system row's granted accounts (#581). Read per call so it flips without a
// rebuild; only the exact value "true" turns it on.
func linkedinAccountSelectionEnabled() bool {
	return os.Getenv(constants.EnvLinkedInAdsAccountSelectionEnabled) == "true"
}

// selectLinkedInAccount resolves linkedInConfig.adAccountId against the resolution a create uses
// (#581). An empty value, or the resolved account itself, keeps res. Any other value is honoured
// only while LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED is on, only on the LF system row, and only
// for one of its grants: the result is then the grant-scoped copy, whose account AND org are the
// grant's, so the caller never chooses the page. Everything else is the pre-#581 refusal: the
// token reaches more accounts than LF creates in, and a caller-named account must never be
// treated as authorised just because the token can reach it.
//
// Every refusal is a pre-flight 400 (ErrCreateConfigInvalid, operator-safe) raised before any
// request, so PreflightCreate and Dispatch share it.
func selectLinkedInAccount(res *resolved, requested string) (*resolved, error) {
	requested = strings.TrimSpace(requested)
	current := strings.TrimSpace(res.accountID)
	if requested == "" || requested == current {
		return res, nil
	}
	if !linkedinAccountSelectionEnabled() {
		return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedin adAccountId %s does not match the connection's account %s — cross-account campaigns are not allowed on this deployment (%s is not \"true\")",
			domain.ErrCreateConfigInvalid, domain.CallerValue(requested), domain.CallerValue(current), constants.EnvLinkedInAdsAccountSelectionEnabled))
	}
	if granted, ok := res.grantScoped(requested); ok {
		return granted, nil
	}
	return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedin adAccountId %s is neither this project's connection account nor one of the LF system account's granted accounts; nothing was sent",
		domain.ErrCreateConfigInvalid, domain.CallerValue(requested)))
}

// linkedInGrantAccounts is the accounts list the LF system row answers while account selection
// is on (#581): its own account first, then each grant, labelled for the picker with the org the
// grant pins. It replaces the token-wide ListAdAccounts walk for the system scope because the
// grant list, not the token's reach, is what a create may name.
func linkedInGrantAccounts(res *resolved) []model.AccessibleAccount {
	out := make([]model.AccessibleAccount, 0, 1+len(res.grants))
	if acct := strings.TrimSpace(res.accountID); acct != "" {
		label := res.label
		if label == "" {
			label = acct
		}
		out = append(out, model.AccessibleAccount{ID: acct, Label: label + " (default, org " + strings.TrimSpace(res.providerConfig["org_id"]) + ")"})
	}
	for _, g := range res.grants {
		label := g.Label
		if label == "" {
			label = g.AccountID
		}
		out = append(out, model.AccessibleAccount{ID: g.AccountID, Label: label + " (org " + g.OrgID + ")"})
	}
	return out
}

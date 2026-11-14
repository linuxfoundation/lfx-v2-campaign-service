// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// maxGrantLabelLen is the label bound migration 000058 enforces (char_length(label) <= 200).
const maxGrantLabelLen = 200

// LinkedInGrantVerifier confirms, before anything is written, that LinkedIn records orgID as the
// organization accountID advertises for, using the system row's own decrypted credential
// (plaintext). It is the same check the connection test runs (VerifyAccountOrgReference), and
// the reason a grant can be trusted later: the org of a campaign created in a granted account is
// always taken from the grant, so a wrong pairing would publish one foundation's ads under
// another's page. Any non-nil error refuses the install, an inconclusive verdict included —
// retrying later is cheap, and a grant written unverified is not.
type LinkedInGrantVerifier func(ctx context.Context, plaintext []byte, accountID, orgID string) error

// ParseLinkedInGrant turns one `-linkedin-grant account:org[:label]` value into a grant. The
// label may itself contain colons; only the first two separate fields. Shapes are checked by
// ValidateLinkedInGrants, not here, so every grant in a run is reported against one rule set.
func ParseLinkedInGrant(s string) (model.LinkedInAccountGrant, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 {
		return model.LinkedInAccountGrant{}, fmt.Errorf("-linkedin-grant %q is not account:org[:label] (both ids are bare LinkedIn numbers)", s)
	}
	g := model.LinkedInAccountGrant{AccountID: strings.TrimSpace(parts[0]), OrgID: strings.TrimSpace(parts[1])}
	if len(parts) == 3 {
		g.Label = strings.TrimSpace(parts[2])
	}
	return g, nil
}

// ValidateLinkedInGrants holds a grant set to the rules the dispatcher and the table rely on:
// both ids numeric and within the shared id length bound (valueShapes' LinkedIn rule), a label
// within migration 000058's bound and free of control characters, and no account granted twice.
// rowAccount/rowOrg are the system row's own pair: a grant naming the default account
// is refused when its org contradicts the row's org_id, because the two would then disagree about
// which page authors that account's ads. A grant equal to the default pair is refused too, as
// redundant: the default is always addressable, and listing it twice invites the two to drift.
func ValidateLinkedInGrants(grants []model.LinkedInAccountGrant, rowAccount, rowOrg string) error {
	seen := make(map[string]bool, len(grants))
	for _, g := range grants {
		for key, v := range map[string]string{"account": g.AccountID, "org": g.OrgID} {
			if len(v) > maxValueLen || !numericID.MatchString(v) {
				return fmt.Errorf("bootstrap: linkedin grant %s id %q must be a bare LinkedIn number (at most %d digits)", key, v, maxValueLen)
			}
		}
		if len([]rune(g.Label)) > maxGrantLabelLen {
			return fmt.Errorf("bootstrap: linkedin grant label for account %s is longer than %d characters", g.AccountID, maxGrantLabelLen)
		}
		if strings.IndexFunc(g.Label, unicode.IsControl) >= 0 {
			return fmt.Errorf("bootstrap: linkedin grant label for account %s contains a control character", g.AccountID)
		}
		if seen[g.AccountID] {
			return fmt.Errorf("bootstrap: linkedin account %s is granted more than once; each account has exactly one org", g.AccountID)
		}
		seen[g.AccountID] = true
		if g.AccountID == strings.TrimSpace(rowAccount) {
			if g.OrgID != strings.TrimSpace(rowOrg) {
				return fmt.Errorf("bootstrap: linkedin grant pairs the system row's own account %s with org %s, but the row's org_id is %s; change -config org_id instead", g.AccountID, g.OrgID, rowOrg)
			}
			return fmt.Errorf("bootstrap: linkedin account %s is the system row's own account and is always addressable; drop it from -linkedin-grant", g.AccountID)
		}
	}
	return nil
}

// InstallLinkedInGrants replaces the LinkedIn account grants (#581) on the LF system LinkedIn row
// with grants, after validating every one and verifying each pairing with verify. clear removes
// every grant and is never combined with grants. The row must already exist: grants extend a
// system row, they are never a way to create one, so a missing row is refused with the command
// that installs it.
//
// The set is DECLARATIVE: what this run names is the whole list afterwards. Accounts granted
// before and not named again are removed, so an operator who drops a foundation's account from
// the command has revoked it — the only reading under which the flag list the operator sees and
// the table can be compared at a glance. Campaigns already created in a revoked account keep
// their recorded account, and pause/budget/metrics on them answer the account-mismatch refusal
// until the grant is restored.
//
// Nothing is written unless every grant validates and verifies. The replace itself is one
// transaction gated on the row's version (domain.LinkedInGrantWriter), so a concurrent install
// loses with ErrPreconditionFailed instead of interleaving.
func InstallLinkedInGrants(
	ctx context.Context,
	repo domain.ConnectionReader,
	writer domain.LinkedInGrantWriter,
	enc domain.Encryptor,
	verify LinkedInGrantVerifier,
	grants []model.LinkedInAccountGrant,
	clear bool,
) error {
	if repo == nil || writer == nil || enc == nil {
		return errors.New("bootstrap: repository, grant writer and encryptor are required")
	}
	if clear && len(grants) > 0 {
		return errors.New("bootstrap: -clear-linkedin-grants and -linkedin-grant ask for opposite things; supply one")
	}
	if !clear && len(grants) == 0 {
		return errors.New("bootstrap: no linkedin grants to install; pass -linkedin-grant account:org[:label] (repeatable) or -clear-linkedin-grants")
	}
	if !clear && verify == nil {
		return errors.New("bootstrap: a grant verifier is required to install linkedin grants")
	}
	provider := model.ProviderLinkedInAds
	existing, err := repo.Get(ctx, model.SystemProjectID, provider)
	if err == nil && existing == nil {
		err = domain.ErrNotFound
	}
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("bootstrap: no system %s connection exists yet; install it first with -provider %s and the credential on stdin, then add grants", provider, provider)
	case err != nil:
		return fmt.Errorf("bootstrap: read system %s connection: %w", provider, err)
	}
	sorted := append([]model.LinkedInAccountGrant(nil), grants...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].AccountID < sorted[j].AccountID })
	if err := ValidateLinkedInGrants(sorted, existing.AccountID, existing.ProviderConfig["org_id"]); err != nil {
		return err
	}
	if len(sorted) > 0 {
		if len(existing.EncryptedCredentials) == 0 {
			return fmt.Errorf("bootstrap: the system %s connection has no stored credential to verify grants with; install one first", provider)
		}
		plaintext, derr := enc.Decrypt(existing.EncryptedCredentials)
		if derr != nil {
			// The cause is dropped: it is derived from the stored ciphertext.
			return fmt.Errorf("bootstrap: the system %s credential could not be decrypted to verify grants (check the encryption key)", provider)
		}
		for _, g := range sorted {
			if verr := verify(ctx, plaintext, g.AccountID, g.OrgID); verr != nil {
				return fmt.Errorf("bootstrap: linkedin grant %s:%s did not verify, nothing was written: %w", g.AccountID, g.OrgID, verr)
			}
		}
	}
	if _, werr := writer.ReplaceLinkedInAccountGrants(ctx, model.SystemProjectID, sorted, systemActor, existing.Version); werr != nil {
		if errors.Is(werr, domain.ErrPreconditionFailed) {
			return fmt.Errorf("bootstrap: system %s connection changed while this command ran; nothing was written, rerun it: %w", provider, werr)
		}
		return fmt.Errorf("bootstrap: write linkedin grants: %w", werr)
	}
	return nil
}

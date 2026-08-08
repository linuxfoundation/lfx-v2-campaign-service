// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// stubRepo records every call so a test can assert WHICH repository operation ran,
// not merely that the installer returned nil. The distinction matters: create and
// rotate are both "success" from the caller's side and only one of them is correct
// for a given starting state.
type stubRepo struct {
	row     *model.Connection
	getErr  error
	calls   []string
	created *model.Connection
	setCT   []byte
	updated *model.Connection
	updVer  int64
}

func (r *stubRepo) Get(_ context.Context, projectID string, _ model.Provider) (*model.Connection, error) {
	r.calls = append(r.calls, "get:"+projectID)
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.row == nil {
		return nil, domain.ErrNotFound
	}
	c := *r.row
	return &c, nil
}

func (r *stubRepo) Create(_ context.Context, c *model.Connection) (*model.Connection, error) {
	r.calls = append(r.calls, "create")
	r.created = c
	return c, nil
}

func (r *stubRepo) Update(_ context.Context, c *model.Connection, expectedVersion int64) (*model.Connection, error) {
	r.calls = append(r.calls, "update")
	r.updated, r.updVer = c, expectedVersion
	return c, nil
}

func (r *stubRepo) SetCredential(_ context.Context, _ string, _ model.Provider, ct []byte, _ *model.Actor) (*model.Connection, error) {
	r.calls = append(r.calls, "set-credential")
	r.setCT = ct
	return r.row, nil
}

func (r *stubRepo) Delete(context.Context, string, model.Provider, *model.Actor) error {
	r.calls = append(r.calls, "delete")
	return nil
}

// fakeEnc marks its output so a test can prove the stored blob is CIPHERTEXT and
// not the plaintext that went in. An installer that forgot to encrypt would still
// store a blob that decrypts "fine" in a test using an identity encryptor.
type fakeEnc struct{ err error }

func (e fakeEnc) Encrypt(plain []byte) ([]byte, error) {
	if e.err != nil {
		return nil, e.err
	}
	return append([]byte("enc:"), plain...), nil
}

func (e fakeEnc) Decrypt(ct []byte) ([]byte, error) {
	return append([]byte{}, ct[len("enc:"):]...), nil
}

const goodCreds = `{"refreshToken":"rt","clientId":"ci","clientSecret":"cs","developerToken":"dt"}`

// TestInstallCreatesAtTheReservedScope pins the two properties the whole feature
// rests on: the row lands at model.SystemProjectID (a row at any other scope would
// never be found by the fallback), and the credential is stored ENCRYPTED.
func TestInstallCreatesAtTheReservedScope(t *testing.T) {
	repo := &stubRepo{}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "8666746580", []byte(goodCreds)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if repo.created == nil {
		t.Fatalf("no row created; calls = %v", repo.calls)
	}
	if repo.created.ProjectID != model.SystemProjectID {
		t.Fatalf("project_id = %q, want %q", repo.created.ProjectID, model.SystemProjectID)
	}
	if got := string(repo.created.EncryptedCredentials); !strings.HasPrefix(got, "enc:") {
		t.Fatalf("credentials stored as %q; they must pass through the encryptor", got)
	}
	if repo.created.Status != model.StatusActive {
		t.Fatalf("status = %q, want active", repo.created.Status)
	}
	if repo.created.CreatedBy == nil || repo.created.UpdatedBy == nil {
		t.Fatal("system row has no actor attribution")
	}
}

// TestInstallIsIdempotent pins rotation. A second run must NOT call Create — the
// singleton index would reject it — and must rotate through SetCredential instead.
func TestInstallIsIdempotent(t *testing.T) {
	repo := &stubRepo{row: &model.Connection{
		ProjectID: model.SystemProjectID, Provider: model.ProviderGoogleAds,
		AccountID: "8666746580", Version: 4, Status: model.StatusActive,
	}}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "8666746580", []byte(goodCreds)); err != nil {
		t.Fatalf("install: %v", err)
	}
	for _, c := range repo.calls {
		if c == "create" {
			t.Fatalf("rotation called Create; calls = %v", repo.calls)
		}
	}
	if repo.setCT == nil {
		t.Fatalf("rotation did not set a credential; calls = %v", repo.calls)
	}
	// No account id change was asked for, so nothing may Update — an Update here
	// would rewrite fields the operator did not supply.
	if repo.updated != nil {
		t.Fatalf("rotation updated the row with no account-id change: %+v", repo.updated)
	}
}

// TestRotationUsesThePostCredentialVersion pins the ordering bug that would leave a
// rotation half-applied: SetCredential bumps the version, so an Update gated on the
// version read BEFORE it fails the optimistic check and the account id never lands.
func TestRotationUsesThePostCredentialVersion(t *testing.T) {
	repo := &stubRepo{row: &model.Connection{
		ProjectID: model.SystemProjectID, Provider: model.ProviderGoogleAds,
		AccountID: "old", Version: 4, Status: model.StatusActive,
	}}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "new", []byte(goodCreds)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if repo.updated == nil {
		t.Fatalf("account id change did not Update; calls = %v", repo.calls)
	}
	if repo.updVer != 5 {
		t.Fatalf("Update gated on version %d, want 5 (the version SetCredential left behind)", repo.updVer)
	}
	if repo.updated.AccountID != "new" {
		t.Fatalf("account id = %q, want %q", repo.updated.AccountID, "new")
	}
}

// TestInstallDoesNotCreateOnAnUnreadableRow pins the fail-closed half. Only
// ErrNotFound may create: on any other read failure the row's state is UNKNOWN, and
// creating on top of an existing-but-unreadable row overwrites a credential nobody
// meant to replace.
func TestInstallDoesNotCreateOnAnUnreadableRow(t *testing.T) {
	repo := &stubRepo{getErr: errors.New("connection refused")}
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "", []byte(goodCreds))
	if err == nil {
		t.Fatal("install succeeded despite an unreadable row")
	}
	if repo.created != nil {
		t.Fatalf("created a row on an unreadable read: %+v", repo.created)
	}
}

// TestInstallRejectsUnusableInput covers the arms that must fail BEFORE anything is
// written. `null`, `[]` and a bare string all parse as valid JSON and would each
// store a blob that decrypts cleanly and then fails at dispatch with nothing
// pointing back at the install.
func TestInstallRejectsUnusableInput(t *testing.T) {
	cases := map[string]struct {
		provider model.Provider
		creds    string
	}{
		"unknown provider": {"not-a-provider", goodCreds},
		"not json":         {model.ProviderGoogleAds, "not json"},
		"json null":        {model.ProviderGoogleAds, "null"},
		"json array":       {model.ProviderGoogleAds, "[]"},
		"json string":      {model.ProviderGoogleAds, `"rt"`},
		"empty object":     {model.ProviderGoogleAds, "{}"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{}
			if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				tc.provider, "", []byte(tc.creds)); err == nil {
				t.Fatal("install accepted an unusable input")
			}
			if len(repo.calls) != 0 {
				t.Fatalf("touched the repository before validating: %v", repo.calls)
			}
		})
	}
}

// TestInstallDoesNotWriteWhenEncryptionFails pins that a failed Encrypt stops the
// install rather than storing an empty blob — which resolve() would later refuse as
// an absent credential, turning a key problem into "the system account is missing".
func TestInstallDoesNotWriteWhenEncryptionFails(t *testing.T) {
	repo := &stubRepo{}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{err: errors.New("boom")},
		model.ProviderGoogleAds, "", []byte(goodCreds)); err == nil {
		t.Fatal("install succeeded despite an encryption failure")
	}
	if len(repo.calls) != 0 {
		t.Fatalf("wrote to the repository after an encryption failure: %v", repo.calls)
	}
}

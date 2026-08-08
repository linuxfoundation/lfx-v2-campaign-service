// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// stubRepo records every call so a test can assert WHICH operation ran: create and
// rotate are both "success" to the caller and only one is right for a given state.
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

// fakeEnc marks its output so a test can prove the stored blob is CIPHERTEXT: an
// installer that forgot to encrypt still "decrypts fine" under an identity encryptor.
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

// goodCreds is the snake_case WIRE form — what design/connection.go documents and what
// an operator holding a working set-credential body would actually pipe in.
const goodCreds = `{"refresh_token":"rt","client_id":"ci","client_secret":"cs","developer_token":"dt"}`

// TestStoredBlobDecodesIntoTheReader is the test that would have caught the original defect.
// Encrypting the document verbatim was not wrong about JSON, it was wrong about WHO READS
// IT — so this asserts on the DECODE, unmarshalling the stored blob into a struct shaped
// like internal/dispatch's googleAdsCreds. Revert the folding and it fails with empty
// fields, exactly how the bug presented.
func TestStoredBlobDecodesIntoTheReader(t *testing.T) {
	for name, in := range map[string]string{
		"wire snake_case": goodCreds,
		"camelCase":       `{"refreshToken":"rt","clientId":"ci","clientSecret":"cs","developerToken":"dt"}`,
		"stored Go names": `{"RefreshToken":"rt","ClientID":"ci","ClientSecret":"cs","DeveloperToken":"dt"}`,
	} {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{}
			if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				model.ProviderGoogleAds, "", []byte(in)); err != nil {
				t.Fatalf("install: %v", err)
			}
			var got struct{ ClientID, ClientSecret, DeveloperToken, RefreshToken string }
			plain, err := fakeEnc{}.Decrypt(repo.created.EncryptedCredentials)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if err := json.Unmarshal(plain, &got); err != nil {
				t.Fatalf("stored blob does not decode: %v", err)
			}
			want := struct{ ClientID, ClientSecret, DeveloperToken, RefreshToken string }{"ci", "cs", "dt", "rt"}
			if got != want {
				t.Fatalf("reader saw %+v, want %+v — the stored keys do not reach the dispatch struct", got, want)
			}
		})
	}
}

// TestInstallCreatesAtTheReservedScope pins the two properties the feature rests on: the
// row lands at model.SystemProjectID, and the credential is stored ENCRYPTED.
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

// TestInstallIsIdempotent pins rotation: a second run must NOT Create (the singleton
// index would reject it) and must rotate through SetCredential instead.
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
	// No account-id change was asked for, so nothing may Update.
	if repo.updated != nil {
		t.Fatalf("rotation updated the row with no account-id change: %+v", repo.updated)
	}
}

// TestRotationUsesThePostCredentialVersion pins the ordering bug that half-applies a
// rotation: an Update gated on the pre-SetCredential version fails the optimistic check.
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

// TestInstallDoesNotCreateOnAnUnreadableRow pins the fail-closed half: on a read error
// that is not ErrNotFound the row's state is unknown, so nothing may be created.
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
// written — `null`, `[]` and a bare string all parse as valid JSON, none is a credential.
func TestInstallRejectsUnusableInput(t *testing.T) {
	cases := map[string]struct {
		provider model.Provider
		creds    string
	}{
		"unknown provider":        {"not-a-provider", goodCreds},
		"missing developer_token": {model.ProviderGoogleAds, `{"refresh_token":"rt","client_id":"ci","client_secret":"cs"}`},
		"empty required value":    {model.ProviderGoogleAds, `{"refresh_token":"","client_id":"ci","client_secret":"cs","developer_token":"dt"}`},
		"null required value":     {model.ProviderGoogleAds, `{"refresh_token":null,"client_id":"ci","client_secret":"cs","developer_token":"dt"}`},
		"colliding spellings":     {model.ProviderGoogleAds, `{"refresh_token":"a","refreshToken":"b","client_id":"ci","client_secret":"cs","developer_token":"dt"}`},
		"not json":                {model.ProviderGoogleAds, "not json"},
		"json null":               {model.ProviderGoogleAds, "null"},
		"json array":              {model.ProviderGoogleAds, "[]"},
		"json string":             {model.ProviderGoogleAds, `"rt"`},
		"empty object":            {model.ProviderGoogleAds, "{}"},
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
// install: an empty blob would later read as an ABSENT credential, not a key problem.
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

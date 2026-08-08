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

// stubRepo records every call so a test can assert WHICH operation ran: create and rotate are
// both "success" to the caller and only one is right for a given state.
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

// fakeEnc marks its output so a test can prove the stored blob is CIPHERTEXT — an installer that
// forgot to encrypt still "decrypts fine" under an identity encryptor.
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

// goodCreds is the snake_case WIRE form design/connection.go documents.
const goodCreds = `{"refresh_token":"rt","client_id":"ci","client_secret":"cs","developer_token":"dt"}`

// TestStoredBlobDecodesIntoTheReader would have caught the original defect: encrypting the
// document verbatim was not wrong about JSON, it was wrong about WHO READS IT — so it asserts on
// the DECODE, into a struct shaped like dispatch's googleAdsCreds. It also pins where the row
// lands and that the blob is CIPHERTEXT, both of which hold for every spelling.
func TestStoredBlobDecodesIntoTheReader(t *testing.T) {
	for name, in := range map[string]string{
		"wire snake_case": goodCreds,
		"camelCase":       `{"refreshToken":"rt","clientId":"ci","clientSecret":"cs","developerToken":"dt"}`,
		"stored Go names": `{"RefreshToken":"rt","ClientID":"ci","ClientSecret":"cs","DeveloperToken":"dt"}`,
	} {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{}
			if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				model.ProviderGoogleAds, "8666746580", nil, []byte(in)); err != nil {
				t.Fatalf("install: %v", err)
			}
			if repo.created == nil {
				t.Fatalf("no row created; calls = %v", repo.calls)
			}
			if got := string(repo.created.EncryptedCredentials); !strings.HasPrefix(got, "enc:") ||
				repo.created.ProjectID != model.SystemProjectID {
				t.Fatalf("row at %q with credentials %q; want the reserved scope, encrypted",
					repo.created.ProjectID, got)
			}
			if repo.created.Status != model.StatusActive || repo.created.UpdatedBy == nil {
				t.Fatalf("row not active/attributed: %+v", repo.created)
			}
			type creds struct{ ClientID, ClientSecret, DeveloperToken, RefreshToken string }
			var got creds
			plain, err := fakeEnc{}.Decrypt(repo.created.EncryptedCredentials)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if err := json.Unmarshal(plain, &got); err != nil {
				t.Fatalf("stored blob does not decode: %v", err)
			}
			if want := (creds{"ci", "cs", "dt", "rt"}); got != want {
				t.Fatalf("reader saw %+v, want %+v — the stored keys do not reach the dispatch struct", got, want)
			}
		})
	}
}

// TestSecondInstallRotates: a second run must NOT Create (the singleton index would reject it)
// but rotate through SetCredential. Phase two pins the ordering bug that half-applies a rotation
// — an Update gated on the PRE-SetCredential version fails the optimistic check.
func TestSecondInstallRotates(t *testing.T) {
	row := func(accountID string, cfg map[string]string) *stubRepo {
		return &stubRepo{row: &model.Connection{
			ProjectID: model.SystemProjectID, Provider: model.ProviderGoogleAds,
			AccountID: accountID, ProviderConfig: cfg, Version: 4, Status: model.StatusActive,
		}}
	}

	repo := row("8666746580", map[string]string{"login_customer_id": "999"})
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "8666746580", nil, []byte(goodCreds)); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	// Nothing changed, so nothing may Update — an omitted flag must not blank a set column.
	if repo.created != nil || repo.setCT == nil || repo.updated != nil {
		t.Fatalf("rotation must SetCredential only; calls = %v, updated = %+v", repo.calls, repo.updated)
	}

	repo = row("old", nil)
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "new", nil, []byte(goodCreds)); err != nil {
		t.Fatalf("rotate with a new account id: %v", err)
	}
	// Version 5 is what SetCredential left behind; the pre-rotation 4 would fail the check.
	if repo.updated == nil || repo.updVer != 5 || repo.updated.AccountID != "new" {
		t.Fatalf("account id change: updated = %+v at version %d, want new at 5", repo.updated, repo.updVer)
	}
}

// TestInstallRejectsUnusableInput covers the arms that must fail BEFORE anything is written:
// `null`, `[]` and a bare string all parse as valid JSON, none is a credential.
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
				tc.provider, "", nil, []byte(tc.creds)); err == nil {
				t.Fatal("install accepted an unusable input")
			}
			if len(repo.calls) != 0 {
				t.Fatalf("touched the repository before validating: %v", repo.calls)
			}
		})
	}
}

// TestInstallRequiresProviderConfigThatDispatchDemands: a provider whose adapter refuses to
// create without a config column cannot be installed without it — the row would decrypt fine and
// fail at campaign creation, far from the installer.
func TestInstallRequiresProviderConfigThatDispatchDemands(t *testing.T) {
	linkedInCreds := []byte(`{"access_token":"tok"}`)
	repo := &stubRepo{}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderLinkedInAds, "1", nil, linkedInCreds); err == nil {
		t.Fatal("installed a linkedin row with no org_id")
	}
	if repo.created != nil {
		t.Fatalf("created an unusable row: %+v", repo.created)
	}
	cfg := map[string]string{"org_id": "987"}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderLinkedInAds, "1", cfg, linkedInCreds); err != nil {
		t.Fatalf("install with org_id: %v", err)
	}
	if got := repo.created.ProviderConfig["org_id"]; got != "987" {
		t.Fatalf("org_id = %q, want 987 — config never reached the row", got)
	}
}

// TestRotationMergesConfigIntoTheRow: Update rewrites EVERY config column from the map, so
// supplying one key must not NULL its siblings — Meta stores page_id AND app_id. Phase two pins
// why the requirement reads the MERGED map, not the flags: a credential rotation supplies no
// -config, and demanding page_id of the flags forces every rotation to re-state it — which is
// what wiped app_id to begin with.
func TestRotationMergesConfigIntoTheRow(t *testing.T) {
	metaRow := func() *stubRepo {
		return &stubRepo{row: &model.Connection{
			ProjectID: model.SystemProjectID, Provider: model.ProviderMetaAds,
			AccountID: "act_1", ProviderConfig: map[string]string{"page_id": "p1", "app_id": "a1"},
			Version: 4, Status: model.StatusActive,
		}}
	}
	metaCreds := []byte(`{"access_token":"tok","app_secret":"sec"}`)

	repo := metaRow()
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderMetaAds, "", map[string]string{"page_id": "p2"}, metaCreds); err != nil {
		t.Fatalf("rotate with config: %v", err)
	}
	if repo.updated == nil {
		t.Fatalf("config change did not Update; calls = %v", repo.calls)
	}
	if got := repo.updated.ProviderConfig; got["app_id"] != "a1" || got["page_id"] != "p2" {
		t.Fatalf("config = %v, want page_id p2 with app_id a1 preserved", got)
	}

	repo = metaRow()
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderMetaAds, "", nil, metaCreds); err != nil {
		t.Fatalf("rotate with no config: %v", err)
	}
	if repo.setCT == nil {
		t.Fatalf("credential not rotated; calls = %v", repo.calls)
	}
	if repo.updated != nil {
		t.Fatalf("rewrote config nobody supplied: %+v", repo.updated.ProviderConfig)
	}
}

// TestInstallWritesNothingWhenItCannotProceed: both arms would leave a row that reads as
// something it is not — a created row over a state we could not observe, or an empty blob that
// later looks like an ABSENT credential rather than a key problem.
func TestInstallWritesNothingWhenItCannotProceed(t *testing.T) {
	for name, tc := range map[string]struct {
		repo *stubRepo
		enc  fakeEnc
	}{
		"unreadable row":     {&stubRepo{getErr: errors.New("connection refused")}, fakeEnc{}},
		"encryption failure": {&stubRepo{}, fakeEnc{err: errors.New("boom")}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := InstallSystemCredentials(context.Background(), tc.repo, tc.enc,
				model.ProviderGoogleAds, "", nil, []byte(goodCreds)); err == nil {
				t.Fatal("install succeeded")
			}
			if tc.repo.created != nil || tc.repo.setCT != nil || tc.repo.updated != nil {
				t.Fatalf("wrote to the repository anyway; calls = %v", tc.repo.calls)
			}
		})
	}
}

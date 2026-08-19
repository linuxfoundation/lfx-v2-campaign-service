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
	updErr  error
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

// UpdateWithCredential records the ciphertext in the SAME field SetCredential uses, so a test
// asserting "the secret was written" cannot pass by the row being rotated through the old
// two-write path — the call list is what separates them.
func (r *stubRepo) UpdateWithCredential(_ context.Context, c *model.Connection, ct []byte, expectedVersion int64) (*model.Connection, error) {
	r.calls = append(r.calls, "update-with-credential")
	r.updated, r.updVer, r.setCT = c, expectedVersion, ct
	if r.updErr != nil {
		return nil, r.updErr
	}
	return c, nil
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
				model.ProviderGoogleAds, "8666746580", false, nil, []byte(in)); err != nil {
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
// but rotate the existing row. Phase two pins that the rotation is ONE version-gated write.
// It used to be two — Update then SetCredential — and ordering them only chose which mixed
// state a failure left behind: new secret against the old account id, or the reverse. So the
// assertion is not about order but about the ABSENCE of a second write: neither "update" nor
// "set-credential" may appear, and the combined call is the last thing that happens.
func TestSecondInstallRotates(t *testing.T) {
	row := func(accountID string, cfg map[string]string) *stubRepo {
		return &stubRepo{row: &model.Connection{
			ProjectID: model.SystemProjectID, Provider: model.ProviderGoogleAds,
			AccountID: accountID, ProviderConfig: cfg, Version: 4, Status: model.StatusActive,
		}}
	}

	repo := row("8666746580", map[string]string{"login_customer_id": "999"})
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "8666746580", false, nil, []byte(goodCreds)); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	// The credential goes in with the row, and an omitted flag must not blank a set column:
	// the write rewrites every column, so "unchanged" has to mean the OLD value was resent.
	if repo.created != nil || repo.setCT == nil || repo.updated == nil {
		t.Fatalf("rotation must write the row and the secret together; calls = %v", repo.calls)
	}
	if repo.updated.AccountID != "8666746580" || repo.updated.ProviderConfig["login_customer_id"] != "999" {
		t.Fatalf("rotation blanked a column nobody supplied: %+v", repo.updated)
	}

	repo = row("8666746580", nil)
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "9746983954", false, nil, []byte(goodCreds)); err != nil {
		t.Fatalf("rotate with a new account id: %v", err)
	}
	// Version 4 is the row's own, and the write is gated on it: a concurrent rotation that
	// bumped the row must lose here rather than interleave.
	if repo.updated == nil || repo.updVer != 4 || repo.updated.AccountID != "9746983954" {
		t.Fatalf("account id change: updated = %+v at version %d, want new at 4", repo.updated, repo.updVer)
	}
	// ONE write. A second call is the mixed-state bug this replaced: the account and the
	// credential must not be separately observable.
	for _, c := range repo.calls {
		if c == "update" || c == "set-credential" {
			t.Fatalf("calls = %v, want account id and secret in one version-gated write", repo.calls)
		}
	}
	if repo.calls[len(repo.calls)-1] != "update-with-credential" {
		t.Fatalf("calls = %v, want the combined write last", repo.calls)
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

		// The LinkedIn refresh trio is all-or-none. Each of these installs a row whose
		// CanRefresh() is false while looking like refresh was configured.
		"linkedin refresh trio missing client_secret": {model.ProviderLinkedInAds,
			`{"access_token":"at","refresh_token":"rt","client_id":"ci"}`},
		"linkedin refresh trio missing client_id": {model.ProviderLinkedInAds,
			`{"access_token":"at","refresh_token":"rt","client_secret":"cs"}`},
		"linkedin refresh trio refresh_token only": {model.ProviderLinkedInAds,
			`{"access_token":"at","refresh_token":"rt"}`},
		"linkedin refresh trio missing refresh_token": {model.ProviderLinkedInAds,
			`{"access_token":"at","client_id":"ci","client_secret":"cs"}`},
		// A member present but empty/whitespace is NOT supplied: it must not satisfy the
		// group and install a trio that cannot authenticate.
		"linkedin refresh trio empty client_secret": {model.ProviderLinkedInAds,
			`{"access_token":"at","refresh_token":"rt","client_id":"ci","client_secret":""}`},
		"linkedin refresh trio whitespace client_secret": {model.ProviderLinkedInAds,
			`{"access_token":"at","refresh_token":"rt","client_id":"ci","client_secret":"   "}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{}
			if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				tc.provider, "", false, nil, []byte(tc.creds)); err == nil {
				t.Fatal("install accepted an unusable input")
			}
			if len(repo.calls) != 0 {
				t.Fatalf("touched the repository before validating: %v", repo.calls)
			}
		})
	}
}

// TestInstallAcceptsBothValidLinkedInCredentialShapes guards the other direction of the
// all-or-none rule. Only a PARTIAL trio is invalid: a bearer-only row is the common case,
// since LinkedIn issues refresh tokens only to approved Marketing Developer Platform
// partners, and rejecting it would make the LF system row uninstallable for most apps.
//
// Without this, the rejection table above is satisfied by a guard that refuses every
// LinkedIn install.
func TestInstallAcceptsBothValidLinkedInCredentialShapes(t *testing.T) {
	cases := map[string]string{
		"bearer only":       `{"access_token":"at"}`,
		"full refresh trio": `{"access_token":"at","refresh_token":"rt","client_id":"ci","client_secret":"cs"}`,
	}
	for name, creds := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{}
			// LinkedIn account ids are digits-only (valueShapes) and the provider requires
			// an org_id config; both are refused before the credential group is reached, so
			// supply valid ones to isolate what this test is actually about.
			if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				model.ProviderLinkedInAds, "512103652", false,
				map[string]string{"org_id": "987"}, []byte(creds)); err != nil {
				t.Fatalf("install rejected a valid LinkedIn credential shape: %v", err)
			}
			if len(repo.calls) == 0 {
				t.Fatal("a valid credential must reach the repository")
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
		model.ProviderLinkedInAds, "1", false, nil, linkedInCreds); err == nil {
		t.Fatal("installed a linkedin row with no org_id")
	}
	if repo.created != nil {
		t.Fatalf("created an unusable row: %+v", repo.created)
	}
	cfg := map[string]string{"org_id": "987"}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderLinkedInAds, "1", false, cfg, linkedInCreds); err != nil {
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
			AccountID: "act_1", ProviderConfig: map[string]string{"page_id": "111", "app_id": "a1"},
			Version: 4, Status: model.StatusActive,
		}}
	}
	metaCreds := []byte(`{"access_token":"tok","app_secret":"sec"}`)

	repo := metaRow()
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderMetaAds, "", false, map[string]string{"page_id": "222"}, metaCreds); err != nil {
		t.Fatalf("rotate with config: %v", err)
	}
	if repo.updated == nil {
		t.Fatalf("config change did not Update; calls = %v", repo.calls)
	}
	if got := repo.updated.ProviderConfig; got["app_id"] != "a1" || got["page_id"] != "222" {
		t.Fatalf("config = %v, want page_id 222 with app_id a1 preserved", got)
	}

	repo = metaRow()
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderMetaAds, "", false, nil, metaCreds); err != nil {
		t.Fatalf("rotate with no config: %v", err)
	}
	if repo.setCT == nil || repo.updated == nil {
		t.Fatalf("credential not rotated; calls = %v", repo.calls)
	}
	if got := repo.updated.ProviderConfig; got["page_id"] != "111" || got["app_id"] != "a1" {
		t.Fatalf("config = %v, want the row's own values resent when no -config is supplied", got)
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
				model.ProviderGoogleAds, "", false, nil, []byte(goodCreds)); err == nil {
				t.Fatal("install succeeded")
			}
			if tc.repo.created != nil || tc.repo.setCT != nil || tc.repo.updated != nil {
				t.Fatalf("wrote to the repository anyway; calls = %v", tc.repo.calls)
			}
		})
	}
}

// TestInstallRejectsMisshapenValues: the installer writes PAST the API, so a value the rest of
// the system refuses must not reach an ACTIVE system row and turn into a dispatch failure nobody
// connects back to install time. Both sources of the rule are covered: design/connection.go's
// Pattern() (Meta, X, LinkedIn) AND the runtime validators for the three providers whose design
// checks presence alone (Google Ads, Microsoft, Reddit) — reading only the design was the gap.
// An OMITTED account id is not a misshapen one; that is the legal credentials-first state.
func TestInstallRejectsMisshapenValues(t *testing.T) {
	metaCreds := []byte(`{"access_token":"tok","app_secret":"sec"}`)
	xCreds := []byte(`{"consumer_key":"a","consumer_secret":"b","access_token":"c","access_token_secret":"d"}`)
	gaCreds := []byte(`{"refresh_token":"rt","client_id":"ci","client_secret":"cs","developer_token":"dt"}`)
	msCreds := []byte(`{"client_id":"ci","client_secret":"cs","refresh_token":"rt","developer_token":"dt"}`)
	rdCreds := []byte(`{"client_id":"ci","client_secret":"cs","refresh_token":"rt"}`)
	cases := map[string]struct {
		provider  model.Provider
		accountID string
		cfg       map[string]string
		creds     []byte
		wantErr   bool
	}{
		"meta account id not act_<digits>": {model.ProviderMetaAds, "foo", map[string]string{"page_id": "1"}, metaCreds, true},
		"meta page id not numeric":         {model.ProviderMetaAds, "act_1", map[string]string{"page_id": "abc"}, metaCreds, true},
		"x id carrying a path separator":   {model.ProviderTwitterAds, "8r7gb", map[string]string{"funding_instrument_id": "a/b"}, xCreds, true},
		"meta values in shape":             {model.ProviderMetaAds, "act_1", map[string]string{"page_id": "1"}, metaCreds, false},
		// Omission is a SHAPE question here, and an omitted id has no shape to fail. Whether
		// omission is ALLOWED is requireAccountID's separate question — see
		// TestInstallRequiresAnAccountIDWhereNothingCanSupplyOneLater. A provider that allows
		// it is used so this case still tests only what it claims to.
		"omitted account id is legal": {model.ProviderGoogleAds, "", map[string]string{"login_customer_id": "1"}, gaCreds, false},

		// Runtime-validator providers. Each of these exited 0 before the rule was added.
		"google ads account id not numeric":       {model.ProviderGoogleAds, "foo", nil, gaCreds, true},
		"google ads login customer id has dashes": {model.ProviderGoogleAds, "8666746580", map[string]string{"login_customer_id": "974-698-3954"}, gaCreds, true},
		"google ads values in shape":              {model.ProviderGoogleAds, "8666746580", map[string]string{"login_customer_id": "9746983954"}, gaCreds, false},
		"microsoft customer id not numeric":       {model.ProviderMicrosoftAds, "1234", map[string]string{"customer_id": "cus-9"}, msCreds, true},
		"microsoft values in shape":               {model.ProviderMicrosoftAds, "1234", map[string]string{"customer_id": "9"}, msCreds, false},
		// Reddit now REQUIRES conversion_pixel_id (see requiredConfigKeys), so the in-shape
		// case must supply one -- these rows assert the ACCOUNT ID's shape, and omitting the
		// pixel would make them fail for an unrelated reason and stop testing what they name.
		"reddit account id with a path separator": {model.ProviderRedditAds, "t2_gv9../x", map[string]string{"conversion_pixel_id": "a2_pixel"}, rdCreds, true},
		"reddit account id in shape":              {model.ProviderRedditAds, "t2_gv9wtbfa", map[string]string{"conversion_pixel_id": "a2_pixel"}, rdCreds, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{getErr: domain.ErrNotFound}
			err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				tc.provider, tc.accountID, false, tc.cfg, tc.creds)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && repo.created != nil {
				t.Fatalf("wrote a misshapen row: %+v", repo.created)
			}
		})
	}
}

// TestCredentialValuesMustBeNonEmptyStrings: presence is not enough. Every dispatcher decodes
// these fields into string members, so a number or a whitespace-only value installs cleanly,
// exits 0, and fails at dispatch — far from the command that caused it.
func TestCredentialValuesMustBeNonEmptyStrings(t *testing.T) {
	for name, creds := range map[string]string{
		"numeric value":    `{"refresh_token":"rt","client_id":123,"client_secret":"cs","developer_token":"dt"}`,
		"whitespace value": `{"refresh_token":"rt","client_id":"   ","client_secret":"cs","developer_token":"dt"}`,
		"object value":     `{"refresh_token":"rt","client_id":{"a":1},"client_secret":"cs","developer_token":"dt"}`,
	} {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{getErr: domain.ErrNotFound}
			err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				model.ProviderGoogleAds, "8666746580", false, nil, []byte(creds))
			if err == nil || repo.created != nil {
				t.Fatalf("err = %v, created = %+v; want a refusal naming client_id", err, repo.created)
			}
			if !strings.Contains(err.Error(), "client_id") {
				t.Errorf("err = %v, want it to name client_id", err)
			}
		})
	}
}

// TestPaddedCredentialValuesAreRefusedRatherThanStored covers the same deferred failure as the
// test above, one step subtler. Checking `TrimSpace(v) == ""` proves a value is not BLANK; it
// says nothing about a value that merely has padding, and the ORIGINAL RawMessage — padding
// included — is what gets encrypted. `"access_token":" token "` therefore installed cleanly and
// exited 0, and LinkedIn's preflight refuses a padded token
// (internal/platform/linkedin/client.go), so the system row every unconnected project falls back
// to was one that every dispatch rejects — with nothing at install time to say so.
//
// Refused rather than trimmed: a credential is opaque here, and silently rewriting one would
// hide a truncated paste. The narrowing half is that padding INSIDE a value, and padding on a
// key this provider does not require, are both left alone — a secret's interior is not this
// command's business.
func TestPaddedCredentialValuesAreRefusedRatherThanStored(t *testing.T) {
	for name, tc := range map[string]struct {
		provider model.Provider
		creds    string
		wantErr  string
	}{
		"a trailing space on a linkedin token":  {model.ProviderLinkedInAds, `{"access_token":"tok "}`, "access_token"},
		"a leading space on a linkedin token":   {model.ProviderLinkedInAds, `{"access_token":" tok"}`, "access_token"},
		"a newline from a here-doc":             {model.ProviderLinkedInAds, `{"access_token":"tok\n"}`, "access_token"},
		"a tab on one of four google ads keys":  {model.ProviderGoogleAds, `{"refresh_token":"rt","client_id":"\tci","client_secret":"cs","developer_token":"dt"}`, "client_id"},
		"padding on two keys names them sorted": {model.ProviderGoogleAds, `{"refresh_token":"rt ","client_id":" ci","client_secret":"cs","developer_token":"dt"}`, "client_id, refresh_token"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{getErr: domain.ErrNotFound}
			err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				tc.provider, "", false, nil, []byte(tc.creds))
			if err == nil || repo.created != nil {
				t.Fatalf("err = %v, created = %+v; want a refusal: the padding is stored verbatim "+
					"and would be sent to the provider", err, repo.created)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to name %s", err, tc.wantErr)
			}
		})
	}

	// The narrowing half. Neither of these is padding this command can act on.
	for name, creds := range map[string]string{
		"whitespace inside a value":    `{"access_token":"to ken"}`,
		"padding on an unrequired key": `{"access_token":"tok","note":"  ignore me  "}`,
	} {
		t.Run(name+" installs", func(t *testing.T) {
			repo := &stubRepo{getErr: domain.ErrNotFound}
			if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				model.ProviderLinkedInAds, "509123456", false, map[string]string{"org_id": "123"}, []byte(creds)); err != nil {
				t.Fatalf("InstallSystemCredentials: %v — this is a credential the provider accepts", err)
			}
			if repo.created == nil {
				t.Fatal("no row written")
			}
		})
	}
}

// TestInstallRejectsConfigKeysTheProviderDoesNotStore: a -config key outside the provider's
// column set has nowhere to be written, so before this guard the command dropped it and exited
// 0 — telling the operator a routing setting was installed that nothing held. The keys below
// are all REAL keys on some other provider, which is what makes the mistake plausible and what
// a union-of-all-providers allow-list would have missed.
func TestInstallRejectsConfigKeysTheProviderDoesNotStore(t *testing.T) {
	gaCreds := []byte(`{"refresh_token":"rt","client_id":"ci","client_secret":"cs","developer_token":"dt"}`)
	liCreds := []byte(`{"access_token":"tok"}`)
	rdCreds := []byte(`{"client_id":"ci","client_secret":"cs","refresh_token":"rt"}`)
	cases := map[string]struct {
		provider model.Provider
		cfg      map[string]string
		creds    []byte
		wantErr  bool
	}{
		"linkedin key on google ads":      {model.ProviderGoogleAds, map[string]string{"org_id": "123"}, gaCreds, true},
		"google ads key on linkedin":      {model.ProviderLinkedInAds, map[string]string{"org_id": "1", "login_customer_id": "9746983954"}, liCreds, true},
		"typo in an otherwise real key":   {model.ProviderGoogleAds, map[string]string{"login_customerid": "9746983954"}, gaCreds, true},
		"any key on a provider with none": {model.ProviderRedditAds, map[string]string{"org_id": "123"}, rdCreds, true},
		"the provider's own key":          {model.ProviderGoogleAds, map[string]string{"login_customer_id": "9746983954"}, gaCreds, false},
		"no config at all":                {model.ProviderGoogleAds, nil, gaCreds, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{getErr: domain.ErrNotFound}
			err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				tc.provider, "", false, tc.cfg, tc.creds)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && repo.created != nil {
				t.Fatalf("wrote a row carrying a key it cannot store: %+v", repo.created)
			}
		})
	}
}

// TestRotationRefusesWhenTheRowMovedUnderIt pins the reason the write is version-gated: two
// bootstrap runs at once must end with one of them refusing, not with one run's account id
// paired to the other's credential. The message has to say nothing was written, because the
// operator's next move is to rerun a command they just watched fail.
func TestRotationRefusesWhenTheRowMovedUnderIt(t *testing.T) {
	repo := &stubRepo{
		row: &model.Connection{
			ProjectID: model.SystemProjectID, Provider: model.ProviderGoogleAds,
			AccountID: "8666746580", ProviderConfig: map[string]string{"login_customer_id": "999"},
			Version: 4, Status: model.StatusActive,
		},
		updErr: domain.ErrPreconditionFailed,
	}
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "9746983954", false, nil, []byte(goodCreds))
	if !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("err = %v, want ErrPreconditionFailed", err)
	}
	if !strings.Contains(err.Error(), "nothing was written") || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("err = %q, want it to say nothing was written and to rerun", err)
	}
}

// TestInstallRequiresAnAccountIDWhereNothingCanSupplyOneLater: credentials-first is a real
// lifecycle state only where BOTH halves of a completable lifecycle are present — the
// dispatcher can enumerate the accounts a credential reaches, AND the path that needs an
// account id refuses an empty one by NAMING the missing choice, so the operator is told to go
// and use that enumeration. Providers holding BOTH halves are in accountDiscoveryProviders and
// may be installed credentials-first; every other provider is refused an empty account id.
//
// The membership is deliberately described as a rule rather than a roster: this comment has been
// corrected three times by naming which providers sit on which side, and each correction was
// falsified by the next ticket that moved one. Read accountDiscoveryProviders for the current
// set. Microsoft is the one member worth calling out, because the rule does not explain it — it
// holds both halves as of LFXV2-3064 and is still excluded, which is a sequencing decision, not
// a capability gap.
//
// What this test actually exercises: LinkedIn's refusal directly (an empty account id must be
// rejected), with Google Ads and Meta as the ALLOWED cases. The other excluded providers are
// refused by the same map check but are not separately invoked here — the guard is one branch,
// so covering it once covers them, but do not read this comment as a claim of per-provider
// coverage.
//
// (This map gates the bootstrap CLI only; the public connection APIs are gated separately by
// Required("account_id") in design/connection.go.) That is the same installable-and-dead shape
// requiredConfigKeys already guards, applied to the one column that is not part of
// ProviderConfig.
//
// Meta is asserted as an ALLOWED case, not a refused one, and it is the case that keeps this
// test honest about the rule: it has had discovery since LFXV2-3062 and was still refused here
// until LFXV2-3061 supplied the tagging, so it is the only provider where the two halves ever
// came apart. If someone adds a provider to accountDiscoveryProviders on the strength of a
// discovery endpoint alone, this comment is the record of why that is not the bar.
//
// The last case is the one that makes this a check on the value WRITTEN rather than the flag
// TYPED: a rotation may omit -account-id, because the row keeps the id it already has.
func TestInstallRequiresAnAccountIDWhereNothingCanSupplyOneLater(t *testing.T) {
	metaCreds := []byte(`{"access_token":"tok","app_secret":"sec"}`)

	repo := &stubRepo{}
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderLinkedInAds, "", false, map[string]string{"org_id": "1"}, []byte(`{"access_token":"tok"}`))
	if err == nil || !strings.Contains(err.Error(), "requires -account-id") {
		t.Fatalf("creating an account-less linkedin row = %v, want a refusal naming -account-id", err)
	}
	if repo.created != nil || repo.updated != nil || repo.setCT != nil {
		t.Fatalf("refused and wrote anyway; calls = %v", repo.calls)
	}

	repo = &stubRepo{}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "", false, nil, []byte(goodCreds)); err != nil {
		t.Fatalf("google ads has account discovery, so credentials-first must still be legal: %v", err)
	}

	repo = &stubRepo{}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderMetaAds, "", false, map[string]string{"page_id": "1"}, metaCreds); err != nil {
		t.Fatalf("meta has discovery AND names the missing choice, so credentials-first must be legal: %v", err)
	}
	if repo.created == nil {
		t.Fatalf("an account-less meta row wrote nothing; calls = %v", repo.calls)
	}
	if repo.created.AccountID != "" {
		t.Fatalf("created.AccountID = %q, want it left empty for the picker to fill", repo.created.AccountID)
	}

	repo = &stubRepo{row: &model.Connection{
		ProjectID: model.SystemProjectID, Provider: model.ProviderMetaAds,
		AccountID: "act_1", ProviderConfig: map[string]string{"page_id": "111"},
		Version: 4, Status: model.StatusActive,
	}}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderMetaAds, "", false, nil, metaCreds); err != nil {
		t.Fatalf("a rotation may omit -account-id when the row already holds one: %v", err)
	}
	if repo.updated == nil || repo.updated.AccountID != "act_1" {
		t.Fatalf("rotation lost the row's account id: %+v", repo.updated)
	}
}

// TestInstallRefusesProvidersTheFallbackCannotServe pins the gate that keeps the installable
// set and the USABLE set the same. model.Provider.Valid() admits HubSpot, but the reserved-scope
// fallback (credsSource.systemConn) is classification-gated to paid ads, so a HubSpot system row
// would be written, reported as installed, and then resolved by nothing. An operator has no way
// to see that from the outside — the row is present and looks healthy — which is exactly why the
// refusal has to happen at install time.
//
// The paid-ads half of the assertion is what keeps this from being a HubSpot blocklist: the gate
// is a classification, so a provider added later is admitted only once it is classified.
func TestInstallRefusesProvidersTheFallbackCannotServe(t *testing.T) {
	hubspotCreds := []byte(`{"private_app_token":"tok"}`)

	repo := &stubRepo{}
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderHubSpot, "acct", false, nil, hubspotCreds)
	if err == nil || !strings.Contains(err.Error(), "not a paid-ads provider") {
		t.Fatalf("installing a hubspot system row = %v, want a refusal naming the classification", err)
	}
	if repo.created != nil || repo.updated != nil || repo.setCT != nil {
		t.Fatalf("refused and wrote anyway; calls = %v", repo.calls)
	}

	repo = &stubRepo{}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "123", false, nil, []byte(goodCreds)); err != nil {
		t.Fatalf("a paid-ads provider must still install: %v", err)
	}
	if repo.created == nil {
		t.Fatalf("paid-ads install wrote nothing; calls = %v", repo.calls)
	}
}

// gaRow is a system Google Ads row as installed: an account selected and one optional config
// column set. Google Ads is the provider used throughout because clearing the account id is a
// legal destination state for it (accountDiscoveryProviders), so these tests exercise the merge
// itself rather than tripping that guard first. Meta qualifies too as of LFXV2-3061, but it is
// not substituted in: login_customer_id is a Google Ads column and is what makes the
// clear-an-optional-column case below a real one.
func gaRow(accountID string, cfg map[string]string) *stubRepo {
	return &stubRepo{row: &model.Connection{
		ProjectID: model.SystemProjectID, Provider: model.ProviderGoogleAds,
		AccountID: accountID, ProviderConfig: cfg, Version: 4, Status: model.StatusActive,
	}}
}

// TestRotationCanClearAnObsoleteConfigColumn covers the half of the merge that preserve-by-
// default cannot express.
//
// Keeping every unmentioned column is right — a rotation should not have to restate the row —
// but on its own it makes an optional column PERMANENT. login_customer_id is the real case: it
// names the manager account a request is issued through, and when that path changes the old
// value is not merely stale, it is sent as a header on every dispatch. Nothing else can remove
// it, either: model.SystemProjectID is refused over HTTP (rejectSystemScope), so this installer
// is the scope's only writer.
func TestRotationCanClearAnObsoleteConfigColumn(t *testing.T) {
	repo := gaRow("8666746580", map[string]string{"login_customer_id": "999"})
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "", false, map[string]string{"login_customer_id": ""}, []byte(goodCreds)); err != nil {
		t.Fatalf("clear login_customer_id: %v", err)
	}
	if repo.updated == nil {
		t.Fatalf("nothing was written; calls = %v", repo.calls)
	}
	if v, ok := repo.updated.ProviderConfig["login_customer_id"]; ok {
		t.Fatalf("login_customer_id is still on the row as %q — a clear that leaves the column set is a no-op reported as success", v)
	}
	// The clear must not take the account with it: the two are independent instructions, and
	// a rotation that quietly returned the row to credentials-first would stop every dispatch.
	if repo.updated.AccountID != "8666746580" {
		t.Fatalf("account id = %q, want it untouched by a -config clear", repo.updated.AccountID)
	}
}

// TestClearingARequiredConfigColumnIsRefused: the clear is checked against the map about to be
// WRITTEN, which is what lets requireConfig see it at all. A clear that emptied org_id would
// leave a LinkedIn row that installs, reports success and refuses every campaign create — the
// same installable-and-dead shape requiredConfigKeys exists to prevent, arrived at by removal
// rather than by omission.
func TestClearingARequiredConfigColumnIsRefused(t *testing.T) {
	repo := &stubRepo{row: &model.Connection{
		ProjectID: model.SystemProjectID, Provider: model.ProviderLinkedInAds,
		AccountID: "12345", ProviderConfig: map[string]string{"org_id": "999"},
		Version: 4, Status: model.StatusActive,
	}}
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderLinkedInAds, "", false, map[string]string{"org_id": ""}, []byte(`{"access_token":"tok"}`))
	if err == nil || !strings.Contains(err.Error(), "requires -config org_id") {
		t.Fatalf("clearing org_id = %v, want a refusal naming the required key", err)
	}
	if repo.updated != nil || repo.setCT != nil {
		t.Fatalf("refused and wrote anyway; calls = %v", repo.calls)
	}
}

// TestClearAccountIDReturnsTheRowToCredentialsFirst is the account-id half of the same gap.
// Reaching credentials-first was expressible only at CREATE time, so a system account whose ad
// account was retired had no way back to the state its own installer documents.
func TestClearAccountIDReturnsTheRowToCredentialsFirst(t *testing.T) {
	repo := gaRow("8666746580", nil)
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "", true, nil, []byte(goodCreds)); err != nil {
		t.Fatalf("clear account id: %v", err)
	}
	if repo.updated == nil || repo.updated.AccountID != "" {
		t.Fatalf("updated = %+v, want the account selection removed", repo.updated)
	}

	// Without the flag the same call means KEEP — the distinction the tri-state exists for.
	repo = gaRow("8666746580", nil)
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "", false, nil, []byte(goodCreds)); err != nil {
		t.Fatalf("rotate without -account-id: %v", err)
	}
	if repo.updated == nil || repo.updated.AccountID != "8666746580" {
		t.Fatalf("updated = %+v, want the account id preserved when no clear was asked for", repo.updated)
	}
}

// TestClearAccountIDIsRefusedWhereNothingCanSupplyOneLater: the clear lands in the value
// requireAccountID already checks, so the rule that credentials-first is legal only where a
// dispatcher can discover the account holds for the removal path too, and holds automatically.
func TestClearAccountIDIsRefusedWhereNothingCanSupplyOneLater(t *testing.T) {
	repo := &stubRepo{row: &model.Connection{
		ProjectID: model.SystemProjectID, Provider: model.ProviderLinkedInAds,
		AccountID: "509123456", ProviderConfig: map[string]string{"org_id": "1"},
		Version: 4, Status: model.StatusActive,
	}}
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderLinkedInAds, "", true, nil, []byte(`{"access_token":"tok"}`))
	if err == nil || !strings.Contains(err.Error(), "requires -account-id") {
		t.Fatalf("clearing linkedin's account id = %v, want a refusal naming -account-id", err)
	}
	if repo.updated != nil || repo.setCT != nil {
		t.Fatalf("refused and wrote anyway; calls = %v", repo.calls)
	}
}

// TestClearIsRefusedWhenThereIsNoRowToClearFrom: a clear on a first install is refused rather
// than dropped. Nothing is there to remove, so obeying it and ignoring it produce the same row —
// which means accepting it reports success for an instruction that never ran, and the likely
// cause is an operator who believed they were rotating a row that is not there.
func TestClearIsRefusedWhenThereIsNoRowToClearFrom(t *testing.T) {
	for name, tc := range map[string]struct {
		clearAccount bool
		cfg          map[string]string
		want         string
	}{
		"account id": {clearAccount: true, want: "nothing to clear"},
		"config key": {cfg: map[string]string{"login_customer_id": ""}, want: "asks to clear a column"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{}
			err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				model.ProviderGoogleAds, "", tc.clearAccount, tc.cfg, []byte(goodCreds))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("clear on a first install = %v, want a refusal containing %q", err, tc.want)
			}
			if repo.created != nil {
				t.Fatalf("refused and created anyway: %+v", repo.created)
			}
		})
	}
}

// TestClearAccountIDAndAccountIDTogetherIsRefused: the two flags ask for opposite things, and
// silently letting either win would make the outcome depend on an ordering nobody wrote down.
func TestClearAccountIDAndAccountIDTogetherIsRefused(t *testing.T) {
	repo := gaRow("8666746580", nil)
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "9746983954", true, nil, []byte(goodCreds))
	if err == nil || !strings.Contains(err.Error(), "opposite things") {
		t.Fatalf("both flags = %v, want a refusal", err)
	}
	if repo.calls != nil {
		t.Fatalf("refused after touching the repository; calls = %v", repo.calls)
	}
}

// TestClearedValueIsNotHeldToAValueShape: an empty value is an instruction, not a value, so it
// must not be measured against the provider's numeric-id pattern. Without the skip in
// requireShapes, every clear failed with a shape complaint about a value nobody supplied.
func TestClearedValueIsNotHeldToAValueShape(t *testing.T) {
	repo := gaRow("8666746580", map[string]string{"login_customer_id": "999"})
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "", false, map[string]string{"login_customer_id": ""}, []byte(goodCreds)); err != nil {
		t.Fatalf("clear rejected as a malformed value: %v", err)
	}
}

// A Reddit system row without a conversion pixel is installable and DEAD, which is exactly
// what requiredConfigKeys exists to prevent.
//
// The blast radius is what makes it worth its own test rather than a table row. The LF
// system row is the FALLBACK for every project that has connected no Reddit account of its
// own, and the Reddit client refuses EVERY campaign create without a pixel — not only the
// "conversions" objective its API docs describe. So one pixel-less install silently refuses
// paid creates for every fallback project, and the failure surfaces per-project at dispatch
// rather than once, loudly, at install time.
func TestInstallRefusesRedditWithoutAConversionPixel(t *testing.T) {
	rdCreds := []byte(`{"client_id":"ci","client_secret":"cs","refresh_token":"rt"}`)
	repo := &stubRepo{getErr: domain.ErrNotFound}
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderRedditAds, "t2_gv9wtbfa", false, nil, rdCreds)
	if err == nil {
		t.Fatal("installed a Reddit system row with no conversion pixel; every project falling back to it would be refused at dispatch")
	}
	if !strings.Contains(err.Error(), "conversion_pixel_id") {
		t.Errorf("error %q does not name the missing key, so the operator cannot act on it", err)
	}
	// Nothing may be written: a row that exists and cannot dispatch is worse than no row,
	// because the fallback probe finds it and stops looking.
	if repo.created != nil {
		t.Errorf("wrote a dead row despite refusing: %+v", repo.created)
	}
}

// The pixel satisfies the requirement wherever it comes from — including a rotation that
// omits the flag but keeps the value already on the row. requireConfig checks the map about
// to be WRITTEN, not the flags as typed, and this pins that distinction for Reddit.
func TestInstallAcceptsRedditWithAConversionPixel(t *testing.T) {
	rdCreds := []byte(`{"client_id":"ci","client_secret":"cs","refresh_token":"rt"}`)
	repo := &stubRepo{getErr: domain.ErrNotFound}
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderRedditAds, "t2_gv9wtbfa", false,
		map[string]string{"conversion_pixel_id": "a2_pixel"}, rdCreds)
	if err != nil {
		t.Fatalf("InstallSystemCredentials: %v", err)
	}
	if repo.created == nil {
		t.Fatal("no row was written")
	}
	if got := repo.created.ProviderConfig["conversion_pixel_id"]; got != "a2_pixel" {
		t.Errorf("stored pixel = %q, want a2_pixel", got)
	}
}

// A ROTATION of a pre-migration Reddit row must be refused too, not just a creation.
//
// The pixel joined requiredConfigKeys with migration 000025, so rows written before it
// carry no conversion_pixel_id. mergeConfig returns nil when -config is omitted, and the
// rotation branch used to gate requireConfig on that nil — so exactly this row could take
// fresh credentials, report success, and remain unusable for every project that falls back
// to it, surfacing per-project at dispatch instead of once here. The sibling test above
// covers CREATION only (it stubs getErr: ErrNotFound), which is why this gap survived.
func TestRotateRefusesRedditRowMissingTheConversionPixel(t *testing.T) {
	rdCreds := []byte(`{"client_id":"ci2","client_secret":"cs2","refresh_token":"rt2"}`)
	repo := &stubRepo{row: &model.Connection{
		ProjectID:      model.SystemProjectID,
		Provider:       model.ProviderRedditAds,
		AccountID:      "t2_gv9wtbfa",
		ProviderConfig: map[string]string{}, // pre-000025: no pixel
	}}

	// No -config supplied: the rotation carries credentials only.
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderRedditAds, "", false, nil, rdCreds)
	if err == nil {
		t.Fatal("rotated a pixel-less Reddit system row; it reports success and stays unusable for every fallback project")
	}
	if !strings.Contains(err.Error(), "conversion_pixel_id") {
		t.Errorf("error %q does not name the missing key, so the operator cannot act on it", err)
	}
	if repo.updated != nil {
		t.Errorf("wrote new credentials onto a row that still cannot dispatch: %+v", repo.updated)
	}
}

// The mirror case: a rotation that omits -config but whose EXISTING row already carries the
// pixel must succeed. This is the behaviour the older test's comment claimed to cover but
// did not — it exercised creation. Without this, the fix above could over-reject and break
// every ordinary credential rotation.
func TestRotateAcceptsRedditRowThatAlreadyHasThePixel(t *testing.T) {
	rdCreds := []byte(`{"client_id":"ci2","client_secret":"cs2","refresh_token":"rt2"}`)
	repo := &stubRepo{row: &model.Connection{
		ProjectID:      model.SystemProjectID,
		Provider:       model.ProviderRedditAds,
		AccountID:      "t2_gv9wtbfa",
		ProviderConfig: map[string]string{"conversion_pixel_id": "a2_pixel"},
	}}

	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderRedditAds, "", false, nil, rdCreds); err != nil {
		t.Fatalf("a rotation of a row that already carries the pixel must succeed: %v", err)
	}
	if repo.updated == nil {
		t.Fatal("no row was updated")
	}
	// The pixel must survive a rotation that did not mention it.
	if got := repo.updated.ProviderConfig["conversion_pixel_id"]; got != "a2_pixel" {
		t.Errorf("rotation dropped the existing pixel: got %q, want a2_pixel", got)
	}
}

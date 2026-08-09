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
		model.ProviderGoogleAds, "8666746580", nil, []byte(goodCreds)); err != nil {
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
		model.ProviderGoogleAds, "9746983954", nil, []byte(goodCreds)); err != nil {
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
			AccountID: "act_1", ProviderConfig: map[string]string{"page_id": "111", "app_id": "a1"},
			Version: 4, Status: model.StatusActive,
		}}
	}
	metaCreds := []byte(`{"access_token":"tok","app_secret":"sec"}`)

	repo := metaRow()
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderMetaAds, "", map[string]string{"page_id": "222"}, metaCreds); err != nil {
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
		model.ProviderMetaAds, "", nil, metaCreds); err != nil {
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
				model.ProviderGoogleAds, "", nil, []byte(goodCreds)); err == nil {
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
		// omission is ALLOWED is requireAccountID's question, and for Meta the answer is no —
		// see TestInstallRequiresAnAccountIDWhereNothingCanSupplyOneLater. Google Ads is used
		// so this case still tests only what it claims to.
		"omitted account id is legal": {model.ProviderGoogleAds, "", map[string]string{"login_customer_id": "1"}, gaCreds, false},

		// Runtime-validator providers. Each of these exited 0 before the rule was added.
		"google ads account id not numeric":       {model.ProviderGoogleAds, "foo", nil, gaCreds, true},
		"google ads login customer id has dashes": {model.ProviderGoogleAds, "8666746580", map[string]string{"login_customer_id": "974-698-3954"}, gaCreds, true},
		"google ads values in shape":              {model.ProviderGoogleAds, "8666746580", map[string]string{"login_customer_id": "9746983954"}, gaCreds, false},
		"microsoft customer id not numeric":       {model.ProviderMicrosoftAds, "1234", map[string]string{"customer_id": "cus-9"}, msCreds, true},
		"microsoft values in shape":               {model.ProviderMicrosoftAds, "1234", map[string]string{"customer_id": "9"}, msCreds, false},
		"reddit account id with a path separator": {model.ProviderRedditAds, "t2_gv9../x", nil, rdCreds, true},
		"reddit account id in shape":              {model.ProviderRedditAds, "t2_gv9wtbfa", nil, rdCreds, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &stubRepo{getErr: domain.ErrNotFound}
			err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
				tc.provider, tc.accountID, tc.cfg, tc.creds)
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
				model.ProviderGoogleAds, "8666746580", nil, []byte(creds))
			if err == nil || repo.created != nil {
				t.Fatalf("err = %v, created = %+v; want a refusal naming client_id", err, repo.created)
			}
			if !strings.Contains(err.Error(), "client_id") {
				t.Errorf("err = %v, want it to name client_id", err)
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
				tc.provider, "", tc.cfg, tc.creds)
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
		model.ProviderGoogleAds, "9746983954", nil, []byte(goodCreds))
	if !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("err = %v, want ErrPreconditionFailed", err)
	}
	if !strings.Contains(err.Error(), "nothing was written") || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("err = %q, want it to say nothing was written and to rerun", err)
	}
}

// TestInstallRequiresAnAccountIDWhereNothingCanSupplyOneLater: credentials-first is a real
// lifecycle state only where the dispatcher can enumerate the accounts a credential reaches.
// Google Ads has that endpoint; the rest do not, and each of their adapters refuses an empty
// account id — so an account-less Meta or LinkedIn system row installs, reports success, and
// then fails every dispatch with nothing an operator can do to complete it. That is the same
// installable-and-dead shape requiredConfigKeys already guards, applied to the one column that
// is not part of ProviderConfig.
//
// The third case is the one that makes this a check on the value WRITTEN rather than the flag
// TYPED: a rotation may omit -account-id, because the row keeps the id it already has.
func TestInstallRequiresAnAccountIDWhereNothingCanSupplyOneLater(t *testing.T) {
	metaCreds := []byte(`{"access_token":"tok","app_secret":"sec"}`)

	repo := &stubRepo{}
	err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderMetaAds, "", map[string]string{"page_id": "1"}, metaCreds)
	if err == nil || !strings.Contains(err.Error(), "requires -account-id") {
		t.Fatalf("creating an account-less meta row = %v, want a refusal naming -account-id", err)
	}
	if repo.created != nil || repo.updated != nil || repo.setCT != nil {
		t.Fatalf("refused and wrote anyway; calls = %v", repo.calls)
	}

	repo = &stubRepo{}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "", nil, []byte(goodCreds)); err != nil {
		t.Fatalf("google ads has account discovery, so credentials-first must still be legal: %v", err)
	}

	repo = &stubRepo{row: &model.Connection{
		ProjectID: model.SystemProjectID, Provider: model.ProviderMetaAds,
		AccountID: "act_1", ProviderConfig: map[string]string{"page_id": "111"},
		Version: 4, Status: model.StatusActive,
	}}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderMetaAds, "", nil, metaCreds); err != nil {
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
		model.ProviderHubSpot, "acct", nil, hubspotCreds)
	if err == nil || !strings.Contains(err.Error(), "not a paid-ads provider") {
		t.Fatalf("installing a hubspot system row = %v, want a refusal naming the classification", err)
	}
	if repo.created != nil || repo.updated != nil || repo.setCT != nil {
		t.Fatalf("refused and wrote anyway; calls = %v", repo.calls)
	}

	repo = &stubRepo{}
	if err := InstallSystemCredentials(context.Background(), repo, fakeEnc{},
		model.ProviderGoogleAds, "123", nil, []byte(goodCreds)); err != nil {
		t.Fatalf("a paid-ads provider must still install: %v", err)
	}
	if repo.created == nil {
		t.Fatalf("paid-ads install wrote nothing; calls = %v", repo.calls)
	}
}

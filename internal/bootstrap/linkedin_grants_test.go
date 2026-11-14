// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// grantWriter records the one replace a run may issue.
type grantWriter struct {
	calls  int
	grants []model.LinkedInAccountGrant
	ver    int64
	err    error
}

func (w *grantWriter) ReplaceLinkedInAccountGrants(_ context.Context, projectID string, g []model.LinkedInAccountGrant, _ *model.Actor, v int64) (*model.Connection, error) {
	w.calls++
	if projectID != model.SystemProjectID {
		return nil, errors.New("grants written outside the system scope")
	}
	w.grants, w.ver = g, v
	if w.err != nil {
		return nil, w.err
	}
	return &model.Connection{}, nil
}

func linkedInSystemRow() *model.Connection {
	return &model.Connection{
		ProjectID: model.SystemProjectID, Provider: model.ProviderLinkedInAds,
		AccountID: "538170226", ProviderConfig: map[string]string{"org_id": "208777"},
		EncryptedCredentials: []byte(`enc:{"AccessToken":"tok"}`), Version: 7,
	}
}

type verifyCall struct{ plaintext, account, org string }

func recordingVerifier(calls *[]verifyCall, fail map[string]error) LinkedInGrantVerifier {
	return func(_ context.Context, p []byte, a, o string) error {
		*calls = append(*calls, verifyCall{string(p), a, o})
		return fail[a]
	}
}

// TestInstallLinkedInGrantsVerifiesEveryGrantThenReplacesTheWholeSet: every pairing is checked
// with the row's own DECRYPTED credential before anything is written, and the write is one
// replace gated on the version that was read, in account order.
func TestInstallLinkedInGrantsVerifiesEveryGrantThenReplacesTheWholeSet(t *testing.T) {
	repo := &stubRepo{row: linkedInSystemRow()}
	w := &grantWriter{}
	var calls []verifyCall
	grants := []model.LinkedInAccountGrant{
		{AccountID: "509430019", OrgID: "208777", Label: "LF Events"},
		{AccountID: "500928401", OrgID: "12893459", Label: "CNCF"},
	}
	if err := InstallLinkedInGrants(context.Background(), repo, w, fakeEnc{}, recordingVerifier(&calls, nil), grants, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(calls) != 2 || calls[0].plaintext != `{"AccessToken":"tok"}` {
		t.Fatalf("verifier calls = %+v; want both grants verified with the decrypted credential", calls)
	}
	want := []model.LinkedInAccountGrant{grants[1], grants[0]}
	if w.calls != 1 || w.ver != 7 || !reflect.DeepEqual(w.grants, want) {
		t.Fatalf("replace = %d calls, version %d, grants %+v; want one replace at version 7 of %+v", w.calls, w.ver, w.grants, want)
	}
}

// TestInstallLinkedInGrantsWritesNothingWhenAnyGrantFails covers every refusal that must leave
// the table untouched: a pairing LinkedIn does not confirm (inconclusive included), a malformed
// or duplicated id, a contradiction with the row's own pair, and a missing system row.
func TestInstallLinkedInGrantsWritesNothingWhenAnyGrantFails(t *testing.T) {
	good := model.LinkedInAccountGrant{AccountID: "509430019", OrgID: "208777"}
	for name, tc := range map[string]struct {
		row    *model.Connection
		grants []model.LinkedInAccountGrant
		fail   map[string]error
		clear  bool
		want   string
	}{
		"verification fails":         {grants: []model.LinkedInAccountGrant{good, {AccountID: "500928401", OrgID: "1"}}, fail: map[string]error{"500928401": errors.New("org mismatch")}, want: "did not verify"},
		"non-numeric account":        {grants: []model.LinkedInAccountGrant{{AccountID: "urn:li:sponsoredAccount:1", OrgID: "1"}}, want: "bare LinkedIn number"},
		"non-numeric org":            {grants: []model.LinkedInAccountGrant{{AccountID: "1", OrgID: "acme"}}, want: "bare LinkedIn number"},
		"account granted twice":      {grants: []model.LinkedInAccountGrant{good, {AccountID: "509430019", OrgID: "9"}}, want: "more than once"},
		"default account other org":  {grants: []model.LinkedInAccountGrant{{AccountID: "538170226", OrgID: "9"}}, want: "row's org_id is 208777"},
		"default pair repeated":      {grants: []model.LinkedInAccountGrant{{AccountID: "538170226", OrgID: "208777"}}, want: "always addressable"},
		"control character in label": {grants: []model.LinkedInAccountGrant{{AccountID: "1", OrgID: "2", Label: "a\nb"}}, want: "control character"},
		"label too long":             {grants: []model.LinkedInAccountGrant{{AccountID: "1", OrgID: "2", Label: strings.Repeat("x", 201)}}, want: "longer than 200"},
		"no system row":              {row: nil, grants: []model.LinkedInAccountGrant{good}, want: "install it first"},
		"clear and grants together":  {grants: []model.LinkedInAccountGrant{good}, clear: true, want: "opposite things"},
		"nothing asked":              {want: "no linkedin grants"},
	} {
		t.Run(name, func(t *testing.T) {
			row := linkedInSystemRow()
			if name == "no system row" {
				row = tc.row
			}
			w := &grantWriter{}
			var calls []verifyCall
			err := InstallLinkedInGrants(context.Background(), &stubRepo{row: row}, w, fakeEnc{}, recordingVerifier(&calls, tc.fail), tc.grants, tc.clear)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if w.calls != 0 {
				t.Fatalf("a refused install wrote grants: %+v", w.grants)
			}
		})
	}
}

// TestInstallLinkedInGrantsClearRemovesEverythingWithoutVerifying: a clear has nothing to verify
// and writes an empty set; a lost version race is reported as rerunnable.
func TestInstallLinkedInGrantsClearRemovesEverythingWithoutVerifying(t *testing.T) {
	w := &grantWriter{}
	if err := InstallLinkedInGrants(context.Background(), &stubRepo{row: linkedInSystemRow()}, w, fakeEnc{}, nil, nil, true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if w.calls != 1 || len(w.grants) != 0 {
		t.Fatalf("clear wrote %d calls, %+v; want one empty replace", w.calls, w.grants)
	}
	w = &grantWriter{err: domain.ErrPreconditionFailed}
	err := InstallLinkedInGrants(context.Background(), &stubRepo{row: linkedInSystemRow()}, w, fakeEnc{}, nil, nil, true)
	if !errors.Is(err, domain.ErrPreconditionFailed) || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("lost race err = %v, want a rerunnable precondition failure", err)
	}
}

func TestParseLinkedInGrantKeepsColonsInTheLabel(t *testing.T) {
	g, err := ParseLinkedInGrant(" 500928401 : 12893459 : CNCF: Cloud Native ")
	if err != nil {
		t.Fatal(err)
	}
	if g != (model.LinkedInAccountGrant{AccountID: "500928401", OrgID: "12893459", Label: "CNCF: Cloud Native"}) {
		t.Fatalf("parsed %+v", g)
	}
	if _, err := ParseLinkedInGrant("500928401"); err == nil {
		t.Fatal("a grant with no org parsed")
	}
}

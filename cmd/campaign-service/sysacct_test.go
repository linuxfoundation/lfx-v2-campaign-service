// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestRunCommandRefusesAnUnknownCommandInsteadOfServing pins the dispatch decision main() makes
// before any serving setup.
//
// Matching only the exact subcommand name and falling through was the defect: flag.Parse stops
// at the first positional argument, so `campaign-service bootstrap-system-acount -provider
// google-ads` parsed without complaint and started the HTTP server. The Kubernetes Job meant to
// install credentials then ran as a second, healthy, idle replica — no credential installed, no
// error logged, and no exit code to fail the Job on.
//
// The scan stops at args[0] deliberately. A subcommand has to come first, and looking further
// would mistake a FLAG VALUE for one: `-p 8080` puts a bare 8080 in the argument list, and
// rejecting that would break ordinary server startup — a far worse failure than the one being
// fixed.
func TestRunCommandRefusesAnUnknownCommandInsteadOfServing(t *testing.T) {
	for name, tc := range map[string]struct {
		args        []string
		wantHandled bool
		wantCode    int
		wantStderr  string
	}{
		"no arguments at all":       {args: nil},
		"server flags only":         {args: []string{"-p", "8080", "-d"}},
		"a flag value that is bare": {args: []string{"-bind", "0.0.0.0"}},
		"a typo of the subcommand":  {args: []string{"bootstrap-system-acount"}, wantHandled: true, wantCode: 2, wantStderr: `unknown command "bootstrap-system-acount"`},
		"some other word":           {args: []string{"serve"}, wantHandled: true, wantCode: 2, wantStderr: `unknown command "serve"`},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr strings.Builder
			handled, code := runCommand(tc.args, &stderr)
			if handled != tc.wantHandled || code != tc.wantCode {
				t.Fatalf("runCommand(%q) = (handled %v, code %d), want (%v, %d)",
					tc.args, handled, code, tc.wantHandled, tc.wantCode)
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// TestParseProviderConfigDistinguishesAClearFromAMalformedEntry: `key=` with no value is how an
// operator removes a config column. It reads like a typo, which is why it was rejected at first,
// but this installer is the system scope's only writer — rejectSystemScope blocks HTTP — so
// without it an obsolete optional column could not be removed by anything, from anywhere.
//
// A missing `=` and an empty KEY stay errors: neither names a column, so neither can be either
// a set or a clear.
func TestParseProviderConfigDistinguishesAClearFromAMalformedEntry(t *testing.T) {
	got, err := parseProviderConfig("org_id=123, login_customer_id=, page_id=  ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]string{"org_id": "123", "login_customer_id": "", "page_id": ""}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Fatalf("%s = %q (present %v), want %q", k, g, ok, v)
		}
	}

	for _, bad := range []string{"org_id", "=123", "  =  "} {
		if _, err := parseProviderConfig(bad); err == nil {
			t.Fatalf("parseProviderConfig(%q) = nil error, want a refusal: it names no column", bad)
		} else if !strings.Contains(err.Error(), "not key=value") {
			t.Fatalf("parseProviderConfig(%q) error = %v, want it to say what the form is", bad, err)
		}
	}
}

// TestRunSysacctBootstrapRejectsResidualArguments pins the residual-argument check.
//
// It matters because of how flag.Parse fails: it STOPS at the first non-flag word and returns
// no error, leaving everything after it in Args(). A typo mid-command therefore does not fail —
// it silently discards every flag that follows. `-provider google-ads typo -account-id 123`
// would install a credentials-first row, or on a rotation keep the account id the operator
// thought they were replacing, on the command that installs the credentials paid campaigns are
// dispatched with.
//
// The case with a flag AFTER the positional is the one worth having: it fails identically
// today, but it is the shape that silently dropped a flag, so a future edit that accepted
// stray words while keeping the check on flags-only invocations would still be caught here.
func TestRunSysacctBootstrapRejectsResidualArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"a bare stray word":         {"-provider", "google-ads", "typo"},
		"a flag after a positional": {"-provider", "google-ads", "typo", "-account-id", "123"},
	} {
		t.Run(name, func(t *testing.T) {
			err := runSysacctBootstrap(args)
			if err == nil {
				t.Fatal("runSysacctBootstrap accepted a stray argument; flags after it are silently ignored")
			}
			if !strings.Contains(err.Error(), "unexpected argument") {
				t.Errorf("error = %v, want it to name the stray argument rather than fail later "+
					"for a missing credential", err)
			}
		})
	}
}

// TestSysacctUsageOffersOnlyInstallableProviders keeps the -provider usage error in step with
// what InstallSystemAccount accepts. A HubSpot system row is refused there (the reserved-scope
// fallback resolves paid ads only), so naming it here would send an operator to a value that
// cannot succeed — the usage message is the only place they learn the valid set.
func TestSysacctUsageOffersOnlyInstallableProviders(t *testing.T) {
	err := runSysacctBootstrap(nil)
	if err == nil {
		t.Fatal("runSysacctBootstrap(nil) succeeded; -provider is required")
	}
	if strings.Contains(err.Error(), string(model.ProviderHubSpot)) {
		t.Errorf("error = %v, but a hubspot system row is refused further down", err)
	}
	if !strings.Contains(err.Error(), string(model.ProviderGoogleAds)) {
		t.Errorf("error = %v, want it to offer the paid-ads providers", err)
	}
	for _, p := range paidAdsProviders() {
		if !p.IsPaidAds() {
			t.Errorf("paidAdsProviders included %s, which is not a paid-ads provider", p)
		}
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
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

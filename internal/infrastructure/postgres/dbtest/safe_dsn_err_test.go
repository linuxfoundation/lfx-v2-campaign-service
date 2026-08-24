// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"errors"
	"net/url"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/redact"
)

// safeDSNErr renders an error that may carry the DSN into a form safe to print.
//
// The DSN is the one value in this file that must never reach a log. Locally it is a
// peer-auth URL with nothing in it, but in CI TEST_DATABASE_URL authenticates over TCP
// with a `user:password@` segment, and CI logs are visible to more people than the secret
// store is. Reporting the env-var NAME instead of its value is the discipline used
// throughout this file -- and on these two paths that discipline is defeated by the error
// itself, which repeats the input it was given.
//
// Two error shapes do it. `url.Parse` fails with a `*url.Error`, whose Error() is
// `fmt.Sprintf("%s %q: %s", Op, URL, Err)` -- the whole raw URL, credentials included.
// `migrate.NewWithSourceInstance` reaches `database.Open`, which parses the URL and wraps
// that same `*url.Error` as "failed to open database: parse %q: ...". Both were verified
// against a password-bearing DSN, and both leaked it in full.
//
// So the cause is UNWRAPPED rather than formatted: `ue.Err` is the diagnosis ("invalid URL
// escape %q", "missing ']' in host") with the URL left behind, which is the part worth
// printing. redact.URLUserinfo then covers anything else in the chain that embedded the
// value, because it is string-based and needs no parse to succeed -- see its package doc,
// which names this exact path. Callers pass the error, never the DSN.
func safeDSNErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return redact.URLUserinfo(ue.Err.Error())
	}
	return redact.URLUserinfo(err.Error())
}

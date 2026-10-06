// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package redact

import "testing"

// The exhaustive cases live in internal/dispatch/creds_test.go, which exercises these through
// the dispatch wrappers. This mirrors a representative set so the package pins its own
// contract: scheme+host only, fail closed on userinfo or a malformed http(s) value.
func TestSnapshotURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"https://example.com/reg?token=SECRET&x=1", "https://example.com"},
		{"https://example.com/p#frag-SECRET", "https://example.com"},
		{"https://example.org/reset/SECRET", "https://example.org"},
		{"https://[fe80::1%25eth0]/reg?t=SECRET", "https://[fe80::1%25eth0]"},
		{"https:///reset/SECRET", ""},
		{"http:/reset/SECRET", ""},
		{"https://user:pass@example.com/x?token=SECRET", ""}, // secretlint-disable-line -- fixture asserting userinfo fails closed
		{"t3_abc123", "t3_abc123"},
		{"not a url?token=SECRET", "not a url"},
		{"example.org/reset/SECRET", "example.org"},
		// Any scheme with an authority reduces to scheme+host, or is dropped with no host.
		{"ftp://[2001:db8::1]/reset/SECRET?token=SECRET", "ftp://[2001:db8::1]"},
		{"file:///private/RESET_TOKEN", ""},
	} {
		if got := SnapshotURL(tc.in); got != tc.want {
			t.Errorf("SnapshotURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSnapshotText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"no url", "Join us at KubeCon!", "Join us at KubeCon!"},
		{"query", "Register https://events.lf.org/reg?access_token=SECRET now", "Register https://events.lf.org now"},
		{"fragment", "https://events.lf.org/reg#token-SECRET", "https://events.lf.org"},
		{"path only", "Details at https://events.lf.org/kubecon/register", "Details at https://events.lf.org"},
		{"userinfo", "link: https://user:pass@example.com/x?token=SECRET", "link: "}, // secretlint-disable-line -- fixture asserting userinfo fails closed
		{"ipv6 host", "see https://[2001:db8::1]/reg?ticket=SECRET now", "see https://[2001:db8::1] now"},
		{"schemeless query", "go to www.example.org/r?ticket=SECRET", "go to www.example.org"},
		{"schemeless path", "see example.org/reset/SECRET now", "see example.org now"},
		{"schemeless userinfo", "bob:pw@a.example", ""}, // secretlint-disable-line -- fixture asserting userinfo is dropped
		{"clock is not userinfo", "keynote 14:00@events.example", "keynote 14:00@events.example"},
		{"non-http scheme", "see ftp://[2001:db8::1]/reset/SECRET?token=SECRET now", "see ftp://[2001:db8::1] now"},
		{"host-less file url", "see file:///private/RESET_TOKEN now", "see  now"},
		{"userinfo before ipv6 host", "x https://bob:" + "pw@[2001:db8::1]/reset/SECRET_PATH?token=SECRET_QUERY y", "x  y"},
		{"sub-delim username", "x admin!:" + "pw@events.example/reset/TOKEN y", "x  y"},
	} {
		if got := SnapshotText(tc.in); got != tc.want {
			t.Errorf("%s: SnapshotText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

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
		{"sub-delim inside username", "x a(b:" + "pw@host.example y", "x  y"},
		{"parenthesised clock", "Keynote (14:00@main.stage)", "Keynote (14:00@main.stage)"},
		{"starred clock", "*9:30@main.stage*", "*9:30@main.stage*"},
		{"quoted clock", "'14:00@main.stage'", "'14:00@main.stage'"},
		{"comma clock", "Mon,9:30@main.stage", "Mon,9:30@main.stage"},
		{"sub-delims-only username", "x !:" + "pw@host.example y", "x  y"},
		{"dollar username", "x $$:" + "pw@host.example y", "x  y"},
		{"plus-suffixed numeric pair", "x alice+2024:" + "1234@ops.example y", "x  y"},
		{"plus clock is not exempt", "x alice+9:" + "30@ops.example y", "x  y"},
		{"comma then a non-clock number", "x a,2024:" + "1234@h.example y", "x  y"},
		{"hour 24 is not a clock", "x Mon,24:" + "00@main.stage y", "x  y"},
		{"minute 60 is not a clock", "x Mon,9:" + "60@main.stage y", "x  y"},
		{"one-digit minute after punctuation is not a clock", "x Mon,9:" + "5@main.stage y", "x  y"},
		{"last minute of the day is a clock", "Mon,23:59@main.stage", "Mon,23:59@main.stage"},
		{"two-digit score is exempt", "finals 3:4@events.example", "finals 3:4@events.example"},
		{"numeric user id and pin", "x 2024:" + "1234@ops.example y", "x  y"},
		{"long numeric pair", "x 12345:" + "67890@host.example y", "x  y"},
	} {
		if got := SnapshotText(tc.in); got != tc.want {
			t.Errorf("%s: SnapshotText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestUsernameIsClock pins the exemption's edges directly: these are the exact line between
// "clock" and "credential persisted in config_snapshot / published on X".
func TestUsernameIsClock(t *testing.T) {
	for _, tc := range []struct {
		user, pass string
		want       bool
	}{
		{"14", "00", true}, {"9", "30", true}, {"3", "4", true}, {"99", "99", true},
		{"2024", "1234", false}, {"12345", "67890", false}, {"123", "4", false}, {"1", "234", false},
		{"Mon,23", "59", true}, {"Mon,0", "00", true},
		{"Mon,24", "00", false}, {"Mon,9", "60", false}, {"Mon,9", "5", false}, {"Mon,123", "00", false},
		{"alice+9", "30", false}, {"a!9", "30", false}, {"", "00", false}, {"14", "", false},
	} {
		if got := UsernameIsClock(tc.user, tc.pass); got != tc.want {
			t.Errorf("UsernameIsClock(%q, %q) = %v, want %v", tc.user, tc.pass, got, tc.want)
		}
	}
}

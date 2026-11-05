// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// #494: WrapQuotedPreflightRefusal keeps the wrapped error's text as Error() and renders every
// quoted segment through CallerValue for the operator.
func TestWrapQuotedPreflightRefusal(t *testing.T) {
	if WrapQuotedPreflightRefusal(nil) != nil {
		t.Fatal("a nil error must stay nil")
	}

	sentinel := errors.New("sentinel")
	inner := fmt.Errorf("google-ads headline %q is too wide; geo %q; image %q; ad group %q: %w",
		"Join BackstageCon now", "US", "https://cdn.example.org/reset/SECRET-494/a.png", `say "hi"`, sentinel)
	err := WrapQuotedPreflightRefusal(inner)

	if err.Error() != inner.Error() {
		t.Errorf("Error() = %q, want the wrapped text unchanged %q", err.Error(), inner.Error())
	}
	if !errors.Is(err, sentinel) {
		t.Error("the cause chain must stay reachable")
	}
	msg, ok := AsPreflightRefusal(fmt.Errorf("dispatch: %w", err))
	if !ok {
		t.Fatal("AsPreflightRefusal must find the refusal through a wrapper")
	}
	want := `google-ads headline (value withheld) is too wide; geo "US"; image (value withheld); ad group (value withheld): sentinel`
	if msg != want {
		t.Errorf("operator message = %q\nwant               %q", msg, want)
	}
	for _, leak := range []string{"SECRET", "://", "BackstageCon", "hi"} {
		if strings.Contains(msg, leak) {
			t.Errorf("operator message %q contains %q", msg, leak)
		}
	}
}

// The other constructors are unchanged: their message is the operator message.
func TestPreflightRefusalf_OperatorMessageIsTheMessage(t *testing.T) {
	err := PreflightRefusalf("budget %s is too small", CallerValue("5"))
	msg, ok := AsPreflightRefusal(err)
	if !ok || msg != err.Error() || msg != `budget "5" is too small` {
		t.Errorf("AsPreflightRefusal = %q, %v; Error() = %q", msg, ok, err.Error())
	}
}

func TestQuotedCallerValues(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`no quotes here`, `no quotes here`},
		{`code "US" and id "1234567890"`, `code "US" and id "1234567890"`},
		{`date "2099-03-01"`, `date "2099-03-01"`},
		{`text "two words"`, `text (value withheld)`},
		{`path "/reset/SECRET"`, `path (value withheld)`},
		{`escaped "a\"b"`, `escaped (value withheld)`},
		{`an opaque token "ghp_SECRET208abcdefghijklmnopqrstuvwxyz0123"`, `an opaque token (value withheld)`}, // secretlint-disable-line -- intentional fake token, asserting it is withheld
		// A value that merely ends in "…" is not a capForError cut (that only happens past 100
		// bytes), and "…" is outside CallerValue's alphabet, so it is withheld.
		{`id "abcdef…"`, `id (value withheld)`},
		// %q renders a double-quote rune as '"'. Its lone quote must not pair with the next
		// one, or the caller value after it would be shown raw.
		{`rune '"' then "B C"`, `rune '"' then (value withheld)`},
		{`rune '"' then "US"`, `rune '"' then "US"`},
		{`url "https://x.example/aaaa…"`, `url (value withheld)`},
		{`empty ""`, `empty ""`},
		{`two "A" then "B C"`, `two "A" then (value withheld)`},
	} {
		if got := quotedCallerValues(tc.in); got != tc.want {
			t.Errorf("quotedCallerValues(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// A showable value cut the way the Google client's capForError cuts (past 100 bytes, with a
	// trailing "…") reads as CallerValue's truncated form, so a cut value cannot pass for a whole one.
	long := strings.Repeat("a", 120)
	cut := `id "` + long[:100] + `…"`
	want := `id ` + CallerValue(long[:100])
	if !strings.HasSuffix(want, " (truncated)") {
		t.Fatalf("CallerValue no longer truncates a 100-byte value, so this case proves nothing: %q", want)
	}
	if got := quotedCallerValues(cut); got != want {
		t.Errorf("quotedCallerValues(capForError cut) = %q, want %q", got, want)
	}
}

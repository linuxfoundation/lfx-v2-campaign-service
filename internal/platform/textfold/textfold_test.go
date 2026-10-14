// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package textfold

import (
	"strings"
	"testing"
)

func TestKey_AgreesWithEqualFold(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"Σ", "ς", true},
		{"Σ", "σ", true},
		{"σ", "ς", true},
		{"ΟΣ", "ος", true},
		{"Free Download", "free download", true},
		{"K", "K", true},   // KELVIN SIGN folds with K
		{"ß", "ss", false}, // full folding only; simple folding keeps them apart, as EqualFold does
		{"free", "freedom", false},
	} {
		if got := Equal(tc.a, tc.b); got != tc.same {
			t.Errorf("Equal(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
		if got := strings.EqualFold(tc.a, tc.b); got != tc.same {
			t.Errorf("test table disagrees with strings.EqualFold for %q, %q", tc.a, tc.b)
		}
	}
	// The reason this package exists: lower-casing does NOT agree with EqualFold here.
	lowered := []string{strings.ToLower("ΟΣ"), strings.ToLower("ος")}
	if lowered[0] == lowered[1] || Key("ΟΣ") != Key("ος") {
		t.Fatalf("ToLower gives %q / %q, Key gives %q / %q", lowered[0], lowered[1], Key("ΟΣ"), Key("ος"))
	}
}

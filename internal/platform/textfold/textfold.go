// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package textfold provides the ONE case-insensitive key every negative-keyword comparison uses
// (LFXV2-2665): request duplicates, the match against what a platform already holds, and the
// check of a platform's echo.
//
// strings.ToLower is not that key. Unicode case folding is an ORBIT, not a mapping onto lower
// case: 'Σ', 'σ' and 'ς' are one orbit, so strings.EqualFold("ΟΣ", "ος") is true while
// strings.ToLower gives "οσ" and "ος", which differ. A lookup keyed by ToLower therefore
// disagrees with an EqualFold comparison, so a keyword can be accepted as an echo yet not be found
// as already present, or the reverse.
package textfold

import (
	"strings"
	"unicode"
)

// Key maps every rune of s to the smallest rune of its unicode.SimpleFold orbit, so
// Key(a) == Key(b) exactly when strings.EqualFold(a, b) — both use simple folding rune by rune.
func Key(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(minFold(r))
	}
	return b.String()
}

// Equal reports whether a and b are the same text under simple case folding.
func Equal(a, b string) bool { return Key(a) == Key(b) }

// minFold returns the smallest rune in r's simple-fold orbit (r itself when it has none).
func minFold(r rune) rune {
	m := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < m {
			m = f
		}
	}
	return m
}

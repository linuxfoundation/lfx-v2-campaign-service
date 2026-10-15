// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "testing"

func TestNormalizeCurrencyCode(t *testing.T) {
	for in, want := range map[string]string{
		"USD": "USD", "EUR": "EUR", "JPY": "JPY",
		"": "", "usd": "", "Usd": "", "US": "", "USDD": "", " USD": "", "USD ": "",
		"US1": "", "U$D": "", "€UR": "", "ÜSD": "",
	} {
		if got := NormalizeCurrencyCode(in); got != want {
			t.Errorf("NormalizeCurrencyCode(%q) = %q, want %q", in, got, want)
		}
	}
}

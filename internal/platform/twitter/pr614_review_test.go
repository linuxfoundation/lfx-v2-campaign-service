// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"errors"
	"strings"
	"testing"
)

// TestNormalizeTweetVariants_RefusesCopyOverTheCap: copy over X's cap on its own, or with no URL
// of its own and too long once the destination URL is appended, is refused at preflight rather
// than in a failed job.
func TestNormalizeTweetVariants_RefusesCopyOverTheCap(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"over the cap alone", strings.Repeat("a", 300), "entry 1 tweetText has weighted length 300, over X's 280-character limit"},
		{"over the cap with the URL appended", strings.Repeat("a", 270), "with the destination URL appended (24) it exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeTweetVariants([]string{"Join us", tc.text}, "", "")
			var te *TargetingError
			if !errors.As(err, &te) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NormalizeTweetVariants = %v; want a TargetingError containing %q", err, tc.want)
			}
		})
	}
	for _, ok := range []string{strings.Repeat("a", 255), strings.Repeat("a", 250) + " https://events.example/kubecon"} {
		if _, err := NormalizeTweetVariants([]string{ok}, "", ""); err != nil {
			t.Errorf("NormalizeTweetVariants(%d runes) = %v, want accepted", len(ok), err)
		}
	}
}

// TestNormalizeWebsiteCardHeadline_RefusesCredentialLinks: the headline is published as the
// card's title, so a link carrying a credential query parameter is refused like tweet text.
func TestNormalizeWebsiteCardHeadline_RefusesCredentialLinks(t *testing.T) {
	_, err := NormalizeWebsiteCardHeadline("https://events.example/r?access_token=SECRET")
	var te *TargetingError
	if !errors.As(err, &te) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("NormalizeWebsiteCardHeadline = %v; want a TargetingError that does not echo the secret", err)
	}
	if h, err := NormalizeWebsiteCardHeadline("KubeCon NA: register at events.example"); err != nil || h == "" {
		t.Fatalf("plain headline = %q, %v; want accepted", h, err)
	}
}

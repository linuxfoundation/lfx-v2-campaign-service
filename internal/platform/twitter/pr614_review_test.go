// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestNormalizeKeywordAdditions_CollapsesWhitespace: the service sizes its answer from this list
// and the dispatcher sends from it, so whitespace-equivalent keywords must already be one here.
// Otherwise the service expects more outcomes than the dispatcher returns and answers 503.
func TestNormalizeKeywordAdditions_CollapsesWhitespace(t *testing.T) {
	got, err := NormalizeKeywordAdditions([]string{"cloud native", "Cloud  Native", "ebpf", "cloud native"})
	if err != nil {
		t.Fatalf("NormalizeKeywordAdditions: %v", err)
	}
	if want := []string{"cloud native", "ebpf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeKeywordAdditions = %q, want %q", got, want)
	}
}

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

// TestPositiveKeyword_MissingOperatorIsEQ: the read, the removal and the add lever share one
// predicate, so a keyword X lists without operator_type is visible, removable and present.
func TestPositiveKeyword_MissingOperatorIsEQ(t *testing.T) {
	for _, tc := range []struct {
		c          TargetingCriterion
		pos, broad bool
	}{
		{TargetingCriterion{TargetingType: TargetingBroadKeyword, TargetingValue: "ebpf"}, true, true},
		{TargetingCriterion{TargetingType: TargetingBroadKeyword, TargetingValue: "ebpf", OperatorType: OperatorEQ}, true, true},
		{TargetingCriterion{TargetingType: TargetingBroadKeyword, TargetingValue: "ebpf", OperatorType: "NE"}, false, false},
		{TargetingCriterion{TargetingType: TargetingBroadKeyword, TargetingValue: "ebpf", Deleted: true}, false, false},
		{TargetingCriterion{TargetingType: "PHRASE_KEYWORD", TargetingValue: "ebpf"}, true, false},
	} {
		if got := tc.c.PositiveKeyword(); got != tc.pos {
			t.Errorf("%+v PositiveKeyword = %v, want %v", tc.c, got, tc.pos)
		}
		if got := tc.c.PositiveBroadKeyword(); got != tc.broad {
			t.Errorf("%+v PositiveBroadKeyword = %v, want %v", tc.c, got, tc.broad)
		}
	}
}

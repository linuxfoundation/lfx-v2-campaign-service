// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// maxSearchPhrases bounds how many phrases one SearchEmailsAnyPhrase walk matches. The design
// caps the request at the same number; this is the client's own backstop, because every phrase
// is tested against every row the walk reads.
const maxSearchPhrases = 8

// SearchEmailsAnyPhrase returns the marketing emails whose name or subject contains ANY of
// phrases as WHOLE WORDS, most-recently-updated first. Read-only (idempotent).
//
// It exists for the template picker's brand-and-event scope. One event is named several ways
// across its own templates -- "Open Source Summit Japan + AutoLinux + Embedded Linux 2026" ships
// emails named "OS Summit Japan + AutoLinux + Embedded Linux 2026 | Explore the Tracks" -- so
// no single substring finds them all, and SearchEmails matches exactly one. Calling it once per
// phrase would re-read the SAME pages for every phrase (the query never goes upstream; see
// SearchEmailsMatching), so the phrases are matched together in ONE walk.
//
// WHOLE WORDS, not substrings, because the phrases are brand names and a substring match is
// wrong for exactly those: "open source summit" must not match "...opensource summitry", and
// "kubecon" must not match "kubeconference". Both sides are normalised by normalizePhrase, so
// punctuation and spacing differences ("KubeCon+CloudNativeCon", "AI_dev", "MCP Dev Summit -")
// do not stop a match.
//
// Same bounds (maxFilteredScan / maxFilteredPages) and the same ErrSearchIncomplete contract
// as SearchEmails' filtered walk: a bound reached with zero matches is an error, not an empty
// answer, because "this brand has no templates" is a claim the walk did not earn.
func (c *Client) SearchEmailsAnyPhrase(ctx context.Context, phrases []string) ([]Email, error) {
	needles := NormalizeSearchPhrases(phrases)
	if len(needles) == 0 {
		return nil, fmt.Errorf("hubspot: SearchEmailsAnyPhrase requires at least one phrase containing a letter or digit")
	}
	return c.walkEmails(ctx, "SearchEmailsAnyPhrase", func(e Email) bool {
		return ContainsAnyPhrase(e.Name, needles) || ContainsAnyPhrase(e.Subject, needles)
	}, nil)
}

// NormalizeSearchPhrases normalises each phrase with normalizePhrase, drops the ones that
// normalise to nothing, removes duplicates, and keeps at most maxSearchPhrases, in input order.
// The result is what ContainsAnyPhrase expects.
func NormalizeSearchPhrases(phrases []string) []string {
	out := make([]string, 0, len(phrases))
	seen := make(map[string]struct{}, len(phrases))
	for _, p := range phrases {
		n := normalizePhrase(p)
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
		if len(out) == maxSearchPhrases {
			break
		}
	}
	return out
}

// ContainsAnyPhrase reports whether text contains any of needles as whole words. needles must
// already be normalised (NormalizeSearchPhrases); text is normalised here.
//
// Padding both sides with a space turns "whole words" into a plain substring test: after
// normalisation every word is separated by exactly one space, so " kubecon " can only match a
// complete word, and a multi-word needle only a run of complete words in the same order.
func ContainsAnyPhrase(text string, needles []string) bool {
	if len(needles) == 0 {
		return false
	}
	haystack := " " + normalizePhrase(text) + " "
	if haystack == "  " {
		return false
	}
	for _, n := range needles {
		if n != "" && strings.Contains(haystack, " "+n+" ") {
			return true
		}
	}
	return false
}

// normalizePhrase lower-cases s and collapses every run of characters that are not letters or
// digits into a single space, trimming both ends. Letters outside ASCII are kept as letters,
// so "München" stays one word rather than being cut at the umlaut.
func normalizePhrase(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if pendingSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			pendingSpace = false
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		pendingSpace = true
	}
	return b.String()
}

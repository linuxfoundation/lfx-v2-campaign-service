// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// The platform-neutral negative-keyword match types of add-negative-keywords. Each adapter
// accepts its platform's subset and refuses the rest (LFXV2-2665).
const (
	negativeMatchExact  = "Exact"
	negativeMatchPhrase = "Phrase"
	negativeMatchBroad  = "Broad"
)

// maxNegativeKeywordsPerRequest matches the design's MaxLength on add-negative-keywords.
const maxNegativeKeywordsPerRequest = 60

// canonicalNegativeMatchType maps any casing of a match type onto its canonical spelling.
func canonicalNegativeMatchType(in string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "exact":
		return negativeMatchExact, true
	case "phrase":
		return negativeMatchPhrase, true
	case "broad":
		return negativeMatchBroad, true
	}
	return "", false
}

// validateNegativeKeywordBatch checks a negative-keyword batch for a platform WITHOUT contacting
// it and returns the normalized batch, index-aligned with the input. The TEXT rules are
// Microsoft's (microsoft.NormalizeNegativeKeywordText), applied to every platform because the
// endpoint publishes one input contract; the MATCH TYPES are the platform's, named in allowed.
// Each refusal is ErrNegativeKeywordInvalid:
//   - 1..max entries;
//   - a text the shared rules refuse;
//   - a match type outside allowed (any casing accepted for the ones inside);
//   - the same (match type, case-folded text) twice — refused, not de-duplicated, because the
//     outcomes are positional.
func validateNegativeKeywordBatch(label string, keywords []model.NegativeKeyword, max int, allowed ...string) ([]model.NegativeKeyword, error) {
	if len(keywords) == 0 || len(keywords) > max {
		return nil, fmt.Errorf("%w: %s: send 1 to %d negative keywords, got %d", domain.ErrNegativeKeywordInvalid, label, max, len(keywords))
	}
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	out := make([]model.NegativeKeyword, 0, len(keywords))
	seen := make(map[string]bool, len(keywords))
	for i, k := range keywords {
		text, err := microsoft.NormalizeNegativeKeywordText(k.Text)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: negative keyword %d %w", domain.ErrNegativeKeywordInvalid, label, i, err)
		}
		mt, known := canonicalNegativeMatchType(k.MatchType)
		if !known || !ok[mt] {
			return nil, fmt.Errorf("%w: %s: negative keyword %d has a match type the platform does not offer (want one of %s)",
				domain.ErrNegativeKeywordInvalid, label, i, strings.Join(allowed, ", "))
		}
		key := mt + "\x00" + strings.ToLower(text)
		if seen[key] {
			return nil, fmt.Errorf("%w: %s: negative keyword %d repeats an earlier entry with the same text and match type", domain.ErrNegativeKeywordInvalid, label, i)
		}
		seen[key] = true
		out = append(out, model.NegativeKeyword{Text: text, MatchType: mt})
	}
	return out, nil
}

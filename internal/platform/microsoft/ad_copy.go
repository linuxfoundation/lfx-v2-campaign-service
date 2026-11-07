// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Caller-supplied responsive search ad copy (#563, gap 1): headlines, descriptions and the two
// display-URL path segments a caller sends instead of the auto-composed copy.
//
// The client has always accepted CampaignInput.Headlines/Descriptions, leniently: an empty entry
// is skipped and a short list is padded with placeholders (composeAdCopy), which is right for an
// internal caller that wants "at least a valid ad". A caller who SENDS copy, though, is sending
// approved copy, and a placeholder slipped in beside it is worse than a refusal. So the strict
// rules a request is held to live here, once, and are exported for the dispatcher's synchronous
// pre-check; CreateCampaign still runs its own lenient checks plus validatePaths before any
// request.
//
// Path1/Path2 follow the v13 ResponsiveSearchAd element docs: at most 15 final characters each
// (7 "for languages with double-width characters"), no forward slash and no newline, Path2 only
// with Path1, and the final URL domain plus the paths at most 67 characters (33 with double-width
// characters). The client applies the reduced caps whenever ANY double-width character is
// present, as it already does for the copy and the display domain, because v13 publishes no
// per-character weighting.

// The request field names a refusal names, so the dispatcher's 400 and the client agree.
const (
	FieldHeadlines    = "headlines"
	FieldDescriptions = "descriptions"
	FieldPath1        = "path1"
	FieldPath2        = "path2"
)

const (
	// maxAdPathRunes / maxAdPathRunesWide are the Path1/Path2 final-character caps.
	maxAdPathRunes     = 15
	maxAdPathRunesWide = 7
)

// AdCopyError is a refusal of one caller ad-copy field. Field is the request field name
// (FieldHeadlines, …); Msg is the operator-safe reason, which quotes a caller value only through
// domain.CallerValue.
type AdCopyError struct {
	Field string
	Msg   string
}

func (e *AdCopyError) Error() string { return e.Field + ": " + e.Msg }

// ValidateCallerAdCopy holds caller-supplied copy to the strict rules: when a list is supplied it
// must have 3–15 headlines / 2–4 descriptions, every entry must carry a word (an empty or
// whitespace-only entry is refused, never skipped), and checkAdCopyList's width-aware length,
// control-character and case-insensitive-uniqueness rules apply. An omitted (nil or empty) list
// is fine: that side is auto-composed. The paths are checked by validatePaths.
func ValidateCallerAdCopy(headlines, descriptions []string, path1, path2 string) error {
	if err := checkCallerCopyList(FieldHeadlines, "headline", headlines, minAdHeadlines, maxAdHeadlines, maxAdHeadlineRunes, maxAdHeadlineRunesWide); err != nil {
		return err
	}
	if err := checkCallerCopyList(FieldDescriptions, "description", descriptions, minAdDescriptions, maxAdDescriptions, maxAdDescriptionRunes, maxAdDescriptionRunesWide); err != nil {
		return err
	}
	_, _, err := validatePaths(path1, path2)
	return err
}

// checkCallerCopyList applies the strict caller rules to one list.
func checkCallerCopyList(field, kind string, items []string, minCount, maxCount, singleLimit, wideLimit int) error {
	if len(items) == 0 {
		return nil
	}
	if len(items) < minCount || len(items) > maxCount {
		return &AdCopyError{Field: field, Msg: fmt.Sprintf("must have %d to %d %ss, got %d", minCount, maxCount, kind, len(items))}
	}
	for i, raw := range items {
		if strings.TrimSpace(raw) == "" {
			return &AdCopyError{Field: field, Msg: fmt.Sprintf("%s %d must contain at least one word", kind, i+1)}
		}
	}
	if err := checkAdCopyList(kind, items, maxCount, singleLimit, wideLimit); err != nil {
		return &AdCopyError{Field: field, Msg: err.Error()}
	}
	return nil
}

// validatePaths checks and trims the two display-URL path segments, returning the values the ad
// will carry. Both empty is the pre-#563 ad (no paths).
func validatePaths(path1, path2 string) (string, string, error) {
	p1, err := checkPath(FieldPath1, path1)
	if err != nil {
		return "", "", err
	}
	p2, err := checkPath(FieldPath2, path2)
	if err != nil {
		return "", "", err
	}
	if p2 != "" && p1 == "" {
		return "", "", &AdCopyError{Field: FieldPath2, Msg: "requires path1 (Microsoft shows path2 only after path1)"}
	}
	return p1, p2, nil
}

// checkPath validates one path segment. A value that is empty after trimming is "not set"; a
// non-empty one must be word-bearing, slash- and control-free and within its width-aware cap.
func checkPath(field, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "", &AdCopyError{Field: field, Msg: "must not contain a control character"}
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", &AdCopyError{Field: field, Msg: "must not be blank; omit it instead"}
	}
	if strings.Contains(s, "/") {
		return "", &AdCopyError{Field: field, Msg: "must not contain a forward slash (each path is one segment)"}
	}
	if !hasWord(s) {
		return "", &AdCopyError{Field: field, Msg: "must contain at least one letter or digit"}
	}
	limit := maxAdPathRunes
	if hasDoubleWidth(s) {
		limit = maxAdPathRunesWide
	}
	if n := utf8.RuneCountInString(s); n > limit {
		if limit == maxAdPathRunesWide {
			return "", &AdCopyError{Field: field, Msg: fmt.Sprintf("is %d characters, exceeding %d (the reduced limit applies because it contains a double-width character)", n, limit)}
		}
		return "", &AdCopyError{Field: field, Msg: fmt.Sprintf("is %d characters, exceeding %d", n, limit)}
	}
	return s, nil
}

// displayURL is the text Microsoft measures against the display-URL cap: the final URL's
// authority followed by "/path1" and "/path2" when set. The separators are counted, which is the
// conservative reading of "the domain portion of the URL in combination with the path 1 and path 2
// strings cannot exceed 67 characters".
func displayURL(authority, path1, path2 string) string {
	s := authority
	if path1 != "" {
		s += "/" + path1
	}
	if path2 != "" {
		s += "/" + path2
	}
	return s
}

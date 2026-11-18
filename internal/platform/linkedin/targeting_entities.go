// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Targeting entity typeahead (#585): search LinkedIn's targeting taxonomy by name, so a picker
// can offer the URNs linkedInConfig.targeting (and linkedInConfig.geoTargets) take without the
// operator pasting them. One request, li-lms-2026-02 (Ad Targeting › Find Entities by
// Typeahead):
//
//	GET /rest/adTargetingEntities?q=typeahead&facet=urn%3Ali%3AadTargetingFacet%3A<key>&query=<text>&queryVersion=QUERY_USES_URNS
//
// locale is left to LinkedIn's documented default (language en, country US). The answer is
// LinkedIn's own taxonomy, the same for every token and every ad account: it names no resource
// any foundation owns.

// MaxTypeaheadQueryLen bounds the query text, in characters. A local bound: LinkedIn documents
// none, and no skill, title or place name is longer.
const MaxTypeaheadQueryLen = 100

// MinTypeaheadQueryLen is the shortest query searched, in characters after trimming. A
// one-character prefix matches a large slice of the taxonomy and is the keystroke a picker
// sends first, so refusing it keeps the cheapest, least useful search off the shared LF token's
// adTargetingEntities quota. Callers should debounce as well: the answer cache only serves a
// repeated identical search, and every new prefix is a new search.
const MinTypeaheadQueryLen = 2

// maxTypeaheadEntities bounds what one answer keeps. LinkedIn's typeahead returns one short
// ranked page; this only stops an unexpectedly large one from being passed on whole.
const maxTypeaheadEntities = 100

// FacetLocations is the typeahead facet for linkedInConfig.geoTargets: a location URN is not a
// linkedInConfig.targeting facet (the geo clause is built from geoTargets), but the picker needs
// it by name all the same.
const FacetLocations = "locations"

// ErrTypeaheadInvalid marks a search refused before any request: an unsupported facet or an
// unusable query. The message is service-authored and never quotes the query.
var ErrTypeaheadInvalid = errors.New("linkedin-ads: invalid targeting entity search")

// typeaheadFacets are the facets the typeahead serves, each with the URN shape an answer must
// have to be kept. Only facets that LinkedIn's typeahead finder supports ("Discovering
// Targeting Entities") AND that a create accepts are listed, and the targeting facets take
// their shape from targetingFacets itself, so a URN offered here is one ValidateTargeting
// accepts. seniorities, jobFunctions, staffCountRanges and interfaceLocales are short fixed
// lists LinkedIn serves only by its adTargetingFacet finder, not typeahead; the segment facets
// are account resources, picked from GET .../linkedin-ads/audiences.
var typeaheadFacets = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{FacetLocations: geoURNRE}
	for _, k := range []string{"skills", "groups", "titles", "interests", "industries", "employers"} {
		f, ok := targetingFacets[k]
		if !ok || f.pattern == nil {
			panic("linkedin: typeahead facet " + k + " is not a pattern facet of targetingFacets")
		}
		m[k] = f.pattern
	}
	return m
}()

// TypeaheadFacetKeys lists the facets SearchTargetingEntities serves, sorted.
func TypeaheadFacetKeys() []string {
	keys := make([]string, 0, len(typeaheadFacets))
	for k := range typeaheadFacets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TargetingEntity is one typeahead answer: a URN in exactly the form the facet takes, and
// LinkedIn's display name for it.
type TargetingEntity struct {
	URN  string
	Name string
}

// NormalizeTypeaheadQuery trims query and checks it: MinTypeaheadQueryLen to
// MaxTypeaheadQueryLen characters after trimming, valid UTF-8, and no control characters. It is shared by the service layer and
// SearchTargetingEntities so the two cannot disagree on what reaches LinkedIn.
func NormalizeTypeaheadQuery(query string) (string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", fmt.Errorf("%w: query is blank", ErrTypeaheadInvalid)
	}
	if !utf8.ValidString(q) {
		return "", fmt.Errorf("%w: query is not valid UTF-8", ErrTypeaheadInvalid)
	}
	if utf8.RuneCountInString(q) < MinTypeaheadQueryLen {
		return "", fmt.Errorf("%w: query is shorter than %d characters", ErrTypeaheadInvalid, MinTypeaheadQueryLen)
	}
	if utf8.RuneCountInString(q) > MaxTypeaheadQueryLen {
		return "", fmt.Errorf("%w: query is longer than %d characters", ErrTypeaheadInvalid, MaxTypeaheadQueryLen)
	}
	if strings.IndexFunc(q, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: query holds a control character", ErrTypeaheadInvalid)
	}
	return q, nil
}

// SearchTargetingEntities runs LinkedIn's typeahead finder for one facet and returns the
// matches in LinkedIn's order (its relevance ranking), de-duplicated by URN. An entry whose URN
// does not have the facet's shape, or that carries no name, is dropped and counted in dropped,
// so a taxonomy LinkedIn changes cannot put a value in the picker that the create then refuses.
//
// The request is sent ONCE (withSendOnce): a 429 is not retried in-call but comes back as the
// error, which the service answers 503. A typeahead that waits out a backoff answers a keystroke
// the operator has already typed past, and each retry spends the shared LF token's quota again;
// the picker's next keystroke is the retry.
//
// The query is never in a returned error: a transport failure's *url.Error would quote the
// request URL, query string and all, so its URL is cut back to the path (scrubQuery).
func (c *Client) SearchTargetingEntities(ctx context.Context, facet, query string) (_ []TargetingEntity, dropped int, _ error) {
	pattern, ok := typeaheadFacets[facet]
	if !ok {
		return nil, 0, fmt.Errorf("%w: facet must be one of %s", ErrTypeaheadInvalid, strings.Join(TypeaheadFacetKeys(), ", "))
	}
	q, err := NormalizeTypeaheadQuery(query)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.doRequest(withSendOnce(ctx), http.MethodGet, "adTargetingEntities", nil, map[string]string{
		"q":            "typeahead",
		"facet":        facetURN(facet),
		"query":        restliEncode(q),
		"queryVersion": "QUERY_USES_URNS",
	}, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("search linkedin targeting entities: %w", scrubQuery(err))
	}
	if resp.Elements == nil {
		return nil, 0, errors.New("search linkedin targeting entities: a 2xx response with no elements field")
	}
	out := make([]TargetingEntity, 0, min(len(*resp.Elements), maxTypeaheadEntities))
	seen := make(map[string]struct{}, len(*resp.Elements))
	for _, el := range *resp.Elements {
		urn := strings.TrimSpace(el.URN)
		name := strings.TrimSpace(el.Name)
		if !pattern.MatchString(urn) || name == "" {
			dropped++
			continue
		}
		if _, dup := seen[urn]; dup {
			continue
		}
		seen[urn] = struct{}{}
		if len(out) == maxTypeaheadEntities {
			break
		}
		out = append(out, TargetingEntity{URN: urn, Name: name})
	}
	return out, dropped, nil
}

// scrubQuery cuts the query string off the *url.Error in err's chain, in place, so the caller's
// search text cannot reach a log line or a response through the error's message. The chain
// itself is untouched, so errors.Is and errors.As still see every sentinel. doRequest makes one
// http.Client.Do call per attempt, so a chain holds at most one *url.Error.
func scrubQuery(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if i := strings.IndexByte(ue.URL, '?'); i >= 0 {
			ue.URL = ue.URL[:i]
		}
	}
	return err
}

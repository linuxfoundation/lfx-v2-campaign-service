// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Targeting search (#550, gap 7 picker read): look up Meta's detailed-targeting taxonomy by name, so
// a picker can offer the ids metaConfig.targeting.interests, behaviors and workPositions take
// without the operator pasting them. One request each, Targeting Search reference:
//
//	interest:      GET /search?type=adinterest&q=<text>&limit=<n>
//	behavior:      GET /search?type=adTargetingCategory&class=behaviors   ("Parameter q ignored": a
//	               browse of the whole class, filtered here by name)
//	work_position: GET /search?type=adworkposition&q=<text>&limit=<n>
//
// The answer is Meta's own taxonomy, the same for every token: it names no ad account resource.

// Targeting search kinds, the values of the read's `type` parameter.
const (
	TargetingSearchInterest     = "interest"
	TargetingSearchBehavior     = "behavior"
	TargetingSearchWorkPosition = "work_position"
)

// TargetingSearchKinds lists the kinds SearchTargeting serves, in a stable order.
func TargetingSearchKinds() []string {
	return []string{TargetingSearchInterest, TargetingSearchBehavior, TargetingSearchWorkPosition}
}

// MinTargetingQueryLen and MaxTargetingQueryLen bound the query text in characters after trimming.
// A one-character prefix matches a large slice of the taxonomy and is the first keystroke a picker
// sends, so it is refused to keep the cheapest, least useful search off the shared token's quota;
// Meta documents no upper bound, and no interest or job title is longer than 100.
const (
	MinTargetingQueryLen = 2
	MaxTargetingQueryLen = 100
)

// maxTargetingResults bounds one answer: the limit sent for the q-searched kinds, and the cap on a
// filtered behavior browse.
const maxTargetingResults = 50

// ErrTargetingSearchInvalid marks a search refused before any request: an unknown kind or an
// unusable query. The message is service-authored and never quotes the query.
var ErrTargetingSearchInvalid = errors.New("meta: invalid targeting search")

// NormalizeTargetingQuery trims query and checks it: MinTargetingQueryLen to MaxTargetingQueryLen
// characters after trimming, valid UTF-8, no control characters. The service layer and
// SearchTargeting share it, so they cannot disagree on what reaches Meta.
func NormalizeTargetingQuery(query string) (string, error) {
	q := strings.TrimSpace(query)
	switch {
	case !utf8.ValidString(q):
		return "", fmt.Errorf("%w: query is not valid UTF-8", ErrTargetingSearchInvalid)
	case utf8.RuneCountInString(q) < MinTargetingQueryLen:
		return "", fmt.Errorf("%w: query is shorter than %d characters", ErrTargetingSearchInvalid, MinTargetingQueryLen)
	case utf8.RuneCountInString(q) > MaxTargetingQueryLen:
		return "", fmt.Errorf("%w: query is longer than %d characters", ErrTargetingSearchInvalid, MaxTargetingQueryLen)
	case strings.IndexFunc(q, unicode.IsControl) >= 0:
		return "", fmt.Errorf("%w: query holds a control character", ErrTargetingSearchInvalid)
	}
	return q, nil
}

// TargetingOption is one search answer. ID is exactly what the matching metaConfig.targeting list
// takes. The size fields are Meta's own estimates and nil when Meta did not report one: interests
// and behaviors may carry a lower and upper bound, a work position a single coverage figure.
type TargetingOption struct {
	ID            string
	Name          string
	Path          []string
	Description   string
	AudienceLower *int64
	AudienceUpper *int64
	Coverage      *int64
}

// targetingSearchRow is the union of the three kinds' documented row fields.
type targetingSearchRow struct {
	ID            graphID  `json:"id"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Path          []string `json:"path"`
	Description   string   `json:"description"`
	Subtext       string   `json:"subtext"`
	AudienceLower *int64   `json:"audience_size_lower_bound"`
	AudienceUpper *int64   `json:"audience_size_upper_bound"`
	Coverage      *int64   `json:"coverage"`
}

// targetingSearchPath builds the request path for kind (the query is already normalized).
func targetingSearchPath(kind, query string) (string, bool) {
	v := url.Values{}
	switch kind {
	case TargetingSearchInterest:
		v.Set("type", "adinterest")
	case TargetingSearchWorkPosition:
		v.Set("type", "adworkposition")
	case TargetingSearchBehavior:
		v.Set("type", "adTargetingCategory")
		v.Set("class", "behaviors")
		return "/search?" + v.Encode(), true
	default:
		return "", false
	}
	v.Set("q", query)
	v.Set("limit", fmt.Sprint(maxTargetingResults))
	return "/search?" + v.Encode(), true
}

// SearchTargeting runs one Targeting Search and returns the matches in Meta's order (its relevance
// ranking), de-duplicated by id. A behavior search is a browse of the whole class, kept here when the
// name contains the query (ignoring case) and capped at maxTargetingResults; use SearchTargetingAll
// to read the whole browse once and filter it locally. A row without a numeric id or a name, or a
// behavior row of another class, is dropped and counted in dropped.
//
// The request is sent ONCE: a throttle is not retried in-call but comes back as the error, which the
// service answers 503. A picker that waits out a backoff answers a keystroke the operator has already
// typed past, and each retry spends the shared token's quota again.
//
// The query is never in a returned error: every failure is wrapped in a targetingSearchError whose
// message names only the kind and the failure class (see scrubTargetingSearchError).
func (c *Client) SearchTargeting(ctx context.Context, kind, query string) (_ []TargetingOption, dropped int, _ error) {
	q, err := NormalizeTargetingQuery(query)
	if err != nil {
		return nil, 0, err
	}
	rows, dropped, err := c.searchTargeting(ctx, kind, q)
	if err != nil {
		return nil, 0, err
	}
	if kind == TargetingSearchBehavior {
		rows = FilterTargetingOptions(rows, q)
	}
	return rows, dropped, nil
}

// SearchTargetingAll is SearchTargeting for the behavior browse without the name filter: the whole
// class, for a caller that caches it and filters each query locally (FilterTargetingOptions).
func (c *Client) SearchTargetingAll(ctx context.Context) (_ []TargetingOption, dropped int, _ error) {
	return c.searchTargeting(ctx, TargetingSearchBehavior, "")
}

// FilterTargetingOptions keeps the options whose name contains query, ignoring case, capped at
// maxTargetingResults. It returns a NEW, non-nil slice.
func FilterTargetingOptions(rows []TargetingOption, query string) []TargetingOption {
	needle := strings.ToLower(strings.TrimSpace(query))
	out := make([]TargetingOption, 0, min(len(rows), maxTargetingResults))
	for _, r := range rows {
		if len(out) == maxTargetingResults {
			break
		}
		if strings.Contains(strings.ToLower(r.Name), needle) {
			out = append(out, r)
		}
	}
	return out
}

func (c *Client) searchTargeting(ctx context.Context, kind, q string) (_ []TargetingOption, dropped int, _ error) {
	path, ok := targetingSearchPath(kind, q)
	if !ok {
		return nil, 0, fmt.Errorf("%w: type must be one of %s", ErrTargetingSearchInvalid, strings.Join(TargetingSearchKinds(), ", "))
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, &raw, false); err != nil {
		return nil, 0, scrubTargetingSearchError(kind, err)
	}
	var page struct {
		Data *[]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || page.Data == nil {
		return nil, 0, &targetingSearchError{kind: kind, class: "the answer had no data field"}
	}
	out := make([]TargetingOption, 0, len(*page.Data))
	seen := make(map[string]bool, len(*page.Data))
	for _, rawRow := range *page.Data {
		var row targetingSearchRow
		if err := json.Unmarshal(rawRow, &row); err != nil {
			dropped++
			continue
		}
		id, name := string(row.ID), strings.TrimSpace(row.Name)
		if !numericIDRE.MatchString(id) || name == "" || (kind == TargetingSearchBehavior && row.Type != "" && row.Type != "behaviors") {
			dropped++
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		opt := TargetingOption{ID: id, Name: name, Path: row.Path, Description: strings.TrimSpace(row.Description),
			AudienceLower: nonNegative(row.AudienceLower), AudienceUpper: nonNegative(row.AudienceUpper), Coverage: nonNegative(row.Coverage)}
		if opt.Description == "" {
			opt.Description = strings.TrimSpace(row.Subtext)
		}
		out = append(out, opt)
		if kind != TargetingSearchBehavior && len(out) == maxTargetingResults {
			break
		}
	}
	return out, dropped, nil
}

// nonNegative reads Meta's size estimate, where a negative value (Meta answers -1 for "not
// computed") means absent.
func nonNegative(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	return v
}

// targetingSearchError carries a search failure without its request URL. Error names the kind and a
// fixed failure class only; Unwrap keeps the original chain, so errors.Is and errors.As still see
// every sentinel (a caller's classification is unchanged), while a log line or response built from
// Error() cannot quote the operator's search text.
type targetingSearchError struct {
	kind  string
	class string
	err   error
}

func (e *targetingSearchError) Error() string {
	return "meta targeting search (" + e.kind + "): " + e.class
}

func (e *targetingSearchError) Unwrap() error { return e.err }

// scrubTargetingSearchError wraps err in a targetingSearchError and, in place, cuts the query string
// off every request path or URL its chain carries (APIError.Path, transportError.Path, url.Error.URL),
// so even a caller that digs into the chain finds no search text.
func scrubTargetingSearchError(kind string, err error) error {
	class := "the request could not be completed"
	var ae *APIError
	if errors.As(err, &ae) {
		ae.Path = cutQuery(ae.Path)
		class = fmt.Sprintf("Meta answered HTTP %d (Graph code %d)", ae.StatusCode, ae.Code)
		if ae.StatusCode == http.StatusTooManyRequests || graphRateLimitCodes[ae.Code] {
			class = "Meta is rate-limiting the search; it was not retried"
		}
	}
	var te *transportError
	if errors.As(err, &te) {
		te.Path = cutQuery(te.Path)
		class = "the answer could not be read"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = cutQuery(ue.URL)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		class = "the search timed out or was cancelled"
	}
	return &targetingSearchError{kind: kind, class: class, err: err}
}

func cutQuery(s string) string {
	if i := strings.IndexByte(s, '?'); i >= 0 {
		return s[:i]
	}
	return s
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ---------------------------------------------------------------------------
// Custom audiences (#564, gap 3): retargeting and exclusion on the line item, and the picker read.
//
// X Ads API v12 reference (https://docs.x.com/x-ads-api/audiences/reference, read 2026-11-07):
//   - GET accounts/:account_id/custom_audiences takes count (max 1000), cursor,
//     custom_audience_ids (comma-separated, max 200) and with_deleted, and answers custom
//     audience objects carrying id, name, audience_type, targetable, reasons_not_targetable,
//     audience_size and deleted. X: "to target a Custom Audience, that audience must be
//     targetable".
//   - A custom audience is targeted by a targeting criterion of targeting_type CUSTOM_AUDIENCE
//     whose targeting_value is the audience id; operator_type NE excludes it. It goes through the
//     same per-criterion POST as the location and language criteria (create_targeting.go).
//
// VERIFICATION LEVEL: none of this has been exercised against a live ad account, which is why
// both the create inputs and the read sit behind TWITTER_CUSTOM_AUDIENCES_ENABLED.
// ---------------------------------------------------------------------------

const (
	// FieldCustomAudienceIDs and FieldExcludedCustomAudienceIDs are the twitterConfig keys.
	FieldCustomAudienceIDs         = "customAudienceIds"
	FieldExcludedCustomAudienceIDs = "excludedCustomAudienceIds"

	// MaxCustomAudienceIDs caps each create list. X allows 100 custom audience criteria per line
	// item, but every criterion is a separate write paced at one per second inside the dispatch's
	// two-minute budget, so a create is held to MaxCreateTargetingCriteria criteria in total and
	// one list can never usefully exceed that.
	MaxCustomAudienceIDs = MaxCreateTargetingCriteria

	// TargetingCustomAudience is X's targeting_type for a custom audience criterion.
	TargetingCustomAudience = "CUSTOM_AUDIENCE"

	// maxCustomAudienceIDLen bounds one id; X's are short base-36 strings such as "1nmth".
	maxCustomAudienceIDLen = 32
	// customAudienceIDsPerLookup is X's custom_audience_ids maximum per request.
	customAudienceIDsPerLookup = 200
	// customAudienceMaxPages caps the picker's walk. Reaching it with a cursor outstanding is
	// not an error: the read reports Truncated, so a picker can say the list is incomplete.
	customAudienceMaxPages = 10
	// maxReasonsShown bounds how many of X's reasons_not_targetable a refusal names.
	maxReasonsShown = 5
)

var (
	customAudienceIDRe = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)
	// reasonRe admits only an enum-shaped reason (TOO_SMALL, PROCESSING) into a message, since
	// every other value is upstream text.
	reasonRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// ErrCustomAudiencesMalformed marks a 2xx custom-audience answer this client cannot believe: no
// data list, an unusable or repeated id, or a page walk that cannot be followed. Upstream text
// never appears in it.
var ErrCustomAudiencesMalformed = errors.New("x: custom audience list could not be confirmed")

// CustomAudience is one custom audience on the client's ad account.
type CustomAudience struct {
	// ID is the audience id, in the form customAudienceIds / excludedCustomAudienceIds accept.
	ID string
	// Name is the display name; may be empty. Not an identifier.
	Name string
	// AudienceType is X's audience_type verbatim (WEB, LIST, MOBILE, ...); may be empty.
	AudienceType string
	// Targetable is X's targetable flag: only a targetable audience can be sent at create.
	Targetable bool
	// AudienceSize is X's audience_size; nil when X sent none, which is not a size of 0.
	AudienceSize *int64
	// ReasonsNotTargetable are X's reasons, enum-shaped values only (TOO_SMALL, PROCESSING).
	ReasonsNotTargetable []string
}

type customAudienceWire struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	AudienceType string `json:"audience_type"`
	// Targetable and Deleted are pointers so an absent or null flag is told apart from false:
	// the create lookup refuses to conclude anything from a missing one (checkCustomAudiences).
	Targetable           *bool    `json:"targetable"`
	AudienceSize         *int64   `json:"audience_size"`
	ReasonsNotTargetable []string `json:"reasons_not_targetable"`
	Deleted              *bool    `json:"deleted"`
}

// deleted reports X's deleted flag; an absent one reads as not deleted, which is what the picker
// walk (with_deleted defaults to false) may assume. The create lookup does not rely on it.
func (w customAudienceWire) deleted() bool { return w.Deleted != nil && *w.Deleted }

func (w customAudienceWire) audience() CustomAudience {
	reasons := make([]string, 0, len(w.ReasonsNotTargetable))
	for _, r := range w.ReasonsNotTargetable {
		if r = strings.TrimSpace(r); reasonRe.MatchString(r) {
			reasons = append(reasons, r)
		}
	}
	return CustomAudience{
		ID:                   w.ID,
		Name:                 strings.TrimSpace(w.Name),
		AudienceType:         strings.TrimSpace(w.AudienceType),
		Targetable:           w.Targetable != nil && *w.Targetable,
		AudienceSize:         w.AudienceSize,
		ReasonsNotTargetable: reasons,
	}
}

// ValidCustomAudienceID reports whether id can be sent as an X custom audience id: 1-32 letters
// or digits. It also keeps an id from altering a query or a criterion value.
func ValidCustomAudienceID(id string) bool { return customAudienceIDRe.MatchString(id) }

// ValidateCustomAudienceIDs checks the two create lists together. Ids are compared exactly, not
// trimmed or case-folded: X's ids are case-sensitive opaque strings, and trimming would send an
// id the caller did not write. A blank, malformed or repeated id, an id in both lists, or a list
// over MaxCustomAudienceIDs is a *TargetingError naming the list and the 1-based position, never
// the value.
func ValidateCustomAudienceIDs(include, exclude []string) error {
	check := func(field string, ids []string) (map[string]struct{}, error) {
		if len(ids) > MaxCustomAudienceIDs {
			return nil, &TargetingError{Field: field, Msg: fmt.Sprintf("holds %d ids; at most %d are accepted", len(ids), MaxCustomAudienceIDs)}
		}
		seen := make(map[string]struct{}, len(ids))
		for i, id := range ids {
			if !ValidCustomAudienceID(id) {
				return nil, &TargetingError{Field: field, Msg: fmt.Sprintf("entry %d is not an X custom audience id (1-%d letters or digits)", i+1, maxCustomAudienceIDLen)}
			}
			if _, dup := seen[id]; dup {
				return nil, &TargetingError{Field: field, Msg: fmt.Sprintf("entry %d repeats an earlier id", i+1)}
			}
			seen[id] = struct{}{}
		}
		return seen, nil
	}
	in, err := check(FieldCustomAudienceIDs, include)
	if err != nil {
		return err
	}
	if _, err := check(FieldExcludedCustomAudienceIDs, exclude); err != nil {
		return err
	}
	for i, id := range exclude {
		if _, both := in[id]; both {
			return &TargetingError{Field: FieldExcludedCustomAudienceIDs, Msg: fmt.Sprintf("entry %d is also in %s; an audience cannot be both targeted and excluded", i+1, FieldCustomAudienceIDs)}
		}
	}
	return nil
}

// decodeCustomAudiencePage decodes one custom_audiences answer, refusing an envelope it cannot
// trust (a duplicated key or malformed text) and a missing, null or non-array data field.
func decodeCustomAudiencePage(resp *apiResponse) ([]customAudienceWire, error) {
	if resp == nil || resp.raw != nil && identityjson.Check(resp.raw) != nil {
		return nil, fmt.Errorf("%w: the response cannot be trusted (duplicated key or malformed text)", ErrCustomAudiencesMalformed)
	}
	var els []customAudienceWire
	if len(resp.Data) == 0 || json.Unmarshal(resp.Data, &els) != nil || els == nil {
		return nil, fmt.Errorf("%w: the response carries no list of audiences", ErrCustomAudiencesMalformed)
	}
	return els, nil
}

// checkCustomAudiences confirms, before anything is created, that every id names a live,
// targetable custom audience on the client's ad account (GET custom_audiences with
// custom_audience_ids, with_deleted=true so a deleted audience is named as deleted rather than
// as missing). An id X does not return, a deleted one, or one X reports not targetable is a
// PreflightRefusal naming the id and, for a non-targetable one, X's enum-shaped reasons. A read
// that fails or cannot be believed is an error: either way nothing is created.
func (c *Client) checkCustomAudiences(ctx context.Context, include, exclude []string) error {
	type want struct{ field, id string }
	var wants []want
	for _, id := range exclude {
		wants = append(wants, want{FieldExcludedCustomAudienceIDs, id})
	}
	for _, id := range include {
		wants = append(wants, want{FieldCustomAudienceIDs, id})
	}
	got := make(map[string]customAudienceWire, len(wants))
	for start := 0; start < len(wants); start += customAudienceIDsPerLookup {
		end := min(start+customAudienceIDsPerLookup, len(wants))
		ids := make([]string, 0, end-start)
		for _, w := range wants[start:end] {
			ids = append(ids, w.id)
		}
		q := url.Values{}
		q.Set("custom_audience_ids", strings.Join(ids, ","))
		q.Set("with_deleted", "true")
		q.Set("count", strconv.Itoa(listPageSize))
		resp, err := c.doRequestAbs(ctx, http.MethodGet, c.accountURL()+"/custom_audiences?"+q.Encode(), "custom_audiences", nil, true /* idempotent: a read */)
		if err != nil {
			return fmt.Errorf("x custom audience lookup failed: %w", err)
		}
		els, err := decodeCustomAudiencePage(resp)
		if err != nil {
			return fmt.Errorf("x custom audience lookup: %w", err)
		}
		for _, e := range els {
			if _, dup := got[e.ID]; dup {
				return fmt.Errorf("x custom audience lookup: %w: the same audience was listed twice", ErrCustomAudiencesMalformed)
			}
			got[e.ID] = e
		}
	}
	for _, w := range wants {
		e, ok := got[w.id]
		// The lookup asks for deleted audiences too, so a live one is only one X says is not
		// deleted and is targetable. An absent or null flag says neither: the answer cannot be
		// believed, and nothing is created.
		if ok && (e.Deleted == nil || e.Targetable == nil) {
			return fmt.Errorf("x custom audience lookup: %w: an audience came back without its deleted or targetable flag", ErrCustomAudiencesMalformed)
		}
		switch {
		case !ok:
			return domain.PreflightRefusalf("invalid twitterConfig.%s: X has no custom audience %s on this ad account", w.field, domain.CallerValue(w.id))
		case *e.Deleted:
			return domain.PreflightRefusalf("invalid twitterConfig.%s: X custom audience %s is deleted", w.field, domain.CallerValue(w.id))
		case !*e.Targetable:
			reasons := e.audience().ReasonsNotTargetable
			if len(reasons) > maxReasonsShown {
				reasons = reasons[:maxReasonsShown]
			}
			why := ""
			if len(reasons) > 0 {
				why = " (" + strings.Join(reasons, ", ") + ")"
			}
			return domain.PreflightRefusalf("invalid twitterConfig.%s: X custom audience %s is not targetable%s", w.field, domain.CallerValue(w.id), why)
		}
	}
	return nil
}

// customAudienceCriteria turns the validated lists into criteria: exclusions (NE) first, so a
// create that stops part-way has applied the suppression before any inclusion, then inclusions
// (EQ).
func customAudienceCriteria(include, exclude []string) (excluded, included []createCriterion) {
	for _, id := range exclude {
		excluded = append(excluded, createCriterion{targetingType: TargetingCustomAudience, value: id, operator: OperatorNE, label: "excluded custom audience " + id})
	}
	for _, id := range include {
		included = append(included, createCriterion{targetingType: TargetingCustomAudience, value: id, operator: OperatorEQ, label: "custom audience " + id})
	}
	return excluded, included
}

// ListCustomAudiences lists every non-deleted custom audience on the client's ad account (GET
// custom_audiences, count=1000), following next_cursor for at most customAudienceMaxPages pages.
// A walk that still has a cursor at the cap returns what it read with truncated true. Every
// audience is listed, targetable or not, so a picker can show why one cannot be used.
//
// Any other failure (a non-2xx, a transport error, an untrustworthy body, an unusable or
// repeated id, a full page with no usable cursor, a repeated cursor) returns nil and an error,
// never a partial list. An account with no audience is an empty non-nil slice.
func (c *Client) ListCustomAudiences(ctx context.Context) (audiences []CustomAudience, truncated bool, err error) {
	if c.account.AccountID == "" || !accountIDRe.MatchString(c.account.AccountID) {
		return nil, false, errors.New("list x custom audiences: the connection's ad account id is not usable")
	}
	out := make([]CustomAudience, 0)
	seen := map[string]struct{}{}
	seenCursors := map[string]struct{}{}
	cursor := ""
	path := "custom_audiences?count=" + strconv.Itoa(listPageSize)
	for page := 1; ; page++ {
		resp, rerr := c.requestPage(ctx, path, cursor)
		if rerr != nil {
			return nil, false, fmt.Errorf("list x custom audiences: %w", rerr)
		}
		els, derr := decodeCustomAudiencePage(resp)
		if derr != nil {
			return nil, false, fmt.Errorf("list x custom audiences: page %d: %w", page, derr)
		}
		for _, e := range els {
			// Validated RAW: an id is offered to a picker to be sent back verbatim.
			if !ValidCustomAudienceID(e.ID) {
				return nil, false, fmt.Errorf("list x custom audiences: %w: page %d: an audience has an unusable id", ErrCustomAudiencesMalformed, page)
			}
			if _, dup := seen[e.ID]; dup {
				return nil, false, fmt.Errorf("list x custom audiences: %w: page %d: the same audience was listed twice", ErrCustomAudiencesMalformed, page)
			}
			seen[e.ID] = struct{}{}
			if e.deleted() {
				continue
			}
			out = append(out, e.audience())
		}
		switch cursorVerdict(resp) {
		case cursorExhausted:
			return out, false, nil
		case cursorUnknowable:
			if len(els) < listPageSize {
				return out, false, nil
			}
			return nil, false, fmt.Errorf("list x custom audiences: %w: a full page came back with no usable cursor", ErrCustomAudiencesMalformed)
		}
		// A repeated cursor is checked BEFORE the page cap, so a looping answer on the last
		// allowed page is still refused rather than reported as a truncated list.
		if _, again := seenCursors[resp.NextCursor]; again {
			return nil, false, fmt.Errorf("list x custom audiences: %w: the list repeated a page cursor", ErrCustomAudiencesMalformed)
		}
		if page >= customAudienceMaxPages {
			return out, true, nil
		}
		seenCursors[resp.NextCursor] = struct{}{}
		cursor = resp.NextCursor
	}
}

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ---------------------------------------------------------------------------
// Ad-group keyword targeting: read + replace (LFXV2-2665)
//
// WHERE A REDDIT KEYWORD LIVES. In the ad group's `targeting` object, as the string array
// `keywords` — the field CreateCampaign sets from the brief's keywords (baseTargeting in
// client.go). A keyword there has no id and no status of its own: it can only be taken out of
// the list.
//
// THE WRITE REPLACES THE WHOLE OBJECT. There is no per-keyword endpoint; a change is a PATCH of
// the ad group carrying `targeting`, and secondary references to Reddit's v3 OpenAPI document
// state that "Targeting is replaced as a whole object" on update
// (https://glama.ai/mcp/servers/filippofinke/reddit-ads-mcp/tools/reddit_ads_update_ad_group,
// consulted 2026-10-06). The document itself (https://ads-api.reddit.com/api/v3/openapi.json)
// refuses automated fetches from the authoring environment, as bid_update.go records. So the
// write here sends back EVERY member of the targeting object exactly as it was read — raw bytes,
// never re-modelled — with only `keywords` changed. That is correct under either semantics
// (replace or merge); what it cannot rule out is a read representation the write interprets
// differently, which is why the dispatcher gates the write off by default and re-reads after it.
//
// CONCURRENCY. Reddit documents no ETag or conditional write for an ad group, so a
// read-modify-write cannot be made atomic upstream. The caller's half is Revision: a fingerprint
// of the whole targeting object, which the dispatcher compares against a fresh read before
// writing and refuses on any difference.
//
// LFX against LFX is closed in campaign-service, not here: the dispatcher holds a per-ad-group
// write lock (a Postgres advisory lock, shared by every replica — see
// dispatch.RedditDispatcher.lockRedditTargeting) from that fresh read through the PATCH and the
// re-read, so two LFX writes on one ad group (a removal and a negative add, say) cannot both pass
// the revision check before either PATCH lands. The second waits, then re-reads and sees the
// first's revision; if the wait runs out it is refused with nothing sent. When the first write's
// PATCH was SENT but its outcome is UNCONFIRMED (timed out, cancelled, a re-read that could not
// confirm it), the lock is kept for a further cooldown (domain.KeywordTargetingUnconfirmedCooldown,
// 15s) before the next LFX write may read: Reddit may still be applying that PATCH, and a write
// admitted at once would read the targeting as it is now, pass its check, and then be overwritten
// by the late PATCH from its stale snapshot. A finite cooldown narrows that window but cannot
// close it — nothing says when, or whether, Reddit finishes a request this service stopped
// waiting for — so a PATCH landing after the cooldown can still overwrite a later LFX write.
//
// What remains is an edit made OUTSIDE this service (Reddit Ads Manager, another API client)
// between the fresh read and the PATCH: the PATCH writes the targeting back as read and silently
// undoes it, and the re-read cannot tell — it sees exactly what was written. An outside edit that
// lands between the PATCH and the re-read is caught, and reported as UNCONFIRMED. That window is
// stated, not hidden; only a conditional write upstream would close it.
// ---------------------------------------------------------------------------

// ErrTargetingUnreadable marks an ad group whose targeting could not be read as an object with a
// keyword list. Refused rather than guessed at: a write built from a misread object would
// change the ad group's other targeting.
var ErrTargetingUnreadable = errors.New("reddit: the ad group's targeting is not a legible object with a keyword list")

// AdGroupTargeting is one ad group's targeting as GetAdGroupTargeting read it.
type AdGroupTargeting struct {
	AdGroupID string
	// CampaignID is the campaign Reddit reports the ad group under; "" when not reported.
	CampaignID string
	// Keywords is the `keywords` member, in Reddit's order; empty when absent or null.
	Keywords []string
	// Revision fingerprints the WHOLE targeting object (keywords included).
	Revision string

	// members is the targeting object member by member, raw bytes as Reddit sent them.
	members map[string]json.RawMessage
	// keywordElems is the `keywords` array element by element, raw, so a write keeps every
	// element it does not remove exactly as read (a null element included); nil when the
	// member was absent or null.
	keywordElems []json.RawMessage
}

// preEncodedBody is a request body that is already JSON and is sent byte for byte. The keyword
// write needs it: json.Marshal HTML-escapes '<', '>' and '&' inside every string — RawMessage
// members included — so a targeting member read as "a<b&c" would go back as "a\u003cb\u0026c".
// Semantically the same string, but not the bytes that were read.
type preEncodedBody []byte

// OtherDimensionsFingerprint fingerprints every targeting member EXCEPT `keywords`, so a re-read
// after a keyword write can prove nothing else moved.
func (t *AdGroupTargeting) OtherDimensionsFingerprint() (string, error) {
	return t.FingerprintExcept("keywords")
}

// FingerprintExcept fingerprints every targeting member except the named one, so a re-read after
// a write of that one member can prove nothing else moved.
func (t *AdGroupTargeting) FingerprintExcept(member string) (string, error) {
	rest := make(map[string]json.RawMessage, len(t.members))
	for k, v := range t.members {
		if k != member {
			rest[k] = v
		}
	}
	return targetingFingerprint(rest)
}

// targetingFingerprint is "sha256:" + the hex SHA-256 of the object's canonical JSON: decoded with
// UseNumber (so no number is rounded through a float) and re-encoded, which sorts every object's
// keys. Two reads of an unchanged object therefore agree whatever order Reddit sends members in.
func targetingFingerprint(members map[string]json.RawMessage) (string, error) {
	raw, err := json.Marshal(members)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// adGroupTargetingWire is the subset of an ad group this path reads. targeting stays raw so no
// member is lost or reshaped by a struct that does not know it.
type adGroupTargetingWire struct {
	ID         string          `json:"id"`
	CampaignID string          `json:"campaign_id"`
	Targeting  json.RawMessage `json:"targeting"`
}

// decodeTargeting turns a raw targeting object into its members, its keyword list and the raw
// keyword elements. A targeting that is absent, null or not an object, or whose `keywords` is not
// an array of strings (a null element is kept as-is and is not a keyword), is
// ErrTargetingUnreadable.
func decodeTargeting(raw json.RawMessage) (map[string]json.RawMessage, []string, []json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" || raw[0] != '{' {
		return nil, nil, nil, ErrTargetingUnreadable
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return nil, nil, nil, ErrTargetingUnreadable
	}
	keywords, elems, err := decodeStringArray(members["keywords"])
	if err != nil {
		return nil, nil, nil, err
	}
	return members, keywords, elems, nil
}

// decodeStringArray decodes one targeting member that is an array of strings (`keywords`,
// `excluded_keywords`): its string values in order, and its raw elements so a write can send back
// every element it keeps exactly as read. Absent or null is an empty list with nil elements; a
// null ELEMENT is kept raw and is not a value; anything else that is not an array of strings is
// ErrTargetingUnreadable.
func decodeStringArray(raw json.RawMessage) ([]string, []json.RawMessage, error) {
	values := []string{}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return values, nil, nil
	}
	if raw[0] != '[' {
		return nil, nil, ErrTargetingUnreadable
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, nil, ErrTargetingUnreadable
	}
	for _, e := range elems {
		e = bytes.TrimSpace(e)
		if string(e) == "null" {
			continue
		}
		if len(e) == 0 || e[0] != '"' {
			return nil, nil, ErrTargetingUnreadable
		}
		var v string
		if err := json.Unmarshal(e, &v); err != nil {
			return nil, nil, ErrTargetingUnreadable
		}
		values = append(values, v)
	}
	return values, elems, nil
}

// GetAdGroupTargeting reads one ad group's targeting via GET /ad_accounts/{account}/ad_groups/{id}
// — the read GetAdGroupBid makes. A PURE READ: its failure is always definite. A 404 is
// (nil, nil). A 2xx naming a different ad group is an error; a targeting that cannot be read as
// an object with a keyword list, or a body identityjson refuses (a duplicated key, malformed
// UTF-8, an unpaired surrogate escape), is ErrTargetingUnreadable.
func (c *Client) GetAdGroupTargeting(ctx context.Context, adGroupID string) (*AdGroupTargeting, error) {
	path, adGroupID, err := c.adGroupBidPath(adGroupID)
	if err != nil {
		return nil, err
	}
	resp, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("reddit: read ad group %s targeting: %w", adGroupID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("reddit: read ad group %s targeting: the response carried no ad group", adGroupID)
	}
	// The raw body, before any decode: a duplicated key (the data-level id, or "keywords" inside
	// targeting) would otherwise resolve last-wins, and the write-back would PATCH a targeting
	// built from one of two answers. Same guard as the campaign settings and lookup reads.
	if err := identityjson.Check(resp.raw); err != nil {
		return nil, fmt.Errorf("reddit: read ad group %s targeting: %w: %w", adGroupID, ErrTargetingUnreadable, err)
	}
	var wire adGroupTargetingWire
	if err := json.Unmarshal(resp.Data, &wire); err != nil {
		return nil, fmt.Errorf("reddit: read ad group %s targeting: the response is not an ad group object", adGroupID)
	}
	if got := strings.TrimSpace(wire.ID); got != adGroupID {
		if got == "" {
			got = "no ad group id"
		}
		return nil, fmt.Errorf("reddit: read ad group %s targeting returned %s instead", adGroupID, got)
	}
	members, keywords, elems, err := decodeTargeting(wire.Targeting)
	if err != nil {
		return nil, fmt.Errorf("reddit: read ad group %s targeting: %w", adGroupID, err)
	}
	rev, err := targetingFingerprint(members)
	if err != nil {
		return nil, fmt.Errorf("reddit: read ad group %s targeting: %w", adGroupID, ErrTargetingUnreadable)
	}
	return &AdGroupTargeting{
		AdGroupID:    adGroupID,
		CampaignID:   strings.TrimSpace(wire.CampaignID),
		Keywords:     keywords,
		Revision:     rev,
		members:      members,
		keywordElems: elems,
	}, nil
}

// RemoveAdGroupKeywords writes base's targeting back without the keywords in remove, via
// PATCH /ad_accounts/{account}/ad_groups/{id} with {"data":{"targeting":{...}}}, and returns the
// keywords the write leaves. Every other targeting member, and every keyword element it does not
// remove (a null one included), is sent exactly as base read it (compacted, never re-escaped:
// preEncodedBody). A keyword is removed when its value equals an entry of remove EXACTLY — no
// trimming, no case folding — and every occurrence of it goes. base MUST be the read of this
// same ad group the caller validated. A write that would leave no keyword is refused before any
// request: it would stop the ad group being keyword targeted at all.
//
// The PATCH is sent ONCE: a 429 is NOT retried. This write replaces the WHOLE targeting from a
// pre-read snapshot, so it does not converge the way a bid or budget write does — if a throttled
// attempt committed and an operator then changed another targeting dimension, a retry would
// overwrite that change, and the final comparison against the same stale snapshot would pass.
// A 429 is therefore UNCONFIRMED at once (IsOutcomeUnconfirmed), as are a transport failure, 3xx
// or 5xx; any other 4xx is a definite refusal — the ad group unchanged. A 2xx echo naming another
// ad group, or a keyword list other than the one left, is UNCONFIRMED.
func (c *Client) RemoveAdGroupKeywords(ctx context.Context, adGroupID string, base *AdGroupTargeting, remove []string) ([]string, error) {
	path, adGroupID, err := c.adGroupBidPath(adGroupID)
	if err != nil {
		return nil, err
	}
	if base == nil || base.members == nil || base.AdGroupID != adGroupID {
		return nil, fmt.Errorf("reddit: remove ad group %s keywords: no targeting read of this ad group to write back: %w", adGroupID, ErrTargetingUnreadable)
	}
	drop := make(map[string]bool, len(remove))
	for _, k := range remove {
		drop[k] = true
	}
	kept := make([]json.RawMessage, 0, len(base.keywordElems))
	remaining := make([]string, 0, len(base.keywordElems))
	for _, e := range base.keywordElems {
		if string(bytes.TrimSpace(e)) == "null" {
			kept = append(kept, e)
			continue
		}
		var k string
		if err := json.Unmarshal(e, &k); err != nil {
			return nil, fmt.Errorf("reddit: remove ad group %s keywords: %w", adGroupID, ErrTargetingUnreadable)
		}
		if drop[k] {
			continue
		}
		kept = append(kept, e)
		remaining = append(remaining, k)
	}
	if len(remaining) == 0 {
		return nil, fmt.Errorf("reddit: remove ad group %s keywords: refusing to leave the ad group with no keyword", adGroupID)
	}
	kw, err := encodeNoEscape(kept)
	if err != nil {
		return nil, fmt.Errorf("reddit: remove ad group %s keywords: %w", adGroupID, err)
	}
	targeting := make(map[string]json.RawMessage, len(base.members)+1)
	for k, v := range base.members {
		targeting[k] = v
	}
	targeting["keywords"] = kw
	encoded, err := encodeNoEscape(map[string]any{"data": map[string]any{"targeting": targeting}})
	if err != nil {
		return nil, fmt.Errorf("reddit: remove ad group %s keywords: %w", adGroupID, err)
	}

	resp, err := c.requestNoThrottleRetry(ctx, http.MethodPatch, path, preEncodedBody(encoded))
	if err != nil {
		return nil, fmt.Errorf("reddit: remove ad group %s keywords: %w", adGroupID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return remaining, nil
	}
	var echo adGroupTargetingWire
	if jerr := json.Unmarshal(resp.Data, &echo); jerr != nil {
		return nil, &transportError{Method: http.MethodPatch, Path: "ad group targeting", Err: fmt.Errorf("decode 2xx targeting update response for ad group %s: not an ad group object", adGroupID)}
	}
	if got := strings.TrimSpace(echo.ID); got != "" && got != adGroupID {
		return nil, &transportError{Method: http.MethodPatch, Path: "ad group targeting", Err: fmt.Errorf("targeting update for ad group %s was acknowledged for ad group %s", adGroupID, got)}
	}
	if t := bytes.TrimSpace(echo.Targeting); len(t) > 0 && string(t) != "null" {
		_, got, _, derr := decodeTargeting(t)
		if derr != nil || !SameKeywords(got, remaining) {
			return nil, &transportError{Method: http.MethodPatch, Path: "ad group targeting", Err: fmt.Errorf("targeting update for ad group %s was acknowledged with a keyword list other than the one sent", adGroupID)}
		}
	}
	return remaining, nil
}

// encodeNoEscape is json.Marshal without HTML escaping (and without the Encoder's trailing
// newline), so RawMessage values keep the bytes they were read with, compacted.
func encodeNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// SameKeywords reports whether a and b hold the same keywords the same number of times, in any
// order: Reddit is not documented to preserve the order it was sent.
func SameKeywords(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := make(map[string]int, len(a))
	for _, k := range a {
		count[k]++
	}
	for _, k := range b {
		count[k]--
		if count[k] < 0 {
			return false
		}
	}
	return true
}

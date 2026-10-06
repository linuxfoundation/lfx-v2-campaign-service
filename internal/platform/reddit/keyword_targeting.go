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
// writing and refuses on any difference. The window between that read and the PATCH remains and
// is stated, not hidden; the re-read afterwards is what reports a lost race as UNCONFIRMED.
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
}

// OtherDimensionsFingerprint fingerprints every targeting member EXCEPT `keywords`, so a re-read
// after a keyword write can prove nothing else moved.
func (t *AdGroupTargeting) OtherDimensionsFingerprint() (string, error) {
	rest := make(map[string]json.RawMessage, len(t.members))
	for k, v := range t.members {
		if k != "keywords" {
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

// decodeTargeting turns a raw targeting object into its members and keyword list. A targeting
// that is absent, null or not an object, or whose `keywords` is not an array of strings, is
// ErrTargetingUnreadable.
func decodeTargeting(raw json.RawMessage) (map[string]json.RawMessage, []string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" || raw[0] != '{' {
		return nil, nil, ErrTargetingUnreadable
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return nil, nil, ErrTargetingUnreadable
	}
	keywords := []string{}
	if kw, ok := members["keywords"]; ok {
		kw = bytes.TrimSpace(kw)
		if len(kw) > 0 && string(kw) != "null" {
			if kw[0] != '[' {
				return nil, nil, ErrTargetingUnreadable
			}
			var list []string
			if err := json.Unmarshal(kw, &list); err != nil {
				return nil, nil, ErrTargetingUnreadable
			}
			keywords = append(keywords, list...)
		}
	}
	return members, keywords, nil
}

// GetAdGroupTargeting reads one ad group's targeting via GET /ad_accounts/{account}/ad_groups/{id}
// — the read GetAdGroupBid makes. A PURE READ: its failure is always definite. A 404 is
// (nil, nil). A 2xx naming a different ad group is an error; a targeting that cannot be read as
// an object with a keyword list is ErrTargetingUnreadable.
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
	members, keywords, err := decodeTargeting(wire.Targeting)
	if err != nil {
		return nil, fmt.Errorf("reddit: read ad group %s targeting: %w", adGroupID, err)
	}
	rev, err := targetingFingerprint(members)
	if err != nil {
		return nil, fmt.Errorf("reddit: read ad group %s targeting: %w", adGroupID, ErrTargetingUnreadable)
	}
	return &AdGroupTargeting{
		AdGroupID:  adGroupID,
		CampaignID: strings.TrimSpace(wire.CampaignID),
		Keywords:   keywords,
		Revision:   rev,
		members:    members,
	}, nil
}

// ReplaceAdGroupKeywords writes base's targeting back with `keywords` set to keywords, via
// PATCH /ad_accounts/{account}/ad_groups/{id} with {"data":{"targeting":{...}}}. Every other
// member is sent exactly as base read it. base MUST be the read of this same ad group the caller
// validated; keywords must be non-empty (an empty list would stop the ad group being keyword
// targeted at all — the dispatcher refuses that before calling here, and so does this).
//
// Classification mirrors UpdateAdGroupBid: a transport failure, 3xx, exhausted 429 or 5xx is
// UNCONFIRMED (IsOutcomeUnconfirmed); ANY failure after a retried 429 is UNCONFIRMED
// (retriedUnconfirmedError); any other 4xx is a definite refusal — the ad group unchanged. A 2xx
// echo naming another ad group, or a keyword list other than the one sent, is UNCONFIRMED.
func (c *Client) ReplaceAdGroupKeywords(ctx context.Context, adGroupID string, base *AdGroupTargeting, keywords []string) error {
	path, adGroupID, err := c.adGroupBidPath(adGroupID)
	if err != nil {
		return err
	}
	if base == nil || base.members == nil || base.AdGroupID != adGroupID {
		return fmt.Errorf("reddit: replace ad group %s keywords: no targeting read of this ad group to write back: %w", adGroupID, ErrTargetingUnreadable)
	}
	if len(keywords) == 0 {
		return fmt.Errorf("reddit: replace ad group %s keywords: refusing to write an empty keyword list", adGroupID)
	}
	kw, err := json.Marshal(keywords)
	if err != nil {
		return fmt.Errorf("reddit: replace ad group %s keywords: %w", adGroupID, err)
	}
	targeting := make(map[string]json.RawMessage, len(base.members)+1)
	for k, v := range base.members {
		targeting[k] = v
	}
	targeting["keywords"] = kw
	body := map[string]any{"data": map[string]any{"targeting": targeting}}

	resp, retries, err := c.requestCounted(ctx, http.MethodPatch, path, body)
	if err != nil && retries > 0 && !IsOutcomeUnconfirmed(err) {
		return &retriedUnconfirmedError{what: "ad group targeting", retries: retries,
			err: fmt.Errorf("reddit: replace ad group %s keywords: %w", adGroupID, err)}
	}
	if err != nil {
		return fmt.Errorf("reddit: replace ad group %s keywords: %w", adGroupID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil
	}
	var echo adGroupTargetingWire
	if jerr := json.Unmarshal(resp.Data, &echo); jerr != nil {
		return &transportError{Method: http.MethodPatch, Path: "ad group targeting", Err: fmt.Errorf("decode 2xx targeting update response for ad group %s: not an ad group object", adGroupID)}
	}
	if got := strings.TrimSpace(echo.ID); got != "" && got != adGroupID {
		return &transportError{Method: http.MethodPatch, Path: "ad group targeting", Err: fmt.Errorf("targeting update for ad group %s was acknowledged for ad group %s", adGroupID, got)}
	}
	if t := bytes.TrimSpace(echo.Targeting); len(t) > 0 && string(t) != "null" {
		_, got, derr := decodeTargeting(t)
		if derr != nil || !SameKeywords(got, keywords) {
			return &transportError{Method: http.MethodPatch, Path: "ad group targeting", Err: fmt.Errorf("targeting update for ad group %s was acknowledged with a keyword list other than the one sent", adGroupID)}
		}
	}
	return nil
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

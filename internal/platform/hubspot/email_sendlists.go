// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ---------------------------------------------------------------------------
// What a prior send actually targeted (LFXV2-2770)
//
// The audience builder's strongest evidence for who an event should mail is who the
// LAST edition mailed. That is recorded on the email itself, under the same
// `to.contactIlsLists` object SetSendList writes — so this read and that write must
// stay on the same field, or the builder would report a precedent the sender never
// used.
//
// The legacy `to.contactLists` field is read too, and only for this purpose. It has
// been non-functional for SENDING since 2024-10-31 (which is why SetSendList refuses
// to emit it), but an email from a prior edition may well have been configured before
// that cut-off — and that historical selection is exactly what the operator is
// looking at. Ids from it are tagged as legacy by the caller, because a legacy list id
// does not resolve against the v3 Lists API.
// ---------------------------------------------------------------------------

// EmailSendLists is the recipient selection recorded on one marketing email.
type EmailSendLists struct {
	// PublishDate is when the email was sent, as HubSpot recorded it. Empty when the
	// email was never published.
	PublishDate string
	// Include and Exclude are ILS (v3) list ids.
	Include []string
	Exclude []string
	// LegacyInclude and LegacyExclude are ids from the pre-ILS `contactLists` field.
	// Kept apart from the v3 ids because they resolve through a different API, and
	// mixing them would produce rows reported as deleted when they exist perfectly
	// well under the legacy one.
	LegacyInclude []string
	LegacyExclude []string
}

// GetEmailSendLists reads one email's recipient selection. Read-only (idempotent).
//
// `includedProperties` is NOT used. It restricts the response to the named properties,
// and `to` is a nested object rather than a property — asking for it by name returns an
// email with an empty selection, which is indistinguishable from a send that targeted
// nothing.
func (c *Client) GetEmailSendLists(ctx context.Context, emailID string) (*EmailSendLists, error) {
	if emailID = strings.TrimSpace(emailID); emailID == "" {
		return nil, fmt.Errorf("hubspot: GetEmailSendLists requires a non-empty email id")
	}
	raw, err := c.doRequest(ctx, http.MethodGet, emailsPath+"/"+url.PathEscape(emailID), nil, true)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ID          string `json:"id"`
		PublishDate string `json:"publishDate"`
		// A POINTER, so an absent `to` is distinguishable from one that selected nothing.
		// As a value struct it decoded to a zero value, and a truncated 2xx such as
		// `{"id":"123"}` returned a successful email with no include or suppression lists —
		// precisely the "indistinguishable from a send that targeted nothing" outcome the
		// doc comment above guards `includedProperties` against. The individual selection
		// fields inside may still be absent; the OBJECT may not.
		To *struct {
			ContactIlsLists *idSelection `json:"contactIlsLists"`
			ContactLists    *idSelection `json:"contactLists"`
		} `json:"to"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("hubspot: decode email %s send lists: %w", emailID, err)
	}
	if resp.ID == "" {
		return nil, fmt.Errorf("hubspot: GetEmailSendLists(%s) returned a 2xx with no id (malformed response)", emailID)
	}
	if resp.To == nil {
		return nil, fmt.Errorf("hubspot: GetEmailSendLists(%s) returned a 2xx with no `to` object (malformed response)", emailID)
	}
	out := &EmailSendLists{PublishDate: strings.TrimSpace(resp.PublishDate)}
	// Each selection is REPORTED AS COMPLETE to the caller, who reads it as the audience a
	// previous send targeted. A blank element (encoding/json accepts `[1,null,3]` into
	// []json.Number with an empty middle value) would silently shorten that audience by one
	// list while still looking like a full answer — so it fails the read instead, the same way
	// ListMembershipIDs refuses a result with no recordId.
	if sel := resp.To.ContactIlsLists; sel != nil {
		if out.Include, err = numberIDs(sel.Include); err != nil {
			return nil, err
		}
		if out.Exclude, err = numberIDs(sel.Exclude); err != nil {
			return nil, err
		}
	}
	if sel := resp.To.ContactLists; sel != nil {
		if out.LegacyInclude, err = numberIDs(sel.Include); err != nil {
			return nil, err
		}
		if out.LegacyExclude, err = numberIDs(sel.Exclude); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// idSelection is HubSpot's include/exclude pair. The ids arrive as numbers on the
// legacy field and as strings on the ILS one, so they are decoded as json.Number.
type idSelection struct {
	Include []json.Number `json:"include"`
	Exclude []json.Number `json:"exclude"`
}

// numberIDs renders decoded ids as strings, preserving order, and REFUSES a blank one.
func numberIDs(nums []json.Number) ([]string, error) {
	out := make([]string, 0, len(nums))
	for _, n := range nums {
		id := strings.TrimSpace(n.String())
		if id == "" {
			return nil, fmt.Errorf("hubspot: email send list selection contained a blank list id; the selection cannot be reported as complete")
		}
		out = append(out, id)
	}
	return out, nil
}

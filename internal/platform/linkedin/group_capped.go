// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ErrCampaignGroupScheduleConflict marks a create refused because the event's shared campaign
// group cannot bound a daily-capped lifetime campaign (#584): either this campaign is such a
// campaign and the group ends later than it, or never; or the group holds one and this create
// would have to widen the group's end, extending that campaign's flight with it.
//
// A daily cap on a lifetime budget is created with LinkedIn's continuous schedule — no
// runSchedule.end on the campaign — so the group's end is the only thing that stops it serving.
// The group is shared by every campaign of an event and project ("Events | <event> | <project>"),
// and its name is parsed by the data pipeline (#587), so it is not renamed per campaign; these
// refusals are what keep the group's end and the capped campaign's end the same instant instead.
//
// Raised before any write, it is also a domain.PreflightRefusal carrying
// domain.ErrCreateConfigInvalid (a 400 wherever that sentinel is mapped; on the asynchronous
// create path, the job's per-platform error verbatim with the claim released). Raised after this
// call's own widening write — only when another writer moved the group's end past this
// campaign's in between — it is a plain error: something was written, so it is not a refusal.
var ErrCampaignGroupScheduleConflict = errors.New("linkedin: the event's campaign group schedule cannot bound a daily-capped lifetime campaign")

// groupCampaignSearchPageSize is the adCampaigns search page size for the capped-campaign check.
// One page covers every realistic group; the walk still follows the cursor.
const groupCampaignSearchPageSize = 1000

// cappedSearchIgnoredStatuses are the campaign statuses that can never serve again, so a capped
// campaign in one of them is not extended by widening its group. Everything else counts —
// ARCHIVED, DRAFT and COMPLETED included — because each can be (re)activated in Campaign Manager
// and would then serve to the group's new end.
var cappedSearchIgnoredStatuses = map[string]struct{}{
	"CANCELED":         {},
	"REMOVED":          {},
	"DELETED":          {},
	"PENDING_DELETION": {},
}

// groupCampaignElement is the part of an adCampaigns search element the capped check reads. The
// budgets stay raw: only their presence matters, and a null reads as absent.
type groupCampaignElement struct {
	ID            flexibleID      `json:"id"`
	Status        string          `json:"status"`
	CampaignGroup string          `json:"campaignGroup"`
	DailyBudget   json.RawMessage `json:"dailyBudget"`
	TotalBudget   json.RawMessage `json:"totalBudget"`
	RunSchedule   *struct {
		End *int64 `json:"end"`
	} `json:"runSchedule"`
}

func rawPresent(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

// findUnboundedCappedCampaignInGroup returns the id of a campaign in group groupID that carries
// both totalBudget and dailyBudget and no runSchedule.end — a daily-capped lifetime campaign whose
// flight only the group's end bounds — or "" when the group holds none.
//
// One adCampaigns search filtered server-side by the group's URN (the walk follows the cursor, but
// a group is far smaller than a page). It fails closed: an incomplete walk, an untrusted page, an
// element of another group (the filter was not honoured) or an unreadable element is an error,
// never "none", because the caller answers "none" by widening the group.
func (c *Client) findUnboundedCappedCampaignInGroup(ctx context.Context, accountID, groupID string) (string, error) {
	nestedPath := fmt.Sprintf("adAccounts/%s/adCampaigns", accountID)
	groupURN := "urn:li:sponsoredCampaignGroup:" + groupID
	seen := map[string]struct{}{}
	pageToken := ""
	for page := 0; page < maxListPages; page++ {
		params := map[string]string{
			"q":        "search",
			"search":   "(campaignGroup:(values:List(" + restliEncode(groupURN) + ")))",
			"pageSize": strconv.Itoa(groupCampaignSearchPageSize),
		}
		if pageToken != "" {
			params["pageToken"] = pageToken
		}
		var raw json.RawMessage
		if err := c.doMonitorGET(ctx, nestedPath, params, &raw); err != nil {
			return "", fmt.Errorf("search campaign group %s's campaigns: %w", groupID, err)
		}
		if err := identityjson.Check(raw); err != nil {
			return "", fmt.Errorf("search campaign group %s's campaigns: %w", groupID, err)
		}
		var resp struct {
			Elements *[]json.RawMessage `json:"elements"`
			Metadata *linkedInMetadata  `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil || resp.Elements == nil {
			return "", fmt.Errorf("search campaign group %s's campaigns: the response is not a campaign page", groupID)
		}
		for _, elRaw := range *resp.Elements {
			var el groupCampaignElement
			if err := json.Unmarshal(elRaw, &el); err != nil {
				return "", fmt.Errorf("search campaign group %s's campaigns: an element is not a campaign of the expected shape", groupID)
			}
			if el.CampaignGroup != "" && trailingID(el.CampaignGroup) != groupID {
				return "", fmt.Errorf("search campaign group %s's campaigns: the search returned a campaign of another group, so its group filter cannot be trusted", groupID)
			}
			if _, gone := cappedSearchIgnoredStatuses[el.Status]; gone {
				continue
			}
			if !rawPresent(el.DailyBudget) || !rawPresent(el.TotalBudget) {
				continue
			}
			if el.RunSchedule != nil && el.RunSchedule.End != nil && *el.RunSchedule.End > 0 {
				continue
			}
			id := trailingID(el.ID.String())
			if id == "" {
				id = "(unknown id)"
			}
			return id, nil
		}
		if resp.Metadata == nil {
			return "", fmt.Errorf("search campaign group %s's campaigns: the response carried no metadata block, so the search could not be confirmed complete", groupID)
		}
		next := resp.Metadata.NextPageToken
		if next == "" {
			return "", nil
		}
		if _, dup := seen[next]; dup {
			return "", fmt.Errorf("search campaign group %s's campaigns: pagination returned a repeated page token", groupID)
		}
		seen[next] = struct{}{}
		pageToken = next
	}
	return "", fmt.Errorf("search campaign group %s's campaigns: exceeded %d pages", groupID, maxListPages)
}

// groupEndText renders a group end for a refusal: an RFC 3339 instant, or "no end".
func groupEndText(end *int64) string {
	if end == nil {
		return "no end"
	}
	return "an end of " + time.UnixMilli(*end).UTC().Format(time.RFC3339)
}

// scheduleConflict builds an ErrCampaignGroupScheduleConflict error. Before any write (wrote
// false) it is a PreflightRefusal carrying ErrCreateConfigInvalid; after this call's own write it
// is a plain error (see ErrCampaignGroupScheduleConflict). The text is service-authored: the group
// id goes through domain.CallerValue, and the instants are derived from integers.
func scheduleConflict(wrote bool, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if wrote {
		return fmt.Errorf("%w: %s", ErrCampaignGroupScheduleConflict, msg)
	}
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: %w: %s", domain.ErrCreateConfigInvalid, ErrCampaignGroupScheduleConflict, msg))
}

// cappedGroupEndRefusal refuses a daily-capped lifetime campaign whose reused group ends later
// than the campaign, or never: the campaign has no end of its own, so it would serve to the
// group's end.
func cappedGroupEndRefusal(wrote bool, groupID string, groupEnd *int64, endMs int64) error {
	return scheduleConflict(wrote,
		"the event's LinkedIn campaign group %s has %s, later than this campaign's end %s; a daily-capped lifetime campaign (linkedInConfig.dailyBudgetUsd) has no end of its own and serves until its campaign group ends, so it could overrun its end date. Set the campaign group's end to %s in Campaign Manager, or drop dailyBudgetUsd; nothing was created",
		domain.CallerValue(groupID), groupEndText(groupEnd), time.UnixMilli(endMs).UTC().Format(time.RFC3339), time.UnixMilli(endMs).UTC().Format(time.RFC3339))
}

// cappedSiblingWidenRefusal refuses to widen a group that holds a daily-capped lifetime campaign:
// moving the group's end would move that campaign's end with it.
func cappedSiblingWidenRefusal(groupID, campaignID string, groupEnd *int64, endMs int64) error {
	return scheduleConflict(false,
		"the event's LinkedIn campaign group %s has %s, before this campaign's end %s, and holds daily-capped lifetime campaign %s, which has no end of its own and stops serving when the group ends; widening the group would extend that campaign's flight. End this campaign on or before the group's end, or end or move campaign %s in Campaign Manager first; nothing was changed or created",
		domain.CallerValue(groupID), groupEndText(groupEnd), time.UnixMilli(endMs).UTC().Format(time.RFC3339), domain.CallerValue(campaignID), domain.CallerValue(campaignID))
}

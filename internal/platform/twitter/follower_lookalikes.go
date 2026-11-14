// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Follower look-alike targeting at create (#564 gap 4c): twitterConfig.followerLookalikeUserIds,
// each sent as one SIMILAR_TO_FOLLOWERS_OF_USER criterion (operator EQ) on the line item this
// create makes. X's v12 targeting reference lists the type as "Followers of a user and users
// similar to them" ("up to 100 Users"), and its example targeting_value is a numeric user id
// ("14230524").
//
// NOT LOOKED UP BEFORE CREATE. The Ads API has no user lookup this client could read, so only the
// id's shape is checked up front. An id X refuses fails at its own criterion POST, after the
// campaign and line item exist and before any tweet: the create stops there with the PAUSED
// campaign and line item returned as a retained partial, exactly as for any other criterion
// failure (CreateCampaign Step 3b, ErrCreateTargetingIncomplete). The owner accepted that risk on
// 2026-11-14.

const (
	// FieldFollowerLookalikeUserIDs is the twitterConfig key, named in refusals.
	FieldFollowerLookalikeUserIDs = "followerLookalikeUserIds"
	// MaxCreateFollowerLookalikes caps the list on its own. X allows up to 100 users per line item;
	// the per-create total (MaxCreateTargetingCriteria, 60) bounds it together with every other
	// criterion, so the per-list cap is that total.
	MaxCreateFollowerLookalikes = MaxCreateTargetingCriteria
	// TargetingSimilarToFollowersOfUser is X's targeting_type for a follower look-alike criterion.
	TargetingSimilarToFollowersOfUser = "SIMILAR_TO_FOLLOWERS_OF_USER"
)

// xUserIDRe is the shape of an X user id: a positive decimal number of 1-19 digits with no
// leading zero (user ids are snowflakes, or older small integers, and always fit an int64).
var xUserIDRe = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// NormalizeFollowerLookalikes applies the gap 4c rules (#564): each id is trimmed and must be a
// decimal X user id (1-19 digits, no leading zero, within int64); duplicates are dropped (first
// kept) and a blank entry refused; at most MaxCreateFollowerLookalikes. Pure, so the dispatcher's
// synchronous preflight and CreateCampaign apply the same rules. A nil or empty list stays nil.
func NormalizeFollowerLookalikes(ids []string) ([]string, error) {
	const shapeMsg = "must be a numeric X user id (1-19 digits, no leading zero)"
	out, err := normalizeTargetList(FieldFollowerLookalikeUserIDs, ids, MaxCreateFollowerLookalikes, func(s string) string { return s }, xUserIDRe, shapeMsg)
	if err != nil {
		return nil, err
	}
	// The 19-digit shape still admits values above the largest int64; X ids never exceed it.
	for i, id := range out {
		if _, perr := strconv.ParseInt(id, 10, 64); perr != nil {
			return nil, &TargetingError{Field: FieldFollowerLookalikeUserIDs, Msg: fmt.Sprintf("entry %d is out of range for an X user id", i)}
		}
	}
	return out, nil
}

// followerLookalikeCriteria turns the normalized ids into the criteria to POST, in request order.
// The label names the public user id, which is not caller free text.
func followerLookalikeCriteria(ids []string) []createCriterion {
	out := make([]createCriterion, 0, len(ids))
	for _, id := range ids {
		out = append(out, createCriterion{targetingType: TargetingSimilarToFollowersOfUser, value: strings.TrimSpace(id), label: "follower look-alike " + id})
	}
	return out
}

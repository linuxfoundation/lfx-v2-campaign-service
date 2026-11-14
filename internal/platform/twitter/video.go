// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Video creative for an AUTHORED promoted-only tweet (#564 gap 7): twitterConfig.videoAssetId, a
// stored MP4 creative asset.
//
// It takes the same three moves as an image (media.go), with media_category amplify_video (X's
// category for a promoted video) and the Media Library's VIDEO type:
//
//  1. the X API v2 chunked upload: initialize (media_type video/mp4, total_bytes, media_category
//     amplify_video), one append per 4 MiB segment, finalize, then the read-only STATUS command
//     while X reports processing_info — at most maxVideoStatusPolls checks, each wait capped at
//     mediaPollCap (5 s), so at most 30 s of waiting, inside the two-minute provider call;
//  2. the media_key added to the ad account's Media Library under the deterministic SHA-256 name;
//  3. that key sent as media_keys on every authored tweet, or as the MEDIA component of the website
//     card (#564 gap 6) when one is requested.
//
// RETRY SAFETY. All of it runs BEFORE the campaign POST, so any failure is (nil, err): nothing
// billable exists and no tweet was published. The library entry is looked up by name before any
// byte is sent, so once it exists a retry reuses it and never uploads the video twice; an upload
// that stopped before the library add (including processing that did not finish within the
// bound) left only an upload session, which expires on its own, and the retry starts a fresh one.
// Tweets are authored only after the campaign and line item exist, once per create, exactly as for
// an image, so a video never causes a second tweet.
//
// BUDGET. A video's writes (initialize, up to eight appends, finalize, the library add) and its
// processing wait share the two-minute dispatch budget with the targeting criteria, so a create
// with a video may name at most MaxCreateTargetingCriteriaWithVideo criteria;
// TestCreateTargetingCriteriaFitTheDispatchBudget pins the arithmetic.
//
// NOT VERIFIED LIVE: TWITTER_VIDEO_ENABLED keeps it off the shared ad account until a live create
// confirms amplify_video processing time and the attachment.

const (
	// MaxVideoBytes is the largest video this client uploads: the creative-asset store's own
	// per-row ceiling (30 MiB). X accepts far larger amplify_video uploads; the bound is ours.
	MaxVideoBytes = 30 << 20
	// maxVideoStatusPolls bounds the STATUS checks after a video's finalize (6 x 5 s = 30 s).
	maxVideoStatusPolls = 6
	// MaxCreateTargetingCriteriaWithVideo caps the targeting criteria of a create that uploads a
	// video, whose writes and processing wait need the room the full 60 would take.
	MaxCreateTargetingCriteriaWithVideo = 30
	// FieldVideoAssetID is the twitterConfig key, named in refusals.
	FieldVideoAssetID = "videoAssetId"
)

// videoMedia is the creative video's kind.
var videoMedia = mediaKind{noun: "creative video", category: "amplify_video", libraryType: "VIDEO", maxPolls: maxVideoStatusPolls}

// VideoInput is a creative video for the authored tweet. Bytes are read, never retained or
// mutated; MIME is the stored asset's verified type.
type VideoInput struct {
	Bytes []byte
	MIME  string
}

// validateVideoInput is the up-front check CreateCampaign runs before any request: bytes present,
// within MaxVideoBytes, declared video/mp4, and opening with an ftyp box (the store checked the
// whole structure at upload; this re-check keeps the client correct for another caller).
func validateVideoInput(v *VideoInput) error {
	if len(v.Bytes) == 0 {
		return fmt.Errorf("invalid creative video: no video bytes")
	}
	if len(v.Bytes) > MaxVideoBytes {
		return fmt.Errorf("invalid creative video: %d bytes exceeds the %d-byte video limit", len(v.Bytes), MaxVideoBytes)
	}
	if v.MIME != "video/mp4" {
		return fmt.Errorf("invalid creative video: type %s is not video/mp4", domain.MIMEValue(v.MIME))
	}
	if len(v.Bytes) < 8 || string(v.Bytes[4:8]) != "ftyp" {
		return fmt.Errorf("invalid creative video: bytes are not an MP4 file")
	}
	return nil
}

// CheckCreateCriteriaCountWithVideo is CheckCreateCriteriaCount for a create that uploads a video.
// Pure, so the dispatcher's synchronous preflight and CreateCampaign apply the same bound.
func CheckCreateCriteriaCountWithVideo(n int) error {
	if n <= MaxCreateTargetingCriteriaWithVideo {
		return nil
	}
	return fmt.Errorf("a create with %s may name at most %d targeting criteria, since the video's upload and processing share the dispatch budget; this one names %d",
		FieldVideoAssetID, MaxCreateTargetingCriteriaWithVideo, n)
}

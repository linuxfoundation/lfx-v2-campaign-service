---
type: "Architecture Doc"
title: "Reddit Video Posts"
description: "Reddit VIDEO posts from stored assets (#549 gap 5): redditConfig.videoAssetId (a stored video/mp4) and thumbnailAssetId (a stored PNG/JPEG) author a VIDEO post whose video and thumbnail Reddit fetches from late-minted signed URLs, the video at a kind=video URL that serves only video/mp4; built from Reddit's public v3 post schema, off unless REDDIT_VIDEO_ENABLED, and the post shape needs a live probe before the flag goes on."
resource: "internal/dispatch/reddit_video.go"
---

# Reddit Video Posts

#549 gap 5, decided by the owner on 2026-11-14. Before it, a Reddit create could author only an
IMAGE post (`imageUrl` / `imageAssetId`); the `video_views` objective could serve only through a
hand-made `postUrl`.

## Request

`redditConfig.videoAssetId` names a creative asset of the brief stored as `video/mp4` (the
upload path from #586: `internal/service/creative_asset_video.go`, migration `000057`).
`redditConfig.thumbnailAssetId` names a stored PNG or JPEG of the same brief. The two are set
together and never with `postUrl`, `imageUrl` or `imageAssetId`.

The thumbnail is **required, not derived**. Reddit's post schema (`components_schema_post`) says
`thumbnail_url` is "required for video posts", and this service has no video decoder to cut a
frame. Reddit's generated-poster job API could make one, but it is asynchronous and unprobed.

## Post shape

The same legacy `POST /profiles/{account}/posts` the image path uses (`createPromotedPost`):

```json
{"data": {"type": "VIDEO", "headline": "...", "allow_comments": false,
          "thumbnail_url": "<signed image URL>",
          "content": [{"media_url": "<signed video URL>", "call_to_action": "Sign Up",
                       "destination_url": "<UTM'd registration URL>"}]}}
```

In the schema, `media_url` is "required for image, video and carousel posts". The destination is
required only for image and carousel posts, but it is sent anyway: a video ad that does not link
to registration is no use to an LF event. The headline and CTA rules are the image post's. The
structured-post endpoint's `VideoCreative` (`video.media.url` plus `thumbnail.media.url`) is
the other documented shape. It is **not** used, because the image path does not use it either.

## Serving the media

Both URLs are signed creative-asset URLs ([Signed Creative-Asset URLs for
Reddit](signed-creative-asset-urls.md)), minted by the client immediately before the one post
request (video first, then thumbnail):

- the video at a **`kind=video`** URL. The kind is bound into the MAC through its own label, and
  the route serves such a URL only if the stored asset is `video/mp4`;
- the thumbnail at an ordinary image URL, which serves PNG/JPEG only and never a video.

An image slot therefore still cannot obtain a video. An image URL for a stored MP4 is the generic
404, and adding `kind=video` to it breaks the signature. Both URLs are scrubbed from the job
error, the steps and the ad warning. The config snapshot keeps both asset ids, and the snapshot
dangling-asset guard reads `videoAssetId` and `thumbnailAssetId`.

## Validation

- **Pre-flight (synchronous 400, `domain.WrapPreflightRefusal` +
  `domain.ErrCreateConfigInvalid`)** is `checkRedditVideoChoice`. It refuses: the flag off (named
  in the message, and checked first), one field without the other, a mix with another creative,
  signed asset URLs not configured, a non-UUID id, and the same id twice. It runs on the
  best-effort decode, like the other gated Reddit inputs.
- **Dispatch** runs the same rules again, then `checkRedditVideoAssets` before any Reddit call:
  each asset must exist for the brief under an active brief and have bytes, the video must be
  `video/mp4`, the thumbnail PNG/JPEG, and the ids must fit a signed URL. Every failure is
  pre-create, so the claim is released.
- **Client** (`reddit.CampaignInput.VideoURLSource` / `ThumbnailURLSource`): it refuses one
  source without the other, or a video with a post URL or an image, before any request. A source
  error or a malformed URL is `(nil, err)`.

## Partial results and retry safety

Nothing changes from the image path. The post is authored **before** the paid campaign:

- a definite refusal of the post (for example, Reddit rejecting the unprobed shape) degrades to a
  campaign and ad group with no ad and an `AdWarning`. That is `created_degraded`, the claim is
  retained, and the operator follows the stray-post guidance;
- an in-flight cancellation is an UNCONFIRMED partial, recorded by name;
- a cancellation before send is `(nil, err)`, with nothing created.

A retry of a degraded row short-circuits on idempotency, as it does for images.

## Deployment gate

`REDDIT_VIDEO_ENABLED` (default off; the chart sets `"false"`). It names no account resource. It
keeps an unverified post shape off the shared ad account.

## Needs a live probe before the flag goes on

- Reddit accepts `type: VIDEO` with `content[0].media_url` plus top-level `thumbnail_url` on the
  legacy profile-posts endpoint, or requires the structured-post `VideoCreative`.
- Reddit's fetcher downloads a 20-30 MiB MP4 from the signed route within one post request: no
  Range requests the route does not honour, and no fetch arriving after the URL's TTL, if
  ingestion is asynchronous.
- Whether post creation returns before video processing, and what a later processing failure
  looks like on the post and the ad.
- Thumbnail requirements (size, aspect ratio) and whether a CTA and destination are accepted on a
  VIDEO post for every objective, `video_views` included.

# 2026-10-26 — Google creative-asset review fixes (PR #394)

**Fix** — four findings from the post-approval Copilot review of
`feat/LFXV2-2665-google-asset-ids`, landed as one commit.

**The per-image cap now runs on the dispatch side, before the blob is loaded.**
`resolveGoogleCreativeAssets` refuses an asset larger than `googleads.MaxImageBytes` immediately
after `GetAssetSize` and before `reserve` or `GetAsset`, so an oversized image is never
materialised and takes no allowance. The LinkedIn, X and Microsoft dispatch paths in the same
package already did exactly this against their own `<platform>.MaxImageBytes`; Google's new
stored-asset path was the only sibling missing it, which left a 5 MiB-plus asset to be loaded,
charged and only then refused by the platform client. The refusal deliberately does **not** copy
the siblings' wording: theirs names a genuine vendor limit, while `googleads.MaxImageBytes` is this
service's own per-image memory bound and Google's asset limit is larger, so it says "this service
handles at most …" and a test pins that clause.

**`maxDemandGenImageBytes` was renamed and exported as `googleads.MaxImageBytes`.** The old name
was a misnomer, not just unexported: `fetchSlotImages`, `fetchOneImage` and `validateImageBytes`
are shared by Demand Gen, Display and Performance Max alike, and every slot in
`ImageSlotCatalog` carries the same cap. Thirteen sites across five files; no duplicate literal
is left in `internal/dispatch`, so the two halves of the cap cannot drift.

**The snapshot URL reduction is unconditional.** `sanitizeSnapshotImageRefs` keyed its exemption
on `namesAsset()`, so an entry carrying both a signed URL and an asset id had the URL written to
the unencrypted `config_snapshot` unreduced. Such an entry is refused at resolve time, but
refusal and persistence are different paths. The exemption belongs to the id **field**, so the id
is still left alone and every URL is reduced; `redact.SnapshotURL("")` returns `""`, so the
asset-only entries that exist in practice round-trip unchanged.

**`CampaignInput.ResolvedImages` is now covered end to end.** It is assigned at exactly one site
in `internal/dispatch/googleads.go`, and deleting that line left both existing suites green while
every stored-asset campaign failed at the `assets:mutate`. `googleads_resolved_images_wiring_test.go`
dispatches a Demand Gen config naming stored assets over the fake API server and asserts the
stored bytes appear base64-encoded in the `assets:mutate` body, that the asset ids do not, and
that the reserver is whole afterwards; a second case asserts an oversized asset is refused with
no campaign created and `GetAsset` never called. One channel, because all three share the single
assignment and `fetchSlotImages` below it.

**`asset_admission.go`'s budget enumeration was corrected** to cover the Google path: the
`assets:mutate` body is NOT zero and is NOT covered by the semaphore — Google has no multipart
upload, so each image is base64-encoded and marshalled into one JSON body. What bounds those
transient copies is the pair of platform caps (`googleads.MaxImageBytes` per image,
`maxCreativeTotalImageBytes` across the creative), both of which run on the pre-resolved path too.
Bounding serialization itself would mean streaming the mutate body, a platform-client design
change and not this control's business.

Each fix was mutation-tested: neutering `ResolvedImages`, disabling the per-image check, and
restoring the `namesAsset()` guard each turned exactly the intended test(s) red and nothing else.

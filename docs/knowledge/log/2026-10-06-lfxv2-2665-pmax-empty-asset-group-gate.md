# 2026-10-06 — LFXV2-2665 Performance Max activation refused on an empty asset group

**Fix** — The Google activation gate treated a Performance Max asset group ID as proof the
campaign could serve. It is not. `createPerformanceMaxAssetGroup` returns the group ID even
when the `assetGroupAssets:mutate` link step fails — deliberately, because an empty group is
visible in the Google Ads UI, looks finished, and is the state an operator most needs to find —
so exactly the campaign that cannot deliver was the one `googleAdsActivationGate` called
provisioned. It un-paused the campaign and reported success, which is the false success
`domain.ErrCampaignNotProvisioned` exists to prevent.

`CampaignResult` now carries `AssetGroupAssetLinks`, the number of links Google CONFIRMED.
`pmax.go` records it on BOTH arms and records it even when it is zero; `googleAdsToggleAssetGroup`
decodes it from the same `Result` blob as the ID and hands it to the gate, which refuses a
recorded zero with a message naming the asset group and pointing at the Google Ads UI.

The field is a `*int`, and that is the load-bearing detail. **Nil is not zero.** Every Performance
Max campaign created before this field has no such key in its blob, and reading absence as zero
would refuse activation on campaigns that are provisioned correctly — an over-refusal, which is
the one failure mode these guards must never have. Nil activates; a recorded zero refuses. An
UNCONFIRMED link mutate (a 2xx with a short or malformed mutate response) records zero rather
than nothing: links may exist, this client cannot say they do, and "cannot say" belongs in the
UI rather than in a claimed launch. PAUSE is untouched throughout — the gate runs only on
ACTIVATE, because refusing to pause is refusing to stop spend.

Tests were added on both sides of the boundary, because dispatch tests alone would not have
held: they build the `Result` blob by hand and would stay green if the create path stopped
writing the field. `pmax_test.go` now asserts the count from the real cascade on three paths —
the happy path (one per confirmed link), a definite link failure (a recorded zero), and a short
link response (a recorded zero for the ambiguous case). `googleads_toggle_channels_test.go`
covers the gate itself: zero links refuses, seven links activates, a blob with no recorded count
activates, and zero links still PAUSES.

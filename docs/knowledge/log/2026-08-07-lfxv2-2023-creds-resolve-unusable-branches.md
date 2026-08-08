# 2026-08-07 — LFXV2-2023: two creds.resolve branches still answered 503

**Update** — `credsSource.resolve` now classifies its two permanent failure branches with
`domain.ErrConnectionNotUsable`, closing the last path by which a pre-send setup failure reached
the service layer's default 503 arm.

**Fix** — The previous change classified the setup failures `resolveGoogleAdsDiscoveryClient`
raises itself, but `credsSource.resolve` sits UNDER it and raises two of its own that belong in the
same bucket: a connection row with an empty credential blob, and one whose blob will not decrypt.
Both mean the row needs editing and neither improves with time, yet both reached the service layer
untagged and landed in its default arm — a 503 telling the caller to retry something that cannot
succeed. Both now wrap `domain.ErrConnectionNotUsable`.

The other two branches deliberately stay untagged, and the reasoning is the whole point of doing
this per-branch rather than at the `resolve` call site: `domain.ErrNotFound` means there is no
connection at all, so the operator should CREATE one (404, not 400), and a repository failure is a
genuine "try again later" (503). A blanket wrap around `resolve` would have flattened all four and
destroyed both distinctions while appearing to fix the bug.

The decrypt branch wraps two errors (`%w: %w`) — the sentinel for classification and the cause for
the log. The cause is not returned to the caller: a decrypt failure can carry ciphertext detail, so
the service layer logs it and answers with a fixed message, the same rule already applied to the
credential-unmarshal path.

Both were revert-verified separately; each fails naming the exact untagged error it produced.

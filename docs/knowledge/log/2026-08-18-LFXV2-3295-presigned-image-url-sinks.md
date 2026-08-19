# 2026-08-18 — LFXV2-3295 pre-signed image URLs reaching persisted sinks

**Fix** — two privacy defects raised by dealako on #144, both in the class "a caller-supplied
URL reaching persisted or API-reachable storage". A creative image URL may be PRE-SIGNED, and a
signature is a bearer credential granting time-boxed read access.

**The success path was never covered.** The previous round scrubbed the four *error* Steps sinks
and stopped there. `campaignFromMeta` passed the raw `metaConfig` to `applyCampaignConfig`, which
JSON-marshals it into `config_snapshot` — a column persisted UNENCRYPTED — with each variant's
`ImageURL` query and signature intact. This fires on EVERY successful create carrying an image,
not on failures, so no amount of error-path scrubbing could have reached it. The fix reuses
`sanitizeSnapshotURL` (`internal/dispatch/creds.go`), the helper `campaignFromReddit` already
applies to `PostURL` for exactly this reason, rather than adding a parallel one.

The variants slice is COPIED before scrubbing. `cfg` is passed by value but `Variants` shares its
backing array with the caller's config, so an in-place scrub would strip the signature from the
live Meta request too — the full URL must still reach Meta, only the persisted copy is sanitized.
Reverting that copy fails `TestMeta_ConfigSnapshotSanitizeDoesNotMutateCallerConfig` rather than
silently breaking image delivery.

**A redactor that fails open is not a redactor.** `scrubURLFromErr` removed the URL by exact
substring replacement — verbatim and percent-encoded. Neither form matches once the text is
mangled, and these paths mangle it routinely: `do` truncates a non-Graph error body at 300 runes,
which can clip the URL mid-query and leave `sig=SECRET_SIG_ABC` where the value was
`sig=SECRET_SIG_ABCDEF`. Reproduced before fixing: the clipped signature was emitted verbatim
into a persisted Step. It now verifies the result carries no residue of the URL's query/fragment
(`urlSecretResidueFree`) and withholds the message — keeping only the redacted URL — when it
cannot confirm the text is clean. Prefixes are checked, not just whole tokens, because truncation
clips from the right; the scheme/host/path are deliberately NOT checked, since `redactURL` leaves
them so the step still says WHICH image failed.

**One claim in the report was falsified.** `config_snapshot` was described as API-reachable.
`campaignResult` (`internal/service/brief.go`) maps only id/project/brief/platform/name/status/
version/etag — `ConfigSnapshot` reaches no API response, and a code comment on the update path
says as much ("the GET response doesn't expose config"). The defect stands on the at-rest ground
alone: the column is unencrypted, and a credential does not belong there regardless of who can
read it back over HTTP. Recorded so a later reader does not inherit the stronger claim.

**Sinks swept and found clean**, so the coverage is auditable rather than assumed: the other six
`applyCampaignConfig` callers (googleads, linkedin, microsoft, twitter, hubspot carry no URL field
in their configs; reddit already sanitizes `PostURL`); `linkedin.CreativeVariant.ImageURN` is a
URN, not a URL, and `reddit.AdVariant` carries no URL at all; every `Steps` line in the reddit,
twitter and meta clients that names a URL already renders through `redactURL` or a
`display*UTMURL` allowlist helper; `validateVariantImageURL` reports only the variant index, never
the URL; and the sole `url` log attributes in the repo (`internal/infrastructure/auth/jwt.go`)
carry `redact.URL(req.URL)`, not a raw value.

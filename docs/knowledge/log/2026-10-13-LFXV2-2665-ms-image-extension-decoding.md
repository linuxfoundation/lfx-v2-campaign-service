# 2026-10-13 — Microsoft image-extension responses are decoded strictly

**Fix** — seven late review threads on #307. All are in
`internal/platform/microsoft/image_extension.go`, and each one could read an unanswered or
misattributed 2xx as success:

- AddAdExtensions: `NestedPartialErrors` is `BatchErrorCollection[]`, now decoded with
  `boundedNestedErrorItems` at both levels. A null identity with a nested rejection is definite.
  An identity returned with nested failures is UNCONFIRMED rather than success.
- Association lookup:
  - An omitted, `null` or non-array `AdExtensionAssociationCollection` now fails the lookup
    instead of reading as "no extension", which used to cause a second upload.
  - `EntityId` and `AssociationType` are decoded. An association that is not this campaign at
    Campaign level fails closed.
- AddMedia: `PartialErrors` beside `MediaIds` is decoded, so an explained null id is a definite
  rejection.
- SetAdExtensionsAssociations: a 2xx with `PartialErrors` omitted (`{}` or `null`) is UNCONFIRMED.
  The failure message no longer says "the extension exists but is not attached" under an
  UNCONFIRMED classification.
- #312 review: error EVIDENCE is now separate from TRUNCATION. `boundedNestedErrorItems` gained
  a whole-wire `AnyErrors` tally. A null identity becomes a definite rejection only when some
  real code is present (retained or not). Truncation or null-only slots stay UNCONFIRMED, since
  calling them definite invites a retry that creates a second extension. A Set body whose
  `PartialErrors` is present but has no codes (`[null]`) is UNCONFIRMED, not confirmed. The
  concept's lookup wording now says that non-image associations are skipped.
  Every image-extension response body now passes `identityjson.Check` before it is decoded.
  The presence probes read exact keys while the struct decode is case-folded and keeps the
  last duplicate, so a duplicated or case-folded key could mark the collection present and
  decode it empty, which licensed a second upload. An untrustworthy lookup body fails the
  lookup; an untrustworthy mutation body is UNCONFIRMED. AddMedia, like the extension create,
  now classifies a null id on whole-wire `PartialErrors.AnyErrors`, so a rejection past the
  16-entry window is definite.
  The lookup no longer returns at the first valid image association. Every image association
  is validated, and the first valid id is returned only after all of them pass. A later foreign
  or unusable entry makes the lookup fail closed, so a mixed response can no longer read as
  AlreadyAttached.

Concept: [Microsoft Creative Image Assets](../architecture/microsoft-creative-assets.md).

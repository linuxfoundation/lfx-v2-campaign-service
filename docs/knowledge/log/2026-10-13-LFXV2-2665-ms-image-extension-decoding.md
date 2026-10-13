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

Concept: [Microsoft Creative Image Assets](../architecture/microsoft-creative-assets.md).

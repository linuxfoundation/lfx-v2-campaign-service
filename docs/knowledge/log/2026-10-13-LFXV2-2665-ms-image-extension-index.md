# 2026-10-13 — Microsoft image lookup needs one collection; errors must name our item

**Fix** — two late review threads on #312, both in
`internal/platform/microsoft/image_extension.go`:

- The association lookup now requires exactly one `AdExtensionAssociationCollection`,
  because Microsoft returns one per requested entity id and one id is sent. A top-level `[]`
  answered for no campaign but used to read as "none", which could allow a duplicate AddMedia.
  It now fails the lookup, as do two or more collections. A null slot or an empty
  `AdExtensionAssociations` inside the one collection still means none.
- A coded `BatchError` counts as a definite rejection only when its `Index` is 0, the one item
  sent, and no coded entry names any other index. An absent, nonzero or out-of-range index makes
  the 2xx contradictory, so the outcome is UNCONFIRMED instead of "NOT attached". This applies
  to AddMedia, AddAdExtensions (attributed by the collection's `Index`) and
  SetAdExtensionsAssociations. New whole-wire decoders, `imageErrors` and `imageNestedErrors`,
  keep `Index` and tally evidence for our item and for other items across the entire array.
- #323 review: the UNCONFIRMED messages no longer take codes from the retained prefix. That
  prefix drops `Index`, so when index-0 entries filled it they were reported as "naming another
  item". The decoders now collect the foreign entries' codes, with their index, over the whole
  wire, and the association, AddMedia and AddAdExtensions messages name only those. The
  concept's AddMedia, AddAdExtensions and Set bullets now state the index-0 rule and
  `imageNestedErrors` in place of the earlier any-error and `boundedNestedErrorItems` wording.
- #323 comment pass, comments only with no behaviour change. Every comment in
  `image_extension.go` now states the current rules:
  - Only a single collection answers the lookup, so `[]` is no longer described as "none".
  - The comments on `adExtensionIdentity`, `mediaIdentity` and the file's OUTCOMES block now
    give the index-0-exclusive rule in place of "any coded error is definite".
  - The `NestedPartialErrors` field comment names `imageNestedErrors` instead of the geo decoder.
  - `AnyErrors` is documented as "coded at any index", which is not rejection evidence by
    itself.

Concept: [Microsoft Creative Image Assets](../architecture/microsoft-creative-assets.md).

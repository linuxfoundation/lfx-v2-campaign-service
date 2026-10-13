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

Concept: [Microsoft Creative Image Assets](../architecture/microsoft-creative-assets.md).

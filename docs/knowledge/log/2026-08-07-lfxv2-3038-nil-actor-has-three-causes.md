# 2026-08-07 — LFXV2-3038: a nil actor has three causes, and the comment named two

**Update** — The doc comment on `Brief.CreatedBy` / `UpdatedBy` gave an exhaustive reading of
nil: either the row predates actor attribution, or the write was system-initiated. Both are
benign, and stating them as the complete set makes nil look benign by definition.

**Fix** — There is a third cause. `attributedActor` also returns nil when a request principal
WAS present but could not be decoded — it logs a warning and persists NULL rather than
failing the write. That case is a regression signal: a real person made the write and the
audit trail lost them. Someone investigating missing attribution against the old comment
would have concluded the row was simply old or system-written and stopped. The comment now
enumerates all three and says explicitly to correlate with the decode warning before
concluding the absence is normal.

**Note** — The failure mode here is a comment that is exhaustive in FORM ("either … or") while
being incomplete in fact. An open-ended phrasing would have been wrong in the same way but
would not have foreclosed the investigation.

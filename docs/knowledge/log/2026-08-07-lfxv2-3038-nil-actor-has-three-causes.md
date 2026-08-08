# 2026-08-07 — LFXV2-3038: a nil actor has three causes, and the comment named two

**Update** — The doc comment on `Brief.CreatedBy` / `UpdatedBy` gave an exhaustive reading of
nil: either the row predates actor attribution, or the write was system-initiated. Both are
benign, and stating them as the complete set makes nil look benign by definition.

**Fix** — There is a third cause. `attributedActor` also returns nil when a request principal
WAS present but could not be decoded — it logs a warning and persists NULL rather than
failing the write. That case is a regression signal: a real person made the write and the
audit trail lost them. Someone investigating missing attribution against the old comment
would have concluded the row was simply old or system-written and stopped.

**Fix** — A first attempt at that correction pointed operators at a "could not decode request
principal" warning. No such warning exists. `attributedActor` emits ONE message — "write
attempted with no authenticated actor" — for every nil, and the request context carries only
the decoded actor, with no record of whether a token was present and undecodable versus
absent entirely. The distinction is genuinely unobservable inside this service. The comment
now says so, and points at what does separate the cases: gateway/ingress evidence of an
Authorization header, and the SHAPE of the warning rate (steady trickle = ordinary
unauthenticated traffic; step change across every write = the auth path broke).

**Note** — Two failure modes here, and the second was self-inflicted. The original comment was
exhaustive in FORM ("either … or") while incomplete in fact, which foreclosed the
investigation. The fix for it then invented a diagnostic signal to correlate against, which
would have foreclosed the investigation in a new place — an operator grepping for a log line
that is never emitted concludes the case did not occur. A correction that names a specific
observable must be checked against the code that emits it, not against what the code ought to
emit.

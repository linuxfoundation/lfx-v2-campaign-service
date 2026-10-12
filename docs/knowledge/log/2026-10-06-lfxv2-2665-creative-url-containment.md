# 2026-10-06 — LFXV2-2665 Google Ads creative-URL containment and asset-mutate ambiguity

**Fix** — The caller-supplied creative image URL was treated as a secret on the way
into Postgres (`sanitizeSnapshotURLs` reduces it to scheme+host before
`config_snapshot`) and as plain text everywhere else. Three sinks were open and all
three are now closed.

- **Error text.** Thirteen refusal and fetch-failure sites in
  `internal/platform/googleads/demandgen_creative.go` now render the URL through
  `redactURLForError`, which keeps scheme+host+path and drops the query — the half of
  a signed CDN URL that IS the credential. These errors are persisted unencrypted as
  Steps entries, so the query string surviving in one is a disclosure, not a cosmetic
  issue.
- **The wrapped cause.** `net/http` and `url.Parse` both return `*url.Error`, whose
  `Error()` prints the full request URL, query included — so `%w` put the URL back
  into the message a second time however carefully the format arguments were
  redacted. New `redactedCause` (`ad_copy.go`) renders the operation and the inner
  cause only, while `Unwrap` keeps the chain intact so `errors.Is`/`As` and the
  create-path ambiguity classification behave exactly as before.
- **The asset label.** Performance Max image assets were named
  `"<slot> <source URL>"`, exporting the caller's URL to Google as a permanent
  human-visible label in the shared Foundation account — redacted going into this
  service's own database, verbatim going out. The name is now omitted entirely,
  matching the Demand Gen sibling and letting Google name the asset.

**Update** — Three further changes in the same round:

- `maxCreativeTotalImageBytes` (64 MiB) caps the SUM of a creative's images as a
  RUNNING total during the fetch loop. The per-image 5 MiB cap bounded one file and
  nothing bounded the set, so a 30-image Performance Max asset group could hold well
  over a hundred megabytes in one process, base64-expanded into a single
  `assets:mutate` body. The bound is deliberately generous — refusing a create Google
  would have accepted is the worse failure of the two.
- A 5xx or timeout on the Demand Gen `assets:mutate` now reports **UNCONFIRMED**
  rather than failed. It was the one `assets:mutate` on a create path asserting
  "failed" over an outcome nobody knows; an operator who believed it would retry into
  a second set of account-level image assets.
- `knownBiddingStrategies` is built as the UNION of the per-channel sets rather than
  aliased to the Search one, and the unknown-name check now runs AFTER the per-channel
  switch so its message advertises the set that actually applies
  (`supported on <kind>: ...`). The alias read correctly only while Search happened to
  be a superset of every channel; listing every known name answered a Demand Gen typo
  with names the next guard refuses, costing the caller two round trips for one
  mistake.

Docs: `docs/knowledge/code/internal-platform-googleads.md` description extended, with
the `docs/knowledge/code/index.md` bullet kept verbatim equal to it.

# 2026-10-13 — X media shares the asset budget, serializes uploads, and requires a reported state

**Fix** — three late review threads on #311 (X Ads creative image assets).

- `bindCreativeAssetSharing` rebound Meta and Microsoft to the container's single
  `AssetReserver`, but not the X dispatcher. X uploads therefore charged a second budget, and
  the process-wide asset-memory bound did not hold. X is now rebound on both live paths. The
  wiring test checks that Meta, Microsoft, X and the signed-asset route all hold that one
  instance (`UsesAssetReserver` / `UsesBudget`). LinkedIn, whose creatives (#315)
  landed alongside, is rebound and asserted the same way.
- `ensureImageMedia`'s lookup, upload and library add were not coordinated, so two concurrent
  dispatches of identical bytes to one account could both upload and both add. They now run
  under a per-(account, image) lock that every client for the account shares through the
  `AccountPacers` registry. The lock is a context-bounded semaphore, and its entries are
  dropped when unused.
- `decodeMediaState` treated a missing or null `data` body, and an empty processing state, as
  "ready". Those are now an UNCONFIRMED unreadable 2xx (missing data, or a STATUS that drops
  `processing_info` after X reported processing) or a state to keep polling (empty or unknown,
  refused at the poll bound). Only `succeeded`, or a finalize with no `processing_info`, is
  ready.

See [X Ads Creative Image Assets](../architecture/x-creative-assets.md).

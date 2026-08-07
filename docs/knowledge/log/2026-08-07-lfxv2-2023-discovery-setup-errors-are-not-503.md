# 2026-08-07 — LFXV2-2023: discovery setup failures were heading for a 503

**Update** — Added `domain.ErrConnectionNotUsable` and wrapped every pre-send failure in
`resolveGoogleAdsDiscoveryClient` with it. Copilot flagged this on the service PR stacked above
this one, but the fix belongs here: the sentinel and the wrapping live in the layer that knows the
failure happened before any request existed, and a status mapping alone in the layer above has
nothing to key on.

**Fix** — The account-discovery handler's default arm answers 503. Three conditions were reaching
it: an inactive connection, a credential blob missing an OAuth field, and a `login_customer_id`
stored with dashes. A 503 is a promise that waiting might help; none of these change until someone
edits the connection, so the operator is told to retry forever and the one thing that would let
them fix it is buried. They now carry `ErrConnectionNotUsable` and the service layer maps it to
400. `creds.resolve` failures are deliberately left unwrapped — that layer separates
`ErrNotFound` (404) from a genuine storage failure (503), and flattening both into "not usable"
would destroy the distinction.

**Fix** — The dashed manager id was the case that forced the check to move. `Client`
already validates it, but inside the same call that talks to Google — so by the time it fails, the
error is indistinguishable at the dispatch boundary from a real upstream failure and gets
classified as retryable. `storedCustomerIDRE` now checks the stored value where it is read. The
client keeps its own copy as the backstop for every other caller; the two regexps must stay in
step. The revert-verification is what shows the duplication earns its keep: with the dispatch
guard disabled the test still fails, but on the client's unwrapped error — a 503 wearing the right
words.

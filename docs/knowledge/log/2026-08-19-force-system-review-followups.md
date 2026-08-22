# 2026-08-19 — force-system-ads-account review follow-ups

**Fix** — three reviewer findings on the force-system-ads-account branch, all against code
added during review rather than the original change. Each was verified before being acted
on, and the two that touch behaviour were mutation-checked.

## A dead guard inside the test written to catch a defect

`meta_test.go`'s httptest handler opened with
`strings.HasPrefix(r.URL.Path, "/act_") && !strings.Contains(r.URL.Path, "/")`, which can
never be true: an httptest request path always begins with `/`, so `Contains(path, "/")`
holds unconditionally and the negation is always false. Confirmed empirically rather than
by reading — a throwaway probe printed the predicate for the preflight path and all three
nested edges, and it was `false` for every one.

The account preflight was in fact already served by the following arm
(`strings.Count(strings.Trim(path, "/"), "/") == 0`), because `url.URL.Path` excludes the
query string: `GET /act_<id>?fields=name,account_status,currency` arrives as the bare
`/act_<id>`. That arm returns `{"currency":"USD","id":"..."}` — a superset of the dead
arm's currency-only body.

The arm was **dropped**, not made reachable. Making it reachable would split one working
case into two, and the `/act_` prefix carries no distinction the handler needs: the
preflight and the campaign-node GET both want a single-segment response, and the currency
is what `CreateCampaign` requires before any mutating call.

The removal was then checked against the mutation the test exists to catch — deleting
`InstagramUserID: cfg.InstagramUserID` from `meta.go`'s `CampaignInput` literal, which
still compiles. `TestMeta_ConfigFieldsReachTheWire` still fails under it
(`instagram_user_id on the wire = <nil>`), so the arm was **not** load-bearing: it was
scaffolding that read as coverage it never provided.

## The forced path's transient-failure arm had no test

`resolveForcedSystem` has two adjacent fail-closed arms one line apart: `ErrNotFound` (no
system row is installed — permanent, remedy is "install the LF connection") and everything
else (`systemOrigin(connLoadFailed(...))` — the store failed to answer, transient, the row
may be readable on the next attempt). Only the first was covered.

The seam this needed already existed: `scopedConnReader` carries an `errs` map keyed by
project id, the same one the FALLBACK path's "system lookup fails" case uses. Injecting at
`model.SystemProjectID` with the flag on is what routes it to the forced path. No fake had
to be extended.

Two compiling mutations confirm the new test binds what it claims:

- dropping the `systemOrigin` wrapper on that line still returns a non-nil, not-created
  error — only the `ErrSystemConnectionOrigin` assertion catches it;
- re-tagging the arm with the absence shape still carries system origin *and*
  `NoUpstreamCreate` — only the NEGATIVE `ErrNotFound` assertion catches it.

That negative is the point: it is the single property separating this arm from the one
directly above it, and without it a mis-classification would attribute a transient store
failure to a misconfiguration that may not exist.

## A README analogy that attached the wrong lifecycle

The bullet described `LFX_FORCE_SYSTEM_ADS_ACCOUNT` as "Read once at startup like
`REDDIT_METRICS_ENABLED`". The first half is true (`newCredsSource`, `creds.go:167`); the
comparison is not. `REDDIT_METRICS_ENABLED` is read per call inside
`RedditDispatcher.ReadMetrics` (`reddit.go:337`), deliberately — its comment says the gate
sits there "so a deployment can flip it without a rebuild". The two flags differ on
precisely the read-once property the sentence claimed they shared, which misleads anyone
asking the operational question: does flipping the env at runtime take effect?

Rewritten to keep only the property they genuinely share — the exact-match `true` parse —
and to state each lifecycle separately: this flag needs a restart, the Reddit one does not.

## Left open deliberately

Three older review threads on the same branch remain open pending a reviewer decision, and
were not answered in code: the `ErrNotFound` classification ordering across the three
service paths plus the async dispatch log, toggle/metrics behaviour for campaigns created
before the flag is flipped, and the spec's reversibility claim for account discovery. All
three are design/rollout questions whose two options are materially different products,
not defects with a single correct fix.

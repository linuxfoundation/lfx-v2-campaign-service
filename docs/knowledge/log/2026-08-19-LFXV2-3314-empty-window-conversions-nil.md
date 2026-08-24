# 2026-08-19 — LFXV2-3314 an empty Google window reported conversions as unmeasured

**Fix** — `googleads.GetCampaignMetrics` has two success paths and only one of them
honoured the invariant the `CampaignMetrics.Conversions` field documents three lines
above itself:

    // Google can, so this adapter never leaves it nil:
    // metrics.conversions is always selected, and proto3 JSON omits default
    // values, so an absent member is a measured 0.0 and is reported as a
    // non-nil zero.
    Conversions *float64 `json:"conversions,omitempty"`

The non-nil zero was materialised only at the row-decoding site, after `rows[0]` is
unmarshalled. The earlier `len(rows) == 0` branch returned a zero-value struct, and a
zero-value POINTER is nil — so the invariant was false on that path.

That path is not an error path. Google Ads omits a campaign from GAQL results
entirely when it had no impressions in the window, which is why the method's own
doc-comment already calls a no-activity window "not an error". So the branch means
the query ran and the measurement was zero. Returning nil there published the
opposite claim: `nil` is reserved, across every adapter, for "this platform cannot
measure conversions at all".

The consequence lands on the rule this ticket added. `no_conversions` refuses to fire
on a nil count — deliberately, so it never flags Meta, X, Reddit or email, none of
which report conversions. A Google campaign with genuinely zero delivery therefore
serialised as unmeasured and the rule stayed silent for exactly the campaign it
exists to catch: one that delivered nothing and converted nobody.

The shipped test could not catch it. `TestGetCampaignMetrics_NoActivityInWindowReturnsZeroValue`
asserted Impressions, Clicks, CostMicros and Ctr — four value-typed fields, all of
which a zero-value struct satisfies honestly — and never asserted `Conversions`. It
passed before the fix and would have passed after it. No other test asserted the old
nil-on-empty behaviour, so nothing had to be rewritten; the gap was the missing
assertion, not a wrong one.

**Verification** — the fix reverted to `return &CampaignMetrics{CampaignID: id, Window: w}, nil`
compiles, and the extended test then fails:

    --- FAIL: TestGetCampaignMetrics_NoActivityInWindowReturnsZeroValue (0.00s)
        metrics_test.go:125: Conversions = nil for a no-activity window; Google
        measured this window and got zero, and nil is reserved for platforms that
        cannot report conversions at all

**Docs** — three contract corrections rode along, all from suppressed review comments.

The generated `CampaignMetrics` object example advertised a combination no adapter
can produce: `conversions: 37` beside an `email` object. Goa emits every attribute
into a generated object example, and `email` and `conversions` are mutually
exclusive — the email channel is one of the channels that never reports conversions.
Deleting the attribute-level `Example` does not help; Goa then fabricates a random
double (`0.2109820735060166`), a worse claim than a wrong one. The type now carries
an EXPLICIT email-shaped object example that omits `conversions` entirely, which is
the contract's own statement about that channel. The per-property `Example(37.0)` is
kept: in the property schema no `email` sits beside it to contradict it.

The `conversions` description named only Meta, X, Reddit and email as sources of an
absent value. Microsoft belongs there too, and not as a platform-wide "cannot": a
blank cell anywhere in a present `ConversionsQualified` column withdraws the whole
total, and the column is absent entirely for accounts not wired for Universal Event
Tracking. Both conditions are now disclosed.

`CampaignActionItem.rule` is now pinned to `Example("budget_constrained")`. It was
previously unpinned, and Goa's auto-selection is a function of the whole design
rather than of the attribute — this change moved it. Pinning it also corrected a
real defect one level up: the nested `BriefMetrics.action_items` example was emitting
`rule: no_conversions` on `platform: reddit-ads`, a finding that cannot exist,
because Reddit's conversion count is always absent and the rule never fires on
absence. `CampaignActionItem`'s own example was `budget_constrained` and was not the
one at fault; the array example was.

Table rows 139-141 of `docs/api-catalog.md` were structurally broken, carrying 7, 8
and 7 pipes where a five-column row needs 6. Each had been edited by appending a
revised cell after an extra `|` instead of replacing the old one, so the rendered
table grew phantom columns and kept both generations of the prose. The revisions were
spliced into single five-cell rows, keeping the newer text where the two disagreed:
the account-identity claim is now "every adapter that verifies tenant identity",
matching the six paid-ads adapters plus HubSpot that raise
`domain.ErrCampaignAccountMismatch`, and the stale "Google Ads and HubSpot, the only
two adapters" and "Two reasons are Google-Ads-only on this path" fragments are gone.

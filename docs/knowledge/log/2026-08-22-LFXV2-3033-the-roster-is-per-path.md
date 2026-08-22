# 2026-08-22 — LFXV2-3033 the cache-bypass roster is per-PATH, not per-provider

**Fix** — the two documents that are the single source of truth for which paths
bypass `clientCache` — the roster comment on `clientCache` in
`internal/dispatch/credcache.go` and the client-cache section of
`docs/knowledge/code/internal-dispatch.md` — both omitted an active bypass:
`GoogleAdsDispatcher.Dispatch` builds its client inline with
`googleads.NewClient` rather than through `cachedGoogleAdsClient`. Both
documents therefore implied that Google Ads creates reuse a cached token across
jobs. They do not; the create path re-mints an OAuth token per campaign.

**Why the omission was plausible, which is the part worth keeping.** The roster
listed Google Ads as *wired*, and that is true — but "wired" is a claim about a
PROVIDER, while every bypass on the list is a claim about a PATH. Google Ads is
wired on exactly one entry point, `resolveGoogleAdsClient`, serving
`ToggleStatus` and `ReadMetrics`. Microsoft is the opposite shape:
`MicrosoftDispatcher.Dispatch` DOES go through `cachedMicrosoftClient`
(`microsoft.go:183`). So a reader checking "is this provider wired?" got the
right answer and the wrong conclusion, and the two providers disagreeing about
their create paths is what made the gap survive two tickets.

**The enumeration, since a partial sweep is what caused this.** `googleads.NewClient`
has three call sites, and only listing all three makes the roster checkable:

| Site | Reached from | Cached? | Was it documented? |
| --- | --- | --- | --- |
| `Dispatch` | `Dispatch` (create) | **bypass** | **no — this entry adds it** |
| `googleAdsClientFor` | `cachedGoogleAdsClient` → `ToggleStatus`, `ReadMetrics` | **cached** | yes |
| `googleAdsClientFor` | `resolveOwnedGoogleAdsClient` → `LookupCampaign` (adoption) | **bypass** | yes — one-shot, not a polling loop |
| `resolveGoogleAdsDiscoveryClient` | `ListAccounts` | **bypass** | yes — account-agnostic, empty CustomerID |

`googleAdsClientFor` is the one that resists a grep-and-count: it is a single
construction site reached two ways, one cached and one not, so the site count
(three) and the path count (four) differ. Counting sites would have said "two of
three documented"; counting paths says three of four.

**Pinned rather than described.** `TestClientCache_GoogleAdsDispatchBypassesTheCache`
asserts the token endpoint is hit TWICE across two dispatches of one unchanged
connection. It is deliberately an assertion of the current, unwired behaviour:
the day someone wires the create path, that test fails and is the prompt to
update both rosters in the same change. It mirrors
`TestClientCache_MicrosoftDispatchUsesTheCachedClient`, which asserts 1 for the
provider whose create path IS cached.

Mutation-verified with a COMPILING revert: replacing the inline
`googleads.NewClient(...)` in `Dispatch` with `d.cachedGoogleAdsClient(...)`
(keeping `creds` and `loginCustomerID` referenced so it still builds) turns the
count to 1 and fails the test — `token endpoint hit 1 times ... want 2`. No
survivor.

**A test that bypassed the helper built to protect it.**
`TestClientCache_MicrosoftProbeStaysOnTheStub` re-spelled all four of
`newMicrosoftCacheDispatcher`'s URL overrides inline, purely because it also
needed a custom `http.Client`. That put the ONE test whose entire job is to
catch a missing reporting override outside the helper whose comment says it
"exists so the reporting override cannot be forgotten in one test and silently
reintroduce the live call".

Measured, and the numbers are the finding: deleting `WithReportingBaseURL` from
the helper left that guard test GREEN (exit 0) while the package's Microsoft
tests went from 1.3s to 5.0s — the extra 3.7s being real DNS and TCP to
`reporting.api.bingads.microsoft.com`. The guard could not see the escape it
exists to detect, because it was not using the thing being mutated.

The helper now takes variadic `extra ...microsoft.Option`, appended after the
origin overrides, so a test can ADD an option without restating them. With the
test routed through it, the same mutation fails loudly: `the Microsoft probe
dialled a non-loopback address 1 time(s)`. `TestClientCache_MicrosoftDispatchUsesTheCachedClient`
still constructs inline because it splits token and API across two servers,
which the single-server helper cannot express; it now carries a comment saying
so and still points all three API origins at its stub.

**A sibling fragment was missing its kind marker, and was corrected.**
`2026-08-20-LFXV2-3033-client-cache-test-binding.md` opened its body with plain
prose and no bold kind marker, which CLAUDE.md:27-34 requires after the H1
(`**Update**`, `**Fix**`, `**Creation**`, `**Note**`, `**Verification**` or
`**Docs**`, followed by an em dash). It now opens `**Fix** —`, the accurate marker
for an entry recording four review findings and the fixes that bind them.

The "never edit another entry's file" rule does not protect it: THIS PR adds that
file, so correcting it here is the only chance to correct it at all — a point raised
in review, and right. An earlier draft of this section recorded the deviation and left
the file alone, which would have shipped a knowledge bundle whose own log documented a
rule its files break.

Worth keeping for anyone relying on the gate: `okfvalidate` does NOT check the kind
marker — it exits 0 on a file without one — so the rule is enforced by review alone,
which is how it drifted. Two fragments already on `main`
(`2026-08-20-LFXV2-2643-a-check-that-cannot-see-what-it-verifies.md` and
`2026-08-20-LFXV2-2643-assertions-that-can-fail-correct-code.md`) are still missing it;
those belong to other entries and are left alone, but a validator rule would stop the
drift rather than relying on a reviewer noticing.

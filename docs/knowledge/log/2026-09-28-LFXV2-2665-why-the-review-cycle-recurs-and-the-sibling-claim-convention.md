# 2026-09-28 — LFXV2-2665: why the review cycle recurs, and the sibling-claim convention

**Docs** — no behaviour change. One section added to the local-pre-PR-review architecture doc,
one comment rewrapped.

Ten rounds of local review on this branch produced no production-code defect. Every finding was
comment or doc truthfulness, and seven of the ten were the same class. That is worth writing down
rather than absorbing as the cost of doing business.

The structural half is inherent and fine: a fix cycle reruns with the ORIGINAL base, so each rerun
re-reads the whole change rather than the fix alone, and a clean round needs three independently
sampling reviewers to be silent at the same time. Several rounds is the shape of the cycle.

The avoidable half is the author's. **A uniqueness or counting claim in a comment is a claim about
every sibling in the repo** — "the one platform", "the only probe", "the strongest of the six" —
and it goes stale when any sibling changes, in a file the change never touched. Round 28 was
exactly that: `reddit.Client.VerifyAccount` called Reddit the only probe reading its configured
account directly; true when written, made false by adding X's probe three files away. No sweep
keyed on "what did this commit touch" reaches it.

So: cross-cutting facts are stated once, in `docs/api-catalog.md`, the roster of record. A comment
says what is true of the code it sits on, and where it must place that against siblings it names
**the reason the relationship holds** rather than re-enumerating the roster — a claim with its
reason attached is falsifiable where it stands, instead of only against a count kept elsewhere.

The second avoidable half is that each round starts blind, so a deliberately declined finding comes
back. Decisions are carried forward in `--extra` on every rerun. That ended a four-round recurrence
of the Microsoft HTTP 400 finding, and has held for five rounds since.

A sweep of every comment added across `9691f752..HEAD` for this class found the surviving sites
already correct and already reasoned: the direct-probe pair in `reddit/probe.go` and
`twitter/probe.go` name each other and say why; `OrgReferenceVerifier`'s "only LinkedIn" and
`ErrSettingsReadbackUnsupported`'s "only Google Ads" were both verified against the actual
implementor sets, as was "the other six" in `ConnectionService`. One overlong comment line in
`twitter/probe.go` was rewrapped; nothing else needed changing.

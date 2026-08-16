# 2026-08-16 — LFXV2-3064 LinkedIn "both halves" claim corrected

**Docs** — Three sites claimed LinkedIn was eligible to join `accountDiscoveryProviders`.
It is not: it has the discovery endpoint this ticket added, but not the tagging half.

The bar for adding a provider is a conjunction — an endpoint that enumerates the accounts a
credential reaches, AND a failure on the path that needs the id which NAMES the missing choice.
`docs/api-catalog.md` asserted LinkedIn had both and cited `resolveLinkedInCredentials` as the
tagging site. That function is real and does tag `domain.ErrAccountNotSelected`, which is what
made the claim survive a reading: the named symbol exists and does what it is said to do. The
error is one level up — `LinkedInDispatcher.Dispatch` never calls it. Its three call sites are
`ToggleStatus`, `resolveLinkedInDiscoveryCredentials` and `ReadMetrics`; the create path
resolves the connection inline and answers a missing account id with a bare `notCreated`, so
adding LinkedIn to the map today would still produce an unclassified create failure.

Microsoft's identical-looking claim IS true, and checking it is what isolates the difference:
`MicrosoftDispatcher.Dispatch` calls `validateMicrosoftConnection` directly, and that is where
the sentinel is attached. Same sentence shape, opposite answer, because the question is which
PATH reaches the tagging — not whether a tagging function exists.

The two sibling sites (`design/connection.go`, `docs/knowledge/code/internal-bootstrap.md`) were
wrong in the other direction, still saying all four remaining providers "lack discovery" — true
before this ticket, falsified by it for LinkedIn and Microsoft. All three now say the same thing:
Microsoft has both halves, LinkedIn has only the first, Reddit and X have neither.

**Note on verification.** The first attempt to check this grepped
`resolveLinkedInCredentials` in `internal/dispatch/linkedin.go`, found a call at line 208, and
nearly concluded the reviewer was wrong. Line 208 is inside `ToggleStatus`; `Dispatch` spans
81–202. A call site in the same FILE is not a call site on the same PATH, and a grep that
returns hits answers a weaker question than the one being asked.

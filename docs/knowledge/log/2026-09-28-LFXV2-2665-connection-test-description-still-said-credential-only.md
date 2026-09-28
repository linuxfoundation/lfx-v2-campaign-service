# 2026-09-28 — LFXV2-2665: the connection-test description still said credential-only

**Docs** — round 19 of review, raised by the repo-code reviewer. No behaviour change.

Two pieces of prose outlived what this branch did to the code under them.

The Goa method description for all seven `test-<platform>` endpoints read "Verify the stored
`<Platform>` credential against the provider." That was true before this branch and is not true
after it: a green result now means the credential authenticated AND the account the connection
names passed that provider's own check — the conjunction `TestResult.ok` already documents. The
description is the API reference an operator reads, so understating it there makes a green
result look like it says less than it says. It now reads "Verify the stored `<Platform>`
credential and the configured account against the provider", with a comment in `design/`
recording that the second clause is deliberate and that how deep the account check goes stays
provider-specific. `gen/**` and the two embedded kodata OpenAPI copies were regenerated.

`internal-dispatch.md` said of a bootstrap connection that `active` does not mean the
credentials were verified — "nothing verifies them". The first half is still exactly right and
is the point of the passage; the second half stopped being true the moment this branch landed
endpoints that do verify. The passage now says nothing on the WRITE path verifies them, names
`ProbeConnection` behind the test endpoints as the thing that does, and keeps the conclusion
intact: nothing runs those endpoints for you, so `active` still carries no claim about the
credential or the account.

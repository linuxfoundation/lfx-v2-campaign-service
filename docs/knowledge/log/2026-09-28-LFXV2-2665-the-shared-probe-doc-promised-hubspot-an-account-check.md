# 2026-09-28 — LFXV2-2665: the shared probe doc promised HubSpot an account check

**Fix** — round 26 of review, raised by the repo-code and general reviewers. No behaviour change;
two shared comments qualified, one duplicated clause removed, one test guard inverted.

`ConnectionProber.ProbeConnection`'s Godoc said `nil` means "the credential authenticated AND the
configured account was reached", and `internal/dispatch/probe.go` opened by saying a connection
test asks both questions of every platform. Both are false for HubSpot, and deliberately so:
`docs/api-catalog.md` already records that HubSpot checks no account at all, because `portal_id` is
not one — nothing routes on it, its only readers build deep links for assets that already exist,
and the portal a campaign lands in is the token's own. The HubSpot probe verifies the private-app
token and stops. Six implementations sit under that interface and five of them do check an account,
which is how the universal phrasing survived: it is true of the majority and the exception is
documented somewhere else. Both comments now name the exception where an implementer reads them.

`googleads.ProbeAccountReach`'s Godoc carried two consecutive clauses, the first saying flat mode
follows enumeration with a `customer_client` read and the second saying it reads the account's own
`customer` record. `customer_client` is the manager-only resource this change stopped using in flat
mode — the first clause is a line of a superseded sentence left behind by an edit that replaced it,
so the comment contradicted itself and the stale half described the removed implementation. Deleted.

The third site is the same correction as the previous entry, one file further on. Round 25 fixed
`connection_test.go`'s inconclusive test where it explained itself, but the assertion below it was
still enforcing the superseded reading — it FAILED the test if the message said the credential was
"neither accepted nor rejected", which is now exactly what the message is required to say. A guard
does not go stale quietly: left alone it would have blocked the corrected wording and read as the
contract while doing it. It now rejects claims in both directions, because the mistake this cycle
keeps re-making is fixing an overclaim by asserting its negation.

Worth naming, because it is the fifth round in which stale prose was the whole finding: three of the
four sites here were reachable from a file I had already edited in this cycle — the same test
function, the same interface, the same Godoc paragraph. Proximity to a fix is not coverage by it.

# 2026-09-28 — LFXV2-2665: the LinkedIn inconclusive arm overclaimed in the other direction

**Fix** — round 25 of review, raised by the repo-code reviewer. No behaviour change; six
rationales and one test assertion corrected, plus the last two copies of round 24's stale sentence.

Round 9 corrected a real overclaim: `TestLinkedinAds`'s inconclusive arm used to justify
`OK: false` by saying the credential had not authenticated, which is not what an incomplete walk
establishes. The correction overshot. Six sites went on to assert the OPPOSITE — that "on this
path the credential demonstrably DID authenticate, since the credential baseline is what gates
entry to the walk at all" — and that is false for two independent reasons.

The baseline gating entry is `testConn`'s, and `internal/service/connection.go` says what it is in
its own doc: the row exists and carries a credential. It reads the stored row and never touches a
dispatcher, so passing it is evidence about this service's datastore and nothing about what
LinkedIn did with the material. And `ErrOrgVerificationInconclusive` explicitly covers a walk that
failed BEFORE send — a pre-send connection failure is named in its own contract — where LinkedIn
received nothing to evaluate. A claim stated unconditionally was false on a class the sentinel is
defined to include.

The fix is not to swap one assertion for the other. An incomplete walk establishes NEITHER half of
the conjunction `ok` names, which is the whole reason the arm answers `OK: false`, and the message
therefore asserts neither: not that the credential failed, not that it succeeded. It names the
unreachability and says nothing about the stored pairing. The corrected sites are
`docs/api-catalog.md`, `internal-service.md` (twice), `domain.ErrOrgVerificationInconclusive`'s
doc, the service arm itself, and `connection_test.go`'s inverse guard — which had been pinning the
overclaim as though it were the contract.

Worth naming, because it is the failure mode of a correction rather than of an original: fixing an
overclaim by asserting its negation keeps the shape of the mistake and only changes its direction.
The honest answer on a path that established nothing is that nothing was established, and that
reads as weaker than either claim, which is exactly why it kept losing to one of them.

Two sites from round 24's sweep were also still describing the pre-round-9 `OK: true` outcome:
`Orchestrator`'s interface Godoc and `internal/platform/linkedin/accounts_test.go`'s diagnostic,
both saying a misfolded permanent failure would be reported "as healthy". It would be reported
`OK: false` with a retry advisory — the operator sent to wait out an outage instead of repairing a
credential. That is the third round in which a sentence corrected in one file was left standing in
another, and the second in which my own sweep for it missed copies.

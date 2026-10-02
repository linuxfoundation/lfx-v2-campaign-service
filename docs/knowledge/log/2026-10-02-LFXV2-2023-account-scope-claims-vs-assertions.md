# 2026-10-02 — LFXV2-2023 account-scope: comments that claimed more than the tests proved

**Fix** — Third follow-up on the account-scope guard
(`2026-10-02-LFXV2-2023-account-scope-guard.md`,
`2026-10-02-LFXV2-2023-account-scope-test-binding.md`), from the local review cycle. The guard
is unchanged in behaviour; this records two false coverage claims and the one real code change
that came out of fixing them.

**The recurring defect was a comment, not logic — three rounds running.** Each round's fix
asserted a property the test could not detect, and the next round caught it. Round 2: a
permitted arm asserting only the guard's sentinel was absent, satisfied by any error. Round 3a:
a comment claiming the permitted arms were inherently unable to do better, when the upstream
path was available offline. Round 3b, below. The pattern is worth naming: a long explanatory
comment is a claim about the code, and an unverified claim in a comment is as wrong as one in a
commit message — it just survives longer.

**A permitted-path assertion cannot discriminate stored-from-caller.** The comment said
asserting the reached path "pins that the account forwarded upstream is the one the connection
stores, so a guard that validated the stored id but forwarded the caller's would fail." False:
the guard passes only when the two are equal after trimming, so on every permitted arm they
are the same string and no assertion on that path can separate them. Verified by mutating Meta
to forward the guard's returned value instead of the caller's — the whole suite still passed.
The comment now states what the arm does prove (the request was issued, carrying the matching
account) and what only the refusal arms carry (that refusal precedes any request, and
sensitivity to the guard existing at all).

**The real fix that claim pointed at.** Both reads forwarded the caller's raw `accountID` and
discarded the guard's return with `_`. That is safe only because each platform's
`ValidateAccountID` already refuses a padded id upstream — a second validator this code would
be depending on silently, which is the coupling `one-validator-not-two` warns about. Both
dispatchers now capture the guard's trimmed return and forward that: one validated value, used
once, and the return is no longer dead.

**A test whose second fixture was decoration.**
`TestMeta_ListAccountCampaignMetrics_SharedAccountAcrossProjects` gave two projects the SAME
stored account and asserted both were permitted, calling itself "the regression test for the
guard's one real failure mode". It tested nothing: the guard's inputs are one project's
resolved row plus the requested id, so it cannot observe another project, and replacing the
reader with only the project under test left it passing. It duplicated the permitted arm in
`..._AccountScope` while advertising cross-project coverage — the kind of false signal that
lets a later refactor look already-tested.

Replaced by `TestMeta_ListAccountCampaignMetrics_ProjectRowIsTheAuthority`, which asserts the
direction that IS expressible: two projects with DIFFERENT stored accounts, each permitted to
read its own and each refused the other's. Removing the second connection now fails 2 of 4
arms, where the old test passed that same mutation. The shared-account configuration needs no
test of its own — both rows hold the same id, so "stored == requested" is already the permitted
arm; the rationale stays in `requireMetaManagedAccount`'s doc comment.

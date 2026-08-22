# 2026-08-22 — a keyword mutate must follow the campaign's creation account

**Fix** — merging `main` into the keyword-endpoints branch produced a change git reported as
clean and that did not compile, and the obvious repair to make it compile was wrong on the one
path where being wrong cannot be undone.

## What the merge actually did

`main` landed the forced-system-ads-account work
([2026-08-20-existing-campaign-follows-its-creation-account](2026-08-20-existing-campaign-follows-its-creation-account.md)),
which split credential resolution in two: `resolve` for CREATION, and `resolveExisting` for any
operation on an already-created campaign, keyed on the account that campaign RECORDS being
created under. To carry that, `resolveGoogleAdsClient` gained a fourth parameter, a
`*model.Campaign`.

This branch had independently added three new call sites of that function — the two keyword /
audience reads and `ApplyKeywordActions`. Neither side touched the other's lines, so the merge
raised no conflict; the result simply failed to build with three "not enough arguments" errors.
A textual merge cannot see that a signature changed under call sites it merged verbatim, so the
absence of a conflict marker said nothing about whether the merge was correct.

## Why `nil` everywhere was the wrong repair

`nil` compiles at all three sites, and `googleAdsCreationCustomerID(nil)` returns `""`, the
documented "unknown, proceed" case that resolves the ordinary project-then-system account. On
the two READS that is right and stays right: those are scoped by a SET of campaigns, so there
is no single recorded creation account to resolve against, and per-campaign identity is enforced
downstream by `googleAdsScopeForCustomer`.

On `ApplyKeywordActions` it is a live defect. That path already compares the campaign's recorded
creating customer against `client.CustomerID()` and refuses on a difference, so the resolver and
the guard are coupled: resolve the project's own account and the guard refuses every campaign
created while `LFX_FORCE_SYSTEM_ADS_ACCOUNT` was on for a project that has a connection of its
own. That is the POST-cutover direction `resolveExisting` exists to prevent — it strands exactly
the campaigns the flag just created, on the path that stops their keywords from serving, while
they keep spending. Passing `campaign` resolves the recorded account and the guard passes.

## The rule

A parameter added to carry a fact is not satisfied by a value that merely type-checks. When a
merge makes a call site fail to compile, the question is which of the callee's cases the caller
is in — here, whether it operates on an existing campaign — not which argument makes the error
go away. Two of these three sites want `nil` and one wants the campaign, and only reading the
callee's contract separates them; all three now say in a comment which case they are, so the
next merge does not have to re-derive it.

Pinned by `TestGoogleAdsKeywordActions_PostCutoverCampaignResolvesItsCreationAccount`, whose
fixture makes the project's account and the campaign's creation account DIFFER. Making them
equal was checked and makes the test fail on both the fixed and the broken code for an unrelated
reason — the difference is what gives the assertion its teeth.

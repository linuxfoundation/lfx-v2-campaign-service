# 2026-08-08 — Account discovery: nine suppressed findings, all about claims the code no longer makes

**Update** — Closed the nine suppressed Copilot findings on PR #84. Not one of them is a
behaviour bug: every single one is a comment, concept paragraph, test message or PR
description asserting something that the previous round of fixes made false. That is the
whole shape of this round, and it is worth naming, because the previous round created it.

**A claim, once made, lives in more places than the file that changed.** Two of the earlier
fixes were classification refinements — decrypt failures split by what is provable, and the
account-id relaxation scoped to re-pointing rather than bootstrap — and each was corrected at
the site where the decision is taken. The RESTATEMENTS were not. Five of the nine findings are
the same two facts, restated elsewhere and left behind:

- `resolveGoogleAdsDiscoveryClient`'s godoc still said the account id "by definition does not
  exist yet", a dozen lines below `validateGoogleAdsCredentials`, which had already been
  corrected to say the opposite: `GoogleAdsConnectionConfig` still declares
  `Required("account_id")`, so an id always exists today and this endpoint serves re-pointing,
  not first-time bootstrap. The reason `CustomerID` is left empty is not that there is nothing
  to put there — it is that the upstream call is account-AGNOSTIC, so scoping it to one id
  would narrow the answer to a subset of the question being asked.
- `internal-dispatch.md` carried the bootstrap claim in full ("a connection is created with
  credentials first and an account chosen afterwards"), which made the knowledge bundle
  contradict both the API catalog and the bootstrap-limitation log fragment shipped in the
  same PR.
- `creds.go` and `internal-dispatch.md` both said the service layer logs the decrypt cause.
  It logs it on the 500 arm only. The `ErrConnectionNotUsable` → 400 arm deliberately
  suppresses it and logs `reason=credential_blob_malformed` alone, because one of the
  conditions on that arm is detected by decoding the DECRYPTED blob and an unmarshal error
  quotes its input. This is the security-relevant direction of the two: a comment saying the
  cause is logged is an invitation to reintroduce the logging that was just removed.
- `connection_accounts_test.go`'s failure message said "the cause is logged, not returned" —
  the pre-fix behaviour, embedded in the diagnostic a future engineer reads at the exact
  moment they are trying to work out what the contract is.

**The taxonomy lesson: grep the CLAIM, not the file.** A doc-accuracy fix is not done when the
file that motivated it is correct. It is done when no surviving sentence in the repo asserts
the old fact. Both fixes above were single-site edits to a fact with five sites.

The remaining three are ordinary drift:

- `aesgcm.go` described `ErrDecryptionFailed` as implying "EVERY connection is broken", while
  the service and domain layers had already been corrected to say GCM authentication failure
  is *indistinguishable* between a wrong deployment key and corruption of one full-length row.
  The 500 mapping is conservative, not a diagnosis. Rewritten to say so — and to state why the
  truncation guard still matters: a provably short blob is DECIDABLE here and always about one
  row, so letting it fall through would convert a certainty into that ambiguity.
- `errors.go` said "which of these four it was" one line after correctly saying five sentinels
  follow, and five are declared.
- `internal-service.md` conflated two different nil outcomes into one rule. They fail in
  opposite directions and only one of them has a guard: a nil from an `AccountLister` is
  rejected by `ReadAccounts` as 503, whereas a nil introduced LATER by the service's own
  conversion loop is created after that guard and serializes as a successful `null`. The
  `make(..., 0, n)` convention is the only thing protecting the second case, which is exactly
  why it has to be stated as its own reason rather than folded into the first.

The ninth was in the PR description: it named the route as
`/projects/{project_id}/connections/google-ads/accounts` while the Goa contract, generated
clients, chart and API catalog all expose `/projects/{project_id}/connection-google-ads/accounts`.
Corrected on the PR. **A PR description is a surface integrators read and no test covers** —
nothing in CI would ever have caught a route that 404s.

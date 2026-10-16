# 2026-10-15 — #208: Reddit composed-name checks run before any upstream call

**Fix** — `reddit.Client.CreateCampaign` composed the campaign and ad-group names and checked
them against Reddit's 200-rune limit only AFTER the Step 1 account-verify GET, so an over-long
name cost a token exchange and a GET before it was refused, and the refusal was a bare
`fmt.Errorf` that the async create job could only report as `platform campaign creation
failed`. The two checks now run immediately after geo normalization, before Step 1: every input
they read (the trimmed event name and project, the resolved `objective`, the normalized `geos`)
is settled by then and nothing the GET returns feeds them. They are built with
`domain.PreflightRefusalf`, so `DispatchFailureMessage` echoes them verbatim; the messages quote
only service-computed rune counts, so no caller text is formatted in. The claim contract is
unchanged (`(nil, err)` → `notCreated`), and the window check stays where it was, above Step 1.5.
The one visible difference is ordering: an input that fails both a name check and the window
check now reports the name check.

Tests: `TestCreateCampaign_ComposedNameRefusedBeforeAnyRequest` (internal/platform/reddit)
counts every token and API request for an over-long composed campaign name and ad-group name and
requires zero; `TestDispatch_PreflightRefusalsAreOperatorSafe` gains a "reddit composed ad group
name too long" case. Mutation-checked: moving the checks back below the GET fails both. See
[internal/dispatch](../code/internal-dispatch.md) "Operator-safe pre-flight refusals".

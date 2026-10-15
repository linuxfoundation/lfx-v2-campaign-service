# 2026-10-15 — #208: deterministic create checks moved before the first upstream call

**Fix** — An audit of every non-Google create path (Reddit, Meta, LinkedIn, X, Microsoft,
HubSpot), clients and dispatchers, looked for a validation that reads only the request, config,
stored connection or clock but runs after an upstream request. Two groups did, and both now run
before any request and are built with `domain.PreflightRefusalf`, so the async create job may
report them verbatim:

- **Reddit window check** (end not after the nudged start): it ran after the account-verify GET
  as a bare `fmt.Errorf`. It now runs right after the composed-name checks. The message is
  unchanged; both instants are `toRedditTimestamp` renderings of parsed times, not caller text.
  The start horizon is now taken before the GET; `redditPastStartBuffer` keeps a 429 term no
  create spends, which covers that one extra GET.
- **Meta geo refusals** ("no usable geo targets", "all selected geo targets … are regulated"):
  they ran after the account preflight GET. Each dropped code now goes through
  `domain.CallerValue`, so the "dropped" list quotes each code (a URL-shaped entry reads
  "(value withheld)"); the regulated list holds only table-vetted codes and is unchanged. The
  "Geo targets dropped/skipped" Steps lines are still appended after "Account verified".

Not moved, by design: LinkedIn's window re-checks inside `findOrCreateCampaignGroup` /
`createSponsoredCampaign` (clock-race re-checks of what `validateSchedule` already refuses),
Meta's minor-unit budget bounds (need the account currency), Reddit's late `ImageURLSource` URL
check, HubSpot's best-effort hero-image URL checks, and every backstop on X, Microsoft and
HubSpot that repeats an earlier check.

Tests: `TestCreateCampaign_WindowRefusedBeforeAnyRequest` (reddit) and
`TestCreateCampaign_GeoRefusalsBeforeAnyRequest` (meta) count every request and require zero;
`TestDispatch_PreflightRefusalsAreOperatorSafe` gains "reddit end not after the nudged start",
"meta no usable geo targets" and "meta all geos regulated". Mutation-checked: moving each block
back below its GET fails its tests. See [internal/dispatch](../code/internal-dispatch.md)
"Operator-safe pre-flight refusals".

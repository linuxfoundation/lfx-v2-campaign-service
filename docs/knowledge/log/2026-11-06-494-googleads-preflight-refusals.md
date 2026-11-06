# 2026-11-06 — Google Ads pre-flight refusals reach the job result

**Fix** — Every refusal the google-ads client raises before its first mutate is now a
`domain.PreflightRefusal`, so the async create job names the rule broken instead of "platform
campaign creation failed" (#494). Two Performance Max creates in prod on 2026-11-05 were refused
for having no description of 60 display units or less, and the operator saw only the generic
sentence. `preflightCampaignKind` wraps its input checks with the new
`domain.WrapQuotedPreflightRefusal`, which keeps `Error()` unchanged and renders every quoted
caller value through `CallerValue` for the operator. Each channel's pre-budget image fetch
marks what the slot refused but not transport failures, and `ErrVideoCreateUnsupported` is
marked too. The connection's account-id check stays unmarked. The two excluded-placement
refusals now quote their value, and a Display user-list exclusion no longer echoes an unknown
collection segment. The five foreign-account refusals quote the caller's customer id, and a
quote a capped error left open to the end of its line is withheld, as is any value a cap cut
with a trailing `…`. A sentinel sweep over every
caller string of each channel's input, including digits-only owner ids, an over-long
malformed URL and an opaque token whose only digit sits past the 100-byte cap, guards the
class, and a `'"'` rune literal no longer shifts the quote pairing. See
`code/internal-platform-googleads.md` § Pre-flight refusals reach the job result (#494) and
`code/internal-dispatch.md` § Operator-safe pre-flight refusals.

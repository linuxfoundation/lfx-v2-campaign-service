---
type: "Go Package"
title: "internal/platform/identityjson"
description: "Refuses a JSON response that encoding/json would decode WITHOUT error into something other than what its bytes say — malformed UTF-8, an unpaired surrogate escape, or a key declared twice — for the campaign-adoption reads whose answer binds a paid campaign to a brief, and for HubSpot's email-statistics and token-info reads."
resource: "internal/platform/identityjson"
tags:
  - platform-client
  - adoption
  - go-package
timestamp: "2026-10-07T00:00:00Z"
---

# internal/platform/identityjson

`Check(raw)` returns nil for a body that decodes without silent substitution and an error wrapping
`ErrUntrustworthy` otherwise. It exists for campaign adoption (LFXV2-2665): the Microsoft, Meta,
Reddit and X clients' `GetCampaign` run it over the raw answer before decoding, because an adoption
binds a brief to an arbitrary upstream campaign on the strength of one read, and the id and name it
returns are what an operator confirms the binding against.

The HubSpot client uses it too (LFXV2-2665, the email account monitor). Statistics responses are
mostly open maps that encoding/json keys EXACTLY, so `readEmailCounters` uses `CheckExactKeys`
(exact duplicates only) plus `FoldedKeyCollision` on the struct-decoded levels; the token-info
answer (`AuthenticatedPortalID`) gets the full `Check`, whose portal id every
caller compares against a recorded one. `"sent":1000,"sent":0` or a doubled `hubId` describes two
answers, and the decoder would pick one silently.

encoding/json has three silent behaviours that turn a malformed answer into a plausible one, and
`Check` refuses each:

- **malformed UTF-8** inside a string is replaced with U+FFFD, with no error;
- an **unpaired surrogate escape** (`\uD800`) — six ASCII bytes, valid UTF-8 — is replaced the same
  way. A genuine U+FFFD (encoded bytes or a `�` escape) and a doubled backslash are left alone;
- a **key declared twice** in one object is resolved in favour of the last value, so one object can
  carry two ids. Keys are compared under the decoder's own fold (case-insensitive, KELVIN SIGN and
  LONG S included), and the whole document is walked, not only the fields a caller reads.

Malformed JSON is not reported here — the caller's `json.Unmarshal` gives the better diagnostic —
and the error text is this package's own sentence, never a byte of the response.

The Google Ads adoption read applies the same three guards with its own unexported helpers
(`internal/platform/googleads/campaign_lookup.go`: `hasDuplicateKeys`,
`hasUnpairedSurrogateEscape`); they were reproduced rather than imported because that file was out
of scope for the change that added this package. **Convergence is a follow-up for the Google Ads
owner:** `internal/platform/googleads` should import this package and delete its copies; until it
does, a fix to either copy must be made to both.

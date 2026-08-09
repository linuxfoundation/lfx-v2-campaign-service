# 2026-08-08 — Verify before bind: looking a Google Ads campaign up by id

**Update** — `Client.GetCampaign` adds the by-id half of campaign adoption, and
`campaignRowIdentity` extracts the row-level identity checks so both lookups share one answer
to "which campaign is this row, and is it adoptable".

## Why by-id at all, when find-by-name already exists

Adoption binds a brief to a campaign this service never created. By name that works when the
operator knows the exact name, but the name is not the platform's identifier: it is mutable,
it is not unique across `REMOVED` campaigns, and an operator reading a campaign id out of the
Google Ads UI has the identifier already. The user's decision was **verify before binding** —
so the id has to be resolved against the platform, in this account, and the campaign's name
and status shown back, before anything is written. That is what `CampaignRef` carries: an id
alone is not something a human can confirm.

## Why the checks moved into a helper

There are now two ways to reach a campaign, and they must agree about what counts as a
trustworthy row. Duplicating the status allow-list, the resource-name shape check and the
identity-fields-agree check would let them drift — and the direction of drift matters: the
lenient one would be the by-id path, where the caller is further along, has already decided
which campaign it wants, and is about to attach real spend to it.

`campaignRowIdentity(row, describe)` returns `(id, live, err)`. `live` is false for `REMOVED`
alone. That asymmetry is the same one the by-name lookup documents: a tombstone is unadoptable
however it arrived, so dropping it can only ever be correct, while any other unrecognised
status — `UNSPECIFIED`, `UNKNOWN`, or the empty string an omitted proto field decodes to —
must error. Treating one as live returns a campaign whose serving state was never established;
treating one as a skip reduces an unverifiable response to a clean absence, which is the
licence-to-create value.

The helper's one sharp edge, in its first version, was that it decided `live` WITHOUT deciding
which campaign the row was, leaving "skip a tombstone" and "check the filter" to be ordered by
the caller — and the by-id path ordered them wrong. Both review bots caught it independently. With
the id check below the skip, a `REMOVED` row for a DIFFERENT campaign left through the
`continue` untested, and a response made only of such rows returned `(nil, nil)`: a response
that honoured NEITHER predicate (the query names one id *and* excludes `REMOVED`) reported as
the trustworthy absence a caller acts on by creating a second campaign against the same budget.
The by-name path never had the hole because it checks its name filter on the raw row before
calling the helper. Rather than bolt a second raw-field check onto the by-id path, the fix went
into the helper: identity is now established BEFORE status, so `campaignRowIdentity` returns an
id for a tombstone too and the caller can check the filter on EVERY row instead of only the
live ones. Position, not presence, was the whole defect — a guard placed under a `continue` is
not a guard.

Reordering also settled a second finding from the same review. The claim the tombstone skip
rests on — "a tombstone is unadoptable however it arrived, so dropping it can only ever be
correct" — is true only once we know WHICH campaign the tombstone is for. Deciding status first
granted that premise without earning it: a `REMOVED` row with a cross-customer resource name,
with identity fields that disagree, or with no usable id at all returned a clean not-live
verdict, and the by-id caller reported a campaign it had never identified as absent. A row must
now say who it is before its status is allowed to mean anything.

**That reordering is a CONTRACT CHANGE to the existing `FindCampaignByName`, not only to the
new by-id path, and calling the extraction behaviour-preserving was wrong.** The by-name lookup
also skipped tombstones before validating them, so a `REMOVED` row that is malformed,
cross-customer, or missing its identity fields used to return `("", nil)` and now errors. The
new behaviour is the one worth having — such a row is not evidence that the campaign we asked
about is absent, and the caller acts on an absence by creating a duplicate PAID campaign — but
it is a change in what an existing exported method returns, so it is pinned by
`TestFindCampaignByName_RemovedRowWithUnusableIdentityIsNotAnAbsence` rather than left implicit.
The narrow half is unchanged and still tested: a WELL-FORMED tombstone is a clean non-match.

The third finding is the mirror image: `GetCampaign` skipped tombstones before the
duplicate-details check, so a response carrying campaign 555 once as `ENABLED` and once as
`REMOVED` returned the live ref. One campaign cannot be both, so such a response has
contradicted itself and none of it is trustworthy — least of all the live row, the one a caller
would bind real spend to. It is checked after the loop, not on sight, because the rows arrive
in no guaranteed order and a leading live row must not buy trust for what follows it. Tombstones
ALONE stay an absence: that is the campaign asked about, reported unadoptable, which is what a
caller needs to hear.

A later pass found the same reasoning stopping one row short. The duplicate-details check sits
below the tombstone skip, so it only ever compared LIVE rows: two `REMOVED` rows naming campaign
555 with two different names both left through the `continue` and the call returned `(nil, nil)`.
But a response that answers one id with two campaigns has contradicted itself just as surely as
one answering both live and removed, and the absence read out of it is not the trustworthy
absence a caller may act on by creating. Tombstones are now held to the same agreement rule.
Identical duplicates stay tolerated, since GAQL really does return one campaign on several rows,
and the name is compared without being validated — a tombstone's name is never surfaced, so an
empty one is not a defect, but two names for one id still is. The narrow shape matters: the rule
is "this response disagrees with itself", not "this tombstone is unsatisfactory".

## A campaign nobody can name cannot be confirmed

The same review pass found the name unchecked: a live row with an omitted or whitespace-only
`campaign.name` was returned as a successful `CampaignRef`. The name is not decoration here —
verify-before-bind means an operator reads it to confirm the id resolves to the campaign they
meant, so a ref without one asks for a confirmation that cannot be given. `Campaign.name` is
required and populated for every campaign, so an empty one in a response that `SELECT`ed it is
a truncated answer rather than a nameless campaign, and it now errors. This is the cheap side
of the asymmetry: the error costs a retry, whereas returning the ref binds real spend to a
campaign nobody could identify.

A third pass narrowed what "a name" means. Two things the by-name path gets for free had to be
made explicit here, and both come from the same asymmetry: `FindCampaignByName` echo-checks the
decoded name against the name it asked for, so a corrupted name surfaces as a filter-not-honoured
error, whereas this path has no expected name — the name IS the answer.

**That "for free" was overstated, and a later review round found where.** The echo-check covers
every requested name except one that already contains U+FFFD — which is a legal campaign name.
Ask for `bad<U+FFFD>name`, receive a row whose raw bytes are `"bad\xffname"`, and the
substitution produces precisely the requested string: the comparison passes on a value nothing
ever saw intact, and an id comes back from an unverified response for an adoption to bind paid
spend to. The raw-bytes guard therefore runs on BOTH lookup paths, not just this one. The
narrowness is not an argument against fixing it — a guard that holds for all names but the
one an attacker or a corrupt upstream can choose is not a guard. Pinned by
`TestFindCampaignByName_MalformedUTF8RowCannotEchoTheRequestedName`, which also keeps the
narrowing half: a name that genuinely carries a properly-encoded U+FFFD must still adopt.

First, `encoding/json` does not enforce that its input is UTF-8. A JSON document must be UTF-8
(RFC 8259 s8.1), but malformed bytes inside a string are silently replaced with U+FFFD and no
error is returned, so `"name":"bad\xffname"` decoded into a perfectly successful `CampaignRef`
carrying a name the campaign does not have — offered to an operator as the thing to confirm the
binding against. The check is on the RAW row bytes rather than on hunting U+FFFD in the decoded
string, because U+FFFD is a legal character in a campaign name and a campaign that genuinely
contains one is not a defect.

That raw-bytes check turned out to be only half the story, and review caught the half that was
missing. `utf8.Valid` answers a question about BYTES, and there are two ways a name can be
rewritten in transit — it sees only the first. `"bad\uD800name"` is six ASCII bytes for the
escape, so the document is perfectly valid UTF-8 and the check passes it; the substitution
happens LATER, when `encoding/json` resolves the escape. An unpaired surrogate is not a Unicode
scalar value, so it decodes to U+FFFD, again with no error, and lands in exactly the value the
name guard below deliberately admits. Verified rather than reasoned about: a scratch program
printed `utf8.Valid(raw): true` and then `err=<nil> name="bad\ufffdname"`. So the guard now also
scans the raw bytes for unpaired UTF-16 surrogate escapes (`hasUnpairedSurrogateEscape`).

Its narrowing is the part worth reading. A well-formed PAIR is how every non-BMP character
reaches Go through JSON, so rejecting `\u` escapes wholesale would refuse every campaign with an
emoji in its name — the same false-absence-as-caution mistake in a new place. A DOUBLED backslash
is ordinary data too: `\\uD800` is a literal backslash followed by the text `uD800`, not an
escape at all, and reading it as one would refuse a legal name. Malformed hex is deliberately NOT
this function's finding — `json.Unmarshal` errors on it, and the caller turns that into a refusal
of its own. Nine sub-tests carry it: four escapes that must be refused, five that must stay
adoptable.

Second, the name is now held to the bounds of the field it came out of: at most
`maxCampaignNameRunes` CHARACTERS, and none of NUL, LF or CR. A value outside those did not come
from a campaign, because Google would not have stored it — so the response carrying it has already
gone wrong. The narrow shape matters more here than usual, because over-rejection is a false
absence wearing conservative clothes: adoption targets records this service never created and
never sanitized, so a TAB, a U+2028 line separator or a zero-width joiner really does occur
upstream, and refusing one answers "cannot trust this" about a campaign sitting right there.
That is why the guard is three explicit runes and not `unicode.IsControl`, which would reject TAB
and miss U+2028/U+2029 anyway. The tests carry both halves — four refusals and five names that
must stay adoptable, including one at exactly the ceiling.

## The two by-id-specific guards

**The caller's id is validated before interpolation, as an identity.** `canonicalCampaignID`
already existed for the ids Google returns; here it also gates the id a caller supplies. It
refuses `"0"`, a value past `math.MaxInt64` and `"007"` — all digits, none of them a campaign
this client can adopt. `"007"` is the instructive one: it matches campaign 7 server-side, so
querying it would surface as a filter-not-honoured conflict, reporting confusion where the
real fault is a malformed request. The injection case is worth stating plainly: with the guard
removed, `GetCampaign("555 OR campaign.id > 0")` emits
`WHERE campaign.id = 555 OR campaign.id > 0`, which is every campaign in the account. There is
no escaper here and there does not need to be — `campaign.id` is an int64 field, compared
unquoted, and the only legal operand is digits.

**The id filter is re-checked client-side, and a mismatch errors on the whole response.** Same
disposition rule as the name filter: a row for a different campaign proves the `WHERE` clause
was not applied, so nothing in the response is trustworthy. Skipping the row instead would
leave zero matches — `(nil, nil)`, the clean absence a caller is entitled to trust.

Duplicate rows for one campaign are tolerated, as by name, but they must agree on the **name**
as well as the id, because the name is the field an operator reads before confirming.

## What this deliberately does not answer

Whether the campaign is already bound to another brief. That is this service's own state, and
answering it here would be answering a database question with an ad-platform call. It belongs
to the adopt endpoint that consumes this.

## Still not wired

As with `FindCampaignByName`, no production caller exists yet. The Goa `adopt-campaign`
endpoint that binds a verified `platform_campaign_id` to a brief is the follow-up.

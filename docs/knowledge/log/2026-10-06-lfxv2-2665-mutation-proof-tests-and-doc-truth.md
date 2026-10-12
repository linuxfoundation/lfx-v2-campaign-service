# 2026-10-06 — LFXV2-2665 Mutation-proofed the Google channel tests, and made the docs say what the code does

**Fix** — Two clusters from the local pre-PR review, with one thing in common: a sentence —
in a test or in a document — that stayed true when the thing it described was deleted.

## Tests that could not fail

**Three channels never reached their own geo and criteria steps.** Display, Performance Max and
Video each carry a campaign-level geo mutate and a campaign-level criteria mutate, as Search does.
Every channel fixture builds on `demandGenInput()`, which asks for neither — so every cascade test
walked past both blocks with the conditions false, and deleting either block from any of the three
files left the whole suite green. `cascade_criteria_test.go` runs all three with a geo and a
language, and asserts what distinguishes the blocks' presence from their absence: TWO
`campaignCriteria:mutate` calls, the geo in the first and the language in the second, the ids on
the result where the activation gate reads them, and both audit steps. Its mutate handler answers
one result per OPERATION rather than a fixed count, because the three channels send different
numbers of assets and a fixed handler would short-answer two of them — which the client correctly
reads as UNCONFIRMED, i.e. as a different failure than the one under test.

Video is exercised through `createVideoCampaignCascade` directly, as the rest of `video_test.go`
already does: `CreateVideoCampaign` refuses before step 1.

**`checkStatusMutateResults` matched by name and was only ever tested by count.** Every cascade
test echoes exactly one result per operation naming the resource it addressed — which a count
check satisfies precisely as well as the set check does. The new table hands it the cases where
the two answers differ: the right count with one resource unaccounted for, MORE results than
operations (tolerated — extra work Google reports), a foreign-account name that is ignored as an
extra rather than counted toward an expectation, and a name of the wrong resource KIND. Replacing
the set check with `len(mr.Results) < len(want)` fails exactly the name-sensitive cases.

**A case that looked like it covered the ASCII rule covered the length rule twice.** `"ÜS"` is
THREE bytes, so `len(country) != 2` fires and `isASCIILetters` is never reached. `"Ü"` (two bytes,
one non-ASCII rune) and `"1S"` are the inputs that reach it; deleting the ASCII clause now fails
exactly those two, and `"ÜS"` still passes.

**`wantErr bool` cannot tell one refusal from another.** Six tables and standalone tests now assert
the MESSAGE: the promotion and price extension tables, the Video creative bounds, the Performance
Max count and combined-ceiling tests, the Performance Max criteria split, and the YouTube id
character rule. Several of these mutations trip two rules at once — an over-long headline is also,
to a count check, a headline — so a bare "it errored" is green when the bound under test is gone
and some neighbour refuses in its place. The Video bounds table uses `wantSub == ""` to mean the
case must be ACCEPTED, which keeps the over-refusal cases in the same table as the refusals.

**An assertion that could never fire.** The Performance Max asset-name test checked
`strings.Contains(a.create.Name, "SECRETSIGNATURE")` one line after failing on any non-empty
`Name` — so the second check was unreachable by construction. What actually has to be true is that
the signed query string reaches Google in NO field, so it is now asserted against the marshalled
asset create. (`pendingAsset.create` is unexported, so the creates are marshalled individually;
marshalling the slice yields `[{},{}]` and would have been a third assertion that could not fail.)

## Documents that outlived the code

`CreateVideoCampaign` refuses — the Google Ads API cannot create a Video campaign — and four places
still described Video creation as something this service does. The package frontmatter and its
verbatim index bullet, the API catalog's `videoCreative` field, and the original Video log entry,
which now carries a superseded-by pointer to the refusal entry rather than being edited away: it
still describes the Video SHAPE accurately, which is what adoption validates against.

Three more were counts and enumerations that stopped being true when Display landed or when Video
became adopt-only: the toggle cascade's channel list omitted Display; the activation gate's
"three channels this dispatcher creates" described five it resolves and four it creates, and listed
no Display arm although the code's `case` names three channels; the API catalog's "four channels it
creates" then listed five, and described the Performance Max 409 without the recorded-zero
asset-link refusal that gate now makes. The dispatch adoption invariant read "only the types this
service can create are mappable" while `VIDEO` is in the mappable set precisely because it is
ADOPT-only.

The `deviceTypes` passage was the subtlest: it justified omitting `CONNECTED_TV` on the grounds
that "Search is the only kind that reaches the map", which was true when it was written and stopped
being true the moment Video and Display started reaching `validateCriteriaPlan` un-narrowed — and
those are exactly the two channels on which Google DOES support TV screens. The omission is still
the right call, but it is now this client's limitation rather than a restatement of Google's, and
the reason it stays closed is a verification this client cannot run: the documentation does not
settle whether a `CONNECTED_TV` criterion carries a bid modifier, and the only reachable account is
a production one where even a `validateOnly` mutate is a POST.

**The through-line.** A test is only a test of the mechanism it can distinguish from its own
absence, and a document is only true of the code at the moment someone checks. Both failure modes
here are the same shape: a claim that was accurate when written, and that nothing forced to move
when the thing underneath it did.

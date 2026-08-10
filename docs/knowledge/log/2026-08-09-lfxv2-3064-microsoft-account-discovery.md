# 2026-08-09 — Microsoft ad-account discovery: a second service, on a second host

**Update** — `internal/platform/microsoft/accounts.go` adds `ListAdAccounts`, enumerating
every Microsoft Advertising account a connection's credentials can reach. It is the third
ad-account discovery client, after Meta (LFXV2-3060) and LinkedIn (LFXV2-3063), and it feeds
the same picker: a connection that holds only credentials needs to be able to ask which
accounts exist before one is chosen.

**Microsoft splits its API by service across different hosts, and this is the first call in
this package that leaves the campaign host.** Everything built so far — creation, the ad
group/ad hierarchy, the status toggle — is Campaign Management at
`campaign.api.bingads.microsoft.com/CampaignManagement/v13/`. Accounts are Customer
Management, at `clientcenter.api.bingads.microsoft.com/CustomerManagement/v13/`. That made
the shape of the change: `doRequest` had 14 call sites and its signature is unchanged, but its
body now delegates to an extracted `do(...)`, and a sibling `doCustomerRequest` routes to
`customerBaseURL` through the same token refresh, 429 policy and outcome classification. The
tests point the two bases at *different* servers, so a call routed to the wrong service fails
rather than passing by coincidence.

Worth recording alongside this: Customer Management is synchronous REST/JSON, which is why
this was buildable at all. It is not the Reporting API — that one is SOAP and asynchronous
(`SubmitGenerateReport` → poll → download a zipped CSV), which is what tabled Microsoft
*metrics* reads. "Microsoft Ads" is at least three services with three transports.

**The interesting design point: discovery must work for a connection with no account id.**
`doRequest` validates `account_id` at the request choke point, which is correct for every
call that acts on one account and exactly wrong here — requiring a valid account id would
make discovery unreachable precisely in the state it exists to resolve. So
`doCustomerRequest` skips `validateAccountIDs` and validates only `customer_id`, and only
when one is set.

The `CustomerAccountId` header is **omitted**, not sent empty. An empty header is still a
claim about an account, and the connections making this call have none. That distinction is
what the test pins: it asserts the header is *absent* from `r.Header`, not that its value is
`""` — the value-only assertion would have passed against a client that always sets it, since
`AccountConfig.AccountID` is `""` in that test anyway. The revert-check made this concrete:
restoring the unconditional `Set` produced `CustomerAccountId was sent (value "")`, a failure
the value assertion would have missed entirely. Same reasoning one layer up: `CustomerId` is
absent from the request *body* unless the connection carries one, because Microsoft documents
the credentials as determining the customer only when the element is omitted — sending `0`
would be a request for customer zero, not a request to infer one.

`OnlyParentAccounts` is sent `false`. Sending `true` would narrow the picker to one customer's
own accounts and answer "no accounts" for an agency-style setup whose accounts are linked
under other customers.

**`Id` is a `json.Number`, and this is not defensive styling.** Microsoft types it as a
`long`; decoding through `any` gives a `float64`, which silently loses precision above 2^53.
The revert-check is the whole argument: with `float64`, `9007199254740993` came back as
`9007199254740992` — a wrong account id that still looks exactly like an account id, and would
be stored on the connection and then either fail against Microsoft or, worse, not.

**Two health axes, kept apart — and neither is a filter.** `AccountLifeCycleStatus` and
`PauseReason` can disagree: Microsoft returns a pause reason alongside a status that is not
"Pause", so an account can read as bindable and still not spend. `Usable()` is an allow-list
(`"Active"` and no pause reason) so an unrecognized status reads as *not confirmed usable*
rather than as healthy, and `PauseLabel()` renders an undocumented flag verbatim
("paused (unrecognized reason 9)") because that raw value is the only thing distinguishing a
Microsoft-side change from a bug here. Draft, suspended and paused accounts are all returned
carrying their reason. Dropping them would answer "your credentials reach no ad accounts"
about an account sitting right there, and send the user hunting a permissions problem that
does not exist.

**Fail, do not truncate.** The endpoint is unpaginated, so unlike LinkedIn there is no cursor
walk to bound; the two remaining false-absence doors are both closed. `AccountsInfo` decodes
into a *pointer* to a slice so `{}` (no answer) stays distinguishable from
`{"AccountsInfo": []}` (zero accounts) — the revert to a plain slice made both read as zero
accounts, which is the exact shape the pointer exists to prevent. And an id failing
`accountIDRE` errors the whole call rather than skipping the row: a response that far from the
documented shape is not the response we think it is, and a short list is indistinguishable
from a complete one at the boundary.

The regexp is reused rather than restated, deliberately. It is the same one
`validateAccountIDs` checks a *configured* id against, so an account this call offers is
necessarily one the client will later accept. Two copies of that contract can drift into
offering ids that fail at bind time — and it also happens to reject the shapes a
`json.Number` can legally hold but an account id cannot (`1.5e3`, `-1`, `""`), so a number
that is not an integer id fails here rather than being rendered into something that merely
looks like one.

**Generalisation.** Where an absence is the licence to act, the decoder is part of the
contract: the difference between "no answer" and "the answer is none" has to survive
unmarshalling, or every guard above it is checking a value that has already lost the
distinction.

## Round 2: the pointer rationale was wrong about what a plain slice does

Copilot, on `accounts.go:104`, the concept doc and this log: `encoding/json` does not collapse
`{}` and `{"AccountsInfo": []}` for a plain slice. An absent or null key leaves the field
UNCHANGED — nil — and a present `[]` decodes to a non-nil empty slice, so the distinction
survives either way. Correct, and the same mistake appears in the LinkedIn client's `Elements`
field, corrected there in the same round.

The earlier entry above is left as written, because this log is history and an entry edited to
be right afterwards records nothing. What it claimed — that "the revert to a plain slice made
both read as zero accounts" — is false. What actually happened is that the revert removed the
nil check ALONGSIDE the pointer, and it was the check, not the type, doing the work.

The pointer stays, on a rationale that is now stated honestly: it is about the next reader,
not the decoder. A plain slice invites `len(x) == 0`, which merges precisely the two cases
this code must keep apart; a nil pointer has to be dereferenced, so the compiler forces
whoever touches it to decide which case they mean. The concept doc now says so, and says not
to simplify the pointer away without replacing the nil guard.

## Round 3: the correction was still wrong about `null`

Round 2 corrected the pointer rationale — a plain slice does preserve `{}` vs `[]` — but did
so with a clause that is itself inaccurate: "encoding/json leaves an absent or null field
UNCHANGED". It does not. An ABSENT key leaves the field untouched; a present `null` is
explicitly SET to nil. Verified rather than reasoned about, because the two differ whenever the
destination is not already zero: decoding `{}` over a slice preset to `["x"]` keeps `["x"]`,
while decoding `{"AccountsInfo":null}` over the same value yields nil.

They agree in this package only because the envelope is declared fresh on every response —
which is a property of the call sites, not of the decoder. Both the field comment and the
concept now say so in those terms. The same clause was corrected in the LinkedIn client in the
same sweep; it had been copied from there.

Nothing about the pointer changes: it is still legibility rather than mechanism.

## Round 4: "every account" was one customer's accounts

Copilot flagged that `AccountsInfo/Query` cannot enumerate accounts across more than one
`CustomerRole`. Verified against Microsoft's own docs rather than from the finding text, and it
is true — twice over:

- `GetAccountsInfo` returns the accounts "accessible from the specified customer". Singular.
- Omitting `CustomerId` does not widen it. The doc reads "if not set, the user's credentials
  are used to determine **the** customer" — also singular. The previous code read that sentence
  as "the credentials' accounts" when it means "one customer, chosen for you".

So a user administering several customers got one customer's accounts and no indication the
rest existed. That is the same false-absence shape this file already fails closed against
everywhere else: a picker that quietly omits an account is worse than one that errors, because
the user concludes the account is not connectable and goes hunting for a permissions problem
that is not there.

**The trap in the fix is `OnlyParentAccounts`.** It looks like it already covers this, and the
doc's own phrasing ("linked accounts") encourages the reading. It does not: a LINKED account is
attached to the customer being queried. A second customer the same user administers is a
different relationship entirely, and only `User/Query` — `UserId` omitted, returning one
`CustomerRole` per reachable customer — names it. The general rule: when a parameter widens a
query along one axis, check it is the axis you need widened before concluding the query is
already complete.

`ListAdAccounts` now discovers customer ids first and queries each. A configured `customer_id`
skips discovery: the operator scoped that connection on purpose, and re-widening it would undo
the scoping. The union dedupes by account id — a linked account legitimately appears under two
customers — and one customer failing fails the whole call, since a partial union is exactly the
bug being removed and reads identically to a complete one at the boundary. Absent, `null` or
empty `CustomerRoles` errors too: Microsoft documents at minimum one entry, so none of those
means "no customers".

Only `CustomerRoles` is decoded off the `User/Query` envelope. The `User` object carries a
password field, a secret answer and an authentication token; a test pins that a malformed body
never reaches the error string, which travels further than the response does.

`TestListAdAccounts_EnumeratesEveryCustomerTheCredentialsReach` was revert-verified against the
pre-fix single-customer behaviour and fails naming both halves — the customers queried and the
accounts returned. `TestListAdAccounts_OneCustomerFailingFailsTheWholeCall` fails with it.

One test-side lesson worth keeping: adapting the existing tests surfaced that the recorder
decoded request bodies with a plain `Unmarshal`, so every JSON number arrived as a `float64`.
The assertion on a customer id compared against `5.550001e+06` — and above 2^53 it would have
compared against a DIFFERENT id than the client sent, which is precisely the precision loss the
PRODUCTION decode uses `json.Number` to avoid. A test harness that does not share the
production decode's care can pass an id the production code would have caught. The recorder now
uses `UseNumber`, which also keeps a quoted `"9988776"` distinguishable from the number.

## Round 5: adding a call ahead of another one made three tests vacuous

Copilot, in suppressed comments on Round 4. Inserting `User/Query` in front of
`AccountsInfo/Query` did not just add a step — it changed what every existing single-handler
test was measuring. Three of them served one canned body to EVERY path, so the call now failed
in `discoveryCustomerIDs` and never reached the guard the test was named for:

- `AbsentEnvelopeIsNotZeroAccounts` — errored on missing `CustomerRoles`, not on `{}`.
- `UnusableIDFailsTheWholeCall` — never decoded a row, so deleting `accountIDRE` entirely would
  not have failed it.
- `NeverEchoesTheResponseBody` — redacted the role-discovery body, duplicating the new
  role-discovery test rather than covering the account decoder.

All three still PASSED, and for a plausible-looking reason: the expected error appeared, just
from one call earlier. That is the failure mode worth naming — **a test asserting only "an
error occurred" cannot tell you WHICH error, and inserting an upstream call silently rewrites
every such test to be about the new call.** Nothing in the build, the linter or the coverage
number moves.

Fixed by routing all three through `withCustomerRoles` and adding a `reached` counter on the
account handler, so a test that never gets to `AccountsInfo/Query` fails on that fact rather
than on the assertion after it. The counter is the durable part: wrapping alone would be
correct today and silently undone by the next inserted call.

Revert-verified individually — nil-envelope guard, `accountIDRE`, and the redaction each fail
their own test now, and neutering the account decoder's redaction leaves
`RoleDiscoveryNeverEchoesTheBody` green, which is how the two are known to be distinct.

The general rule for this repo: **when a new call is added ahead of an existing one, every test
of the later call needs a positive assertion that it was reached.** Order of assertions is not
a substitute — the error arrives either way.

## Round 6: the reused validator was answering a different question

Copilot filed three suppressed findings. Two were the same one on two lines, and it was
right.

Both discovery loops validated ids with `accountIDRE` (`^[0-9]+$`), and the comments were
proud of the reuse: the same regexp `validateAccountIDs` applies to a *configured*
account id, so a discovered account could not fail at bind time. That property is real.
The problem is what the regexp was written for. It guards a value about to be placed in
an HTTP header — a **transport** check, whose question is "is this safe to send" — and it
answers that correctly while admitting `0` and values past `MaxInt64`.

Discovery asks a different question. The id *is* the answer: it goes to the picker, gets
bound to a connection, and spends money. That is an **identity** check, and this package
already had one — `numberID`, used on ids returned by the create path, enforcing positive
and int64-ranged on top of the digit shape. The fix is one line in each loop. Everything
`numberID` accepts `accountIDRE` accepts too, so the bind-time property survives intact;
the reuse argument was sound, it just pointed at the wrong one of two available checks.

The general form, which is why this is worth a log entry rather than a diff: **a
validation borrowed from a transport concern is not automatically the right one for an
identity claim.** "Reused rather than restated" is a good instinct and it silenced the
question of *which* check by making consistency the whole argument. Ask what question the
borrowed check was written to answer before reusing it for a different one.

Revert-verified with four new table cases — `0` and `9223372036854775808`, in both the
customer-role and account-id tables. All four fail against `accountIDRE` and pass against
`numberID`; the pre-existing `-1` and `1.5e3` cases pass either way, which is exactly why
they did not catch this.

The third finding was about the PR description, not the code: it still described
`CustomerId` as omitted unless the connection carries one, which Round 4 replaced with
two-stage role discovery. Description rewritten.

## Round 7: the configured id was trusted more than the discovered one

Round 6 swapped `accountIDRE` for `numberID` on the DISCOVERED customer and account ids.
Copilot's next pass pointed at the branch above it: a configured `CustomerID` is returned from
`discoveryCustomerIDs` unchanged, so `0`, a leading-zero literal, and anything past `MaxInt64`
are enumerated under and reported as the customer whose accounts these are.

It looked covered, which is why it survived a round. `doCustomerRequest` does validate
`CustomerID` — but with `accountIDRE`, which is the transport check Round 6 had just finished
arguing is the wrong question for an identity claim. The configured branch returns before that
check anyway, and even after it the value is an answer, not a header.

The asymmetry is the part worth keeping: a discovered id arrived from the API seconds ago; a
configured one has been in a connection record since whenever it was written, possibly by an
earlier version of this code. Validating the fresher one more strictly is backwards. Fixed by
running the configured value through the same `numberID`, failing the call with a message that
names the value rather than silently querying under it.
`TestListAdAccounts_RejectsAnUnusableConfiguredCustomer` covers zero, int64 overflow and a
leading zero; revert-verified, and the leading-zero case is instructive — without the fix it
reaches `json.Marshal` and dies on `invalid number literal "0123456"`, i.e. the value really was
going out on the wire.

Copilot's second finding was a comment that had gone stale under Round 6's own test. The
`CustomerAccountId` omission rationale said the calls that skip the header "are the ones made by
a connection that has no account yet" — but
`TestListAdAccounts_OmitsTheAccountHeaderEvenWhenOneIsConfigured`, added the same round, pins
the opposite: omission follows the OPERATION's scope, and discovery skips the header even with
an account configured, because re-pointing is half of what discovery is for. Corrected there and
in the package overview, which still claimed account headers go on every call.

General shape: **a check that exists somewhere is not a check that answers your question.** Both
findings this round are the same mistake seen from two sides — reusing a validator, and reusing
a rationale, without re-deriving either from the caller that now depends on it.

## Round 8: "the account headers" was two headers with different rules

Both Copilot findings this round were the same defect in two places, and Copilot said so
("this issue also appears on line 658"). Both were real, and both were comments — the code
and the tests were already correct, which is exactly why they survived.

`doCustomerRequest`'s godoc opened with *"The account headers are NOT sent"*, plural, and the
package overview said *"the account/customer-id headers go on ACCOUNT-SCOPED calls only"*.
Neither is true of `CustomerId`: `attempt` sends it whenever `c.account.CustomerID != ""`,
regardless of `accountScoped`, so a Customer Management discovery call carries it. The
behaviour is right and deliberate — `CustomerId` names the manager account the CREDENTIALS
act under, not the account being asked about, so it narrows nothing discovery needs — and
`accounts_test.go` already pins it (`CustomerId header = 9988776` on a discovery call). Only
the prose was wrong.

The tell was sitting inside the same comment. Its last sentence read *"CustomerID is still
validated when set, because it does reach a header"* — which contradicts the opening sentence
three lines above it. A comment that refutes itself is a strong signal that a two-element
concept got collapsed into a plural noun: "the account headers" reads as one thing, and the
two headers it names have opposite scoping rules. Both comments now name `CustomerAccountId`
specifically and state the `CustomerId` case as its own clause, with the reason it is not
gated.

Worth generalising, because the mechanism is not Microsoft-specific: **a plural noun covering
two items with different rules will eventually be written as though the rules were shared.**
The cost here was contained — a reader would be misled about which headers a discovery call
carries — but the same collapse in a security or scoping comment is how a later change gets
made "consistent" with a rule that never applied to both halves. No code change; the docs
bundle (`internal-platform-microsoft.md`) already described the split correctly, which is
what made the drift visible once it was pointed at.

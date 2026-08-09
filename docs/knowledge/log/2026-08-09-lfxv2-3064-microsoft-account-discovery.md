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

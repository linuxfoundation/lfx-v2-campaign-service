# 2026-08-08 — Credentials-first connection bootstrap, and the 503 that was hiding behind a dead guard

**Update** — `GoogleAdsConnectionConfig` no longer declares `Required("account_id")`, so a Google
Ads connection can be created with credentials alone and have its ad account chosen afterwards:

```
POST   /projects/{id}/connection-google-ads          (credentials, no account_id)
GET    /projects/{id}/connection-google-ads/accounts (discovery)
PUT    /projects/{id}/connection-google-ads          (set the chosen account_id)
```

Account discovery shipped ahead of this, and the earlier log fragments
(`2026-08-07-lfxv2-2023-accounts-bootstrap-limitation.md` and its siblings) record the limitation
accurately as of their dates: the endpoint could list accounts, but the caller had to have already
pasted a customer id to create the connection at all, so discovery was only useful for RE-POINTING
a connection that was already complete. Those fragments are history, not current contract; this
one supersedes them.

## Google Ads only, and the reason is not "start small"

Google Ads is the only provider with an account-discovery endpoint, so it is the only provider
where a caller can create a connection without an account id and then find out what to put there.
Relaxing the requirement for LinkedIn, Meta, Reddit, X, Microsoft or HubSpot would produce a
connection that cannot be finished from inside this API — the operator would have to obtain the id
out-of-band regardless, and the only thing gained is a half-configured row. The rule to carry
forward: an optional `account_id` and a discovery endpoint ship together or not at all.

## What the change actually cost: a dead guard with no sentinel

The design edit was one line. The defect it exposed was not.

`validateGoogleAdsConnection` has always rejected an empty account id, and it returned a bare
`fmt.Errorf` carrying no sentinel. That was harmless only because it was UNREACHABLE — the design
required `account_id`, so no stored connection could lack one. Making the field optional turns
that guard into the NORMAL state of a freshly created connection, and a bare error falls through
to each handler's `default:` arm, which answers **503 "the platform did not respond"** — about a
platform that was never contacted, with a remedy (wait) that can never work, for a project that
has simply not finished setting up.

The fix is `domain.ErrAccountNotSelected`, wrapped alongside `ErrConnectionNotUsable`. The roles
are split: `ErrConnectionNotUsable` picks the status, `ErrAccountNotSelected` supplies the reason
token (`unusableConnectionReason` → `account_not_selected`) and the specific message. Because it
is always wrapped alongside the other, each handler must match it FIRST — a broad match swallows
the distinction and tells an operator to repair credentials that are fine.

Two handlers had to learn it, not three. The status toggle and the metrics read answer 409 (the
campaign is the resource; an unfinished connection is a precondition conflict, matching how they
already classify `ErrCampaignNotProvisioned`), and both are non-retryable, which is the property
a client acts on and the one 503 got wrong. **Account discovery deliberately does not map it**:
it calls `validateGoogleAdsCredentials` rather than `validateGoogleAdsConnection`, so the
account-id guard never runs there — accepting an account-less connection is what makes the
bootstrap possible, since discovery is how the operator finds the account to select.

**The general lesson: an unreachable guard is untested error-classification, and relaxing a
constraint is what makes it reachable.** Before dropping a `Required`, grep for the guard that the
requirement made dead and check what it returns — not just that it returns something.

## Two things this deliberately did NOT need

Both were live risks worth ruling out before committing to the approach, and both came back clean:

- **No migration.** `account_id` is `NOT NULL TEXT` (migration `000001`), so `""` is a legal value.
  "Unfinished" is spelled as the empty string, not NULL.
- **No response-contract change.** `Connection.AccountID` is a plain Go `string`, so the response
  type's `Required("account_id")` is satisfied by `""`. If that field ever becomes a pointer, the
  contract has to change with it — `TestCreateGoogleAds_WithoutAccountID` fails if it does.

## `status=active` on a connection with no account is deliberate

It has to be: `validateGoogleAdsCredentials` refuses a non-active connection, so a distinct
"pending" status would make discovery unreachable for exactly the connections that need it, and
the bootstrap would dead-end at step two. "Active" describes the CREDENTIALS, not readiness to run
a campaign. Readiness is a derived fact — `account_id` non-empty — and is reported through the
reason vocabulary rather than a second status carrying the same bit.

One consequence to keep in mind: `PUT` is a full replace, so omitting `account_id` on update
CLEARS a previously chosen one. That is the same semantics `label` and `login_customer_id` have
always had on that handler, and it is the intended way to un-select an account.

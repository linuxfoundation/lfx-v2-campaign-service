# 2026-11-16 — #563 Microsoft levers address confirmed failed ad groups; unconfirmed ad groups block the bid

**Fix** — The ad-group findings of several #613 review rounds, which landed in mixed entries on
#613 and are collected here for the PR that carries the ad group code.

- The bid and keyword levers use every distinct further ad group whose `adGroupId` was confirmed
  (`microsoftConfirmedExtraAdGroupIDs`), including a `failed` group whose ad or keyword step failed
  after Microsoft confirmed it. Before this, a bid write on a `created_degraded` campaign saved a
  new campaign bid while that group kept its old one, and keyword actions refused that group's
  confirmed keywords. Activation still addresses only `created` groups, so it never opens a partly
  built one.
- A later ad group whose create was ambiguous and recorded no id is marked `unconfirmed` in
  `adGroups`. While any is, `update-campaign-bid` refuses (409, `ErrBidUnwritable`) naming the
  entry, instead of re-bidding the rest and reporting success (the reconciliation entry of the
  same day narrows when).
- A later ad group create that Microsoft accepted but answered with an unreadable body (`{}`,
  errNoID) is recorded `unconfirmed` too. `createAdGroupAndAd` wraps its UNCONFIRMED ad group, ad
  and keyword errors in `hierarchyStepUnconfirmedError`, so `IsOutcomeUnconfirmed` agrees with that
  classification.
- When a later ad group's bid write fails ambiguously (timeout, 5xx, unreadable success), the error
  says that group's outcome is unconfirmed and the campaign may be partly re-bid, rather than that
  it "was not" re-bid.

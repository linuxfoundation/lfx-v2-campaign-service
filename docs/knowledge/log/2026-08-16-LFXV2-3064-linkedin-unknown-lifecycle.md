# 2026-08-16 — LFXV2-3064 LinkedIn unknown lifecycle status

**Fix** — `linkedInAccountLabel` rendered an account whose lifecycle status is ABSENT or
unrecognized identically to a healthy one, so the account-discovery picker offered it as
the good choice.

Lifecycle and serving are independent on LinkedIn, and only the first is a deny-list.
`StatusLabel()` returns `""` for ACTIVE, absent and unrecognized alike — deliberately, since
the package has nothing to say about the latter two. The label's only other qualifier was
gated on `!Servable()`, and `Servable()` reads `servingStatuses` alone and never consults
lifecycle. An account with `Status: ""` and `servingStatuses: ["RUNNABLE"]` therefore cleared
every arm and returned a bare name: byte-for-byte what an ACTIVE + RUNNABLE account returns.

That is the one answer a picker must not give, because the operator's next act is to bind a
paid campaign to the row. The doc comment above the function already asserted the intended
behaviour — an empty label "is not a claim the account is fine" — while the code rendered it
as exactly that reassurance whenever serving was RUNNABLE.

The new term keys on `Active()` rather than on `StatusLabel()`'s emptiness, which is what
distinguishes "ACTIVE" from "not confirmed"; `Active()` had no production caller before this.
A CANCELED account was always labelled correctly — the map speaks for it. The gap was strictly
the bucket the map cannot speak for, which is also the bucket the `Servable()`-gated fallback
could not catch.

This matches the shape Microsoft already had: `Usable()` requires `Status == "Active"`, so an
absent status fails closed there and reports "account status could not be confirmed". LinkedIn
now uses that same wording for the same state.

**Note on the test gap.** Every case in
`TestLinkedInAccountLabelSurfacesWhyAnAccountCannotServe` pinned `Status: "ACTIVE"`, so the
whole unknown-lifecycle bucket was untested — the table exercised the serving axis thoroughly
and the lifecycle axis not at all. Two cases now cover absent and unrecognized status; with the
guard reverted both fail, and the failure prints the label as `"LF Events [USD]"`, which is the
defect stated in the assertion output rather than only in prose.

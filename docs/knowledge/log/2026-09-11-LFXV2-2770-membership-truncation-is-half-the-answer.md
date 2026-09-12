# 2026-09-11 — LFXV2-2770: the HubSpot client learns to count people, not lists

**Update** — `internal/platform/hubspot` gained two reads the audience explorer needs:
`list_memberships.go` (paged membership ids, the legacy v1 name of a list v3 can no longer see,
and a not-found test) and `email_sendlists.go` (the lists a prior marketing email actually
targeted).

Memberships exist because list SIZES cannot answer the question an operator is asking. Summing the
sizes of a selection over-counts, and registrant/speaker overlap is the normal case rather than
the exception, so the only honest preview is a union of ids. Only ids are collected — pulling
contact properties for tens of thousands of records in order to count them would move real
personal data through this service for no reason at all.

`ListMembershipIDs` returns `(ids, truncated, err)` and **`truncated` is the important half of
that return.** Paging stops at 100 pages of 250, the endpoint's maximum — 25,000 records. A
truncated membership makes the union an UNDER-count, and understating how many people an email
reaches is the one error direction that must never be presented as exact, which is why the
explorer reports `25,000+` above the cap and never a number it cannot stand behind. A caller that
discards the flag silently converts a floor into a fact.

`LegacyListName` looks unnecessary until a current list's filters reference a list old enough that
v3 cannot see it. A name this service cannot read is a suppression it cannot credit — so the
missing lookup would not surface as an error, it would surface as a QA finding about an exclusion
that is in fact correctly applied. `IsNotFound` keeps "deleted" and "unreadable" apart, because
they are different answers for the operator holding the list.

`GetEmailSendLists` reads the same `to.contactIlsLists` object `SetSendList` writes, or the
builder would report a precedent the sender never used. The legacy `to.contactLists` selection is
read only here: it stopped functioning for sending on 2024-10-31, but a prior edition's email can
predate that, and those ids resolve through a different API — so they are carried in their own
fields. Mixing them would report rows as deleted when they exist perfectly well under the legacy
endpoint. And `includedProperties` is deliberately not used: `to` is a nested object rather than a
property, so asking for it by name returns an email with an empty selection, which is
indistinguishable from a send that targeted nobody.

Part of [[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]].

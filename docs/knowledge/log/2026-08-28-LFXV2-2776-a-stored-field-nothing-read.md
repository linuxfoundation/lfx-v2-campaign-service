# 2026-08-28 — a field the UI confirms saving, that nothing on the dispatch path reads

**Update** — `sender_name` and `sender_email` are declared for HubSpot in `model.connection`,
accepted by the connections API, and written to the connection row. Nothing on the dispatch path
ever read them. The single production `PatchEmailSettings` call passed `Subject` alone, so every
cloned draft kept the TEMPLATE's sender no matter what the project had configured.

## Why this is worse than an unsupported field

An unsupported field is refused, and the operator learns immediately. This one is accepted: the
form saves, the value round-trips through `GET`, and the UI confirms it. Every signal an operator
has says the sender is configured, while the field it controls is never consulted. The only way to
discover it is to look at a sent email and notice the From line is not what you set.

The shape to look for: a config key that has a **writer and a reader in the API layer, and no
reader in the layer that acts on it**. `git grep` for the key name and check whether any hit is
outside `internal/service/connection.go` and `gen/`.

## The coupling the fix had to be careful about

Subject and sender share one endpoint, so they go in ONE patch — splitting them would spend two
calls and could let one land while the other failed, leaving a draft whose halves came from
different sources with nothing recording it.

That made an unvalidated field dangerous in a new way. Nothing validates `sender_email`: the Goa
attribute declares no `Format(FormatEmail)` and `connection.go` writes the operator's string
through unchanged, so a malformed address really can be stored. HubSpot answers a bad `replyTo`
with a **400**, which `ambiguous()` classifies as terminal rather than retryable — so forwarding it
would have rejected the whole patch and lost the generated SUBJECT too. A defect in one field
silently reverting an unrelated one.

`mail.ParseAddress` at the dispatch boundary drops only the address, keeping the subject and
`fromName`. The draft falls back to the template's sender, which is the same outcome as configuring
none: a working LF-owned address rather than a broken send. Validating at save time as well would
be better — it is where the operator can see the error — but the dispatch guard is the one that
protects rows already stored.

## Testing note

Both sender tests assert at the **wire** level (`from.fromName` / `from.replyTo`). HubSpot ignores
`name`/`email` on that object, so a test reading the `EmailSettings` struct would pass against a
payload HubSpot silently discards — the field mapping is exactly the thing that needs pinning.
Confirmed by swapping the two mappings in `email.go`: both assertions fail.

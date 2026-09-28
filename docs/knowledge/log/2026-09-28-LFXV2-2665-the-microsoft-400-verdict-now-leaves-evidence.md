# 2026-09-28 — LFXV2-2665: the Microsoft 400 verdict now leaves evidence

**Update** — rounds 20, 21 and 22 of review. The general reviewer raised the same finding in all
three, and this entry records both what was declined and what was done instead.

**The finding.** `markConfiguredCustomerRejection` claims every `400` from `AccountsInfo/Query`
as proof the configured `customer_id` is unreachable, gating only on the status and on the id
having come from the connection row. The status alone does not establish the cause: a moved
contract, a developer-token problem, or some other operation-level validation can answer `400`
too, and each of those is a service defect being reported to an operator as their own bad field.
`apiError` already parses `ErrorCodes` and this branch ignores them.

**Why the proposed fix was declined, three times, on unchanged reasoning.** The fix asked for an
allowlist of "documented customer-not-found / customer-authorization error codes". The Customer
Management codes for a missing or unreachable customer are pinned by nothing in this repo and by
no test against the live API. A guessed literal never matches, so every real occurrence falls back
out of the marker and restores the paging `500` the marker exists to remove — while the allowlist
sitting in the file reads as a gap that was closed, and a gap that looks closed stops being looked
for. The reviewer's own escape clause — "if Microsoft does not provide a reliable discriminator,
leave an unclassified `400` on the service-defect path" — is the bug verbatim. Verifying the codes
needs live Microsoft ad-account access, which is still blocked.

**What was done instead.** The finding is right about the cost even though its remedy is not
available, so the cost stops being invisible. `ConfiguredCustomerRejectionCodes(err) []string`
hands out the codes the predicate refuses to read, and `MicrosoftDispatcher.ProbeConnection` logs
them at warn every time it renders `customerNotReachable`. The verdict is unchanged — still the
status and the provenance, still no code — but the first real occurrence in any environment now
leaves behind exactly the evidence an allowlist would need, which is the only way one can ever be
written honestly. That turns a disagreement that cannot be settled from this repo into one that
settles itself the first time the path fires in dev.

The accessor returns a copy, and answers nil for an error this predicate does not claim so it
cannot drift into a general body reader. `ErrorCodes` is bounded at parse time and carries no
upstream body text — `apiError` drops the raw body after extraction for exactly that reason — so
logging it is consistent with the credential-and-untrusted-text rule rather than an exception to
it. `customer_id` is deliberately not logged: it is the operator's own identifier, and the verdict
already names the field.

`TestConfiguredCustomerRejectionCodes_SurfacesWhateverMicrosoftSent` asserts pass-through with an
obviously invented code literal. Nothing may come to depend on its value; a plausible-looking real
code in that fixture would be the same unearned claim the predicate declines to make, which is the
mistake round 20 corrected in the neighbouring fixture.

# 2026-08-07 — LFXV2-2023: route concepts, the catalog's status list, and a diagnostic that over-claimed

**Update** — Three documentation-currency gaps around the account-discovery endpoint, all
of the same shape: the code changed and the surfaces that describe it did not.

**Fix** — The HTTPRoute regex and the RuleSet both stopped treating `connection-*` as one
uniform family. `connection-google-ads` is now its own alternation branch because it carries
a sub-path the others do not have yet — `/accounts`. Folding that into the shared
alternation would admit it for every provider, and a path the HTTPRoute admits but the
RuleSet does not rule is a parity violation. `docs/knowledge/kubernetes/httproute.md` and
`ruleset.md` both still described the undifferentiated family, so the one asymmetry in the
matcher was invisible to anyone reading the concepts instead of the YAML. Both now state it,
including the rule for the next provider: move it out of the shared branch, do not widen
the shared branch.

**Fix** — The api-catalog row listed 400/404/503 for the endpoint. The handler also returns
**400 for a stored connection that exists but is not usable** (inactive, incomplete
credential blob, malformed stored config, ciphertext too short to be valid) and **500 for a
well-formed blob that fails authenticated decryption**; the Goa design publishes all four.
A catalog that under-lists statuses is read as a complete contract by anyone writing a
client.

**Fix** — The `ErrCredentialDecryptionFailed` arm asserted that the cause is a wrong or
rotated application key, and that because the key is deployment-wide "this same failure is
hitting every project's connection at this instant". That is one of two causes, not the
cause: this one row's ciphertext being corrupted or tampered with produces the identical
error while every other connection is fine. GCM cannot distinguish them. Stated as a
certainty it misdirects incident response onto a key-rotation path for what may be a single
damaged row. The comment and the log message now name both and frame the first triage
question as which one it is — are other projects failing too.

**Note** — The blast-radius claim was defensible when written, because the only way to reach
that arm was a full-length blob. It became wrong in the same push that made truncated blobs
a 400 (see the truncated-ciphertext entry): narrowing what reaches an arm changes what that
arm is allowed to conclude, and nothing makes the prose downstream of a guard re-derive
itself.

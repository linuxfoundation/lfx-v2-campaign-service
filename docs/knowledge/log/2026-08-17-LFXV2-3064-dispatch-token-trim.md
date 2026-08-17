# 2026-08-17 — LFXV2-3064 dispatch token trim + doc sweep

**Fix** — `LinkedInDispatcher.Dispatch` only TESTED `strings.TrimSpace(creds.AccessToken) == ""`
and then sent the raw value, while `resolveLinkedInCredentials` — shared by the discovery and
toggle paths — ASSIGNS the trimmed token back. The two therefore disagreed about the same stored
credential: a whitespace-padded token listed accounts successfully through discovery and was
refused on create, which is the misleading-discovery state the shared resolver exists to prevent.
`Dispatch` now adopts the trimmed value.

The existing helper test carried a comment carving `Dispatch` out — "Dispatch keeps its own inline
checks because it wraps them in `notCreated()` … a different contract". That carve-out is what let
the divergence persist: the contract difference is real for CLAIM RELEASE and says nothing about
whether the token is normalised the same way.

**Note on the test, which was wrong twice before it was right.** The first version asserted only
that `Dispatch` did not reject the padded token as "incomplete". That passed against the DEFECT —
the padded token was never rejected there either — so it proved nothing. Reverting the fix left it
green, which is the whole point of running the revert. The second version asserts the
`Authorization` header captured by an `httptest` server, which is the only observable that
separates adopting the trim from testing it. Reverting now fails with
`access token must not have leading or trailing whitespace` — the LinkedIn client's own preflight,
which also confirms the upstream refusal is real rather than assumed.

A mutation attempt in between silently failed to apply (the `perl` pattern did not match) and the
test "passed" against unmutated code. A revert that does not change the file is not evidence;
assert the mutation landed before believing its result.

**Docs** — Three sites contradicted the corrections made earlier the same day, all found in
Copilot's SUPPRESSED comments rather than as review threads:

- `design/connection.go` still said LinkedIn and Microsoft have "no list to choose from" ~30 lines
  below the new paragraph saying they gained discovery. Now states why each of the four still
  keeps `Required("account_id")`, which differs per provider.
- `docs/api-catalog.md` named `accountDiscoveryProviders` as the only gate for credentials-first
  LinkedIn/Microsoft. There are three: the public payloads' own `Required("account_id")`, the
  bootstrap map, and (LinkedIn only) create-path tagging. Naming one would send the next change to
  do a third of the work.
- `docs/knowledge/kubernetes/httproute.md` said "the remaining four carry neither" when moving
  LinkedIn and Microsoft into the `/accounts` branch left only Reddit and X.

The shape is the same each time: a correction that updates the sentence it targets and leaves the
neighbouring paragraph asserting the opposite. Grep the CLAIM across the repo, not the line.

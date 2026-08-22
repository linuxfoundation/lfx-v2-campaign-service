# 2026-08-22 — three assertions that could not say what went wrong

**Fix** — the credential round-trip suite had three defects of the same family: each assertion
was CORRECT, and each would have described its own failure badly or fired for a reason outside
the property under test.

## The scratch database cloned template1

`migrate_down_live_test.go` provisioned its per-version scratch database with
`CREATE DATABASE %q`, and Postgres clones `template1` when no TEMPLATE is given. `template1` is an
ordinary database that a developer or another tool may have added objects to — unlike `template0`,
which is guaranteed pristine.

The test then asserts a fully-empty public schema after stepping the down migrations to version
zero. Any stray object in `template1` fails that assertion while every down migration is correct,
and the failure names the migrations rather than the template.

Verified rather than argued: creating one table in `template1` and re-running produced

    stepping DOWN from version 1 did not restore the schema that migrating UP to version 0
    produces: UNEXPECTED (the down did not remove it): column:stray_pollution.id integer

With `TEMPLATE template0` the same polluted `template1` has no effect. The local `template1`
happens to be clean today, which is exactly why this sat unnoticed — the test passes on the
machine that wrote it and fails on the machine that does not.

## The round-trip failure printed only lengths

The stored-column assertion compares with `bytes.Equal` and is right, but its message reported
`stored %d bytes, read back %d`. For the fault it most needs to describe — a column returning the
right NUMBER of bytes with different CONTENT, from an encoding round-trip or a padding driver —
that prints two identical numbers and reads like a passing run that somehow failed.

It now reports length plus `sha256.Sum256` of each side, the pattern the decrypt assertions in the
same file already used. Mutating the read to flip one byte in place now yields
`stored 54 bytes sha256=c5d9d2fd… read back 54 bytes sha256=6ed98bb4…` — the lengths still match
and the digests separate them. Neither digest reveals key material.

This also makes an existing claim in this ticket's knowledge log TRUE: it described the digest
pattern as "the pattern the same file already used for the stored-column assertion", and the
stored-column assertion was the one site that did not use it.

## gcmSizes did not do what its comment claimed

The comment said reading the sizes off a constructed AEAD "beats pinning literals" because a
production move to a different nonce length would otherwise fail a correct blob. It does not.
`gcmSizes` builds a SEPARATE `cipher.NewGCM`, which always returns the 12/16 defaults; there is no
accessor for the AEAD `crypto.AESGCM` actually holds. Had production moved to
`NewGCMWithNonceSize`, this helper would still have reported 12 and produced precisely the failure
the comment promised it avoided.

The construction does buy something real and narrower: the two numbers stay consistent with each
other and with the cipher they describe, instead of being two literals that can drift apart. The
comment now says that, names the gap it does not close, and records that exporting
NonceSize/TagSize would close it properly and is declined because a test may not widen the
production API. Both copies of the claim — the helper's doc and the assertion that calls it — were
corrected together.

## The family

None of the three would have failed a green run, and none was a wrong assertion. Two were about
what a failure MESSAGE can distinguish, and one was a comment describing a guarantee its code did
not provide. A test earns trust from what it says when it fails, not only from passing.

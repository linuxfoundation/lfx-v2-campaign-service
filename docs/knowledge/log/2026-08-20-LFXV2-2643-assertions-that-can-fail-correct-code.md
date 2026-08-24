# 2026-08-20 — LFXV2-2643 two assertions that could not do what they claimed

**Fix** — second batch of suppressed-review fixes on PR #159, and the two findings are the
same defect pointing in opposite directions: an assertion that can fail CORRECT
code, and a snapshot that cannot fail INCORRECT code. Both were introduced by
commits on this very branch that were fixing that exact class elsewhere.

**A substring search that can fail correct encryption.** The round-trip test's
negative arm ran

    bytes.Contains(got.EncryptedCredentials, []byte("secret"))

and reported a hit as "the stored blob is not sealed". AES-GCM ciphertext is
pseudorandom, so it can legitimately contain the six bytes `secret`. The blob
here is 54 bytes (12-byte nonce + 26-byte ciphertext + 16-byte tag), and
`secret` has no proper self-overlap, so the exact probability is

    (54 - 6 + 1) / 256^6 = 49 / 281474976710656 = 1.74e-13

about **1 in 5.7 trillion**. Small, and that is not the point: it is NONZERO, so
correct encryption has a random failure mode — which is precisely the class of
flake `sealTextHostile` was added to this same file, one function away, to
eliminate. The commit that retired a probabilistic precondition left a
probabilistic assertion standing.

The absence of the substring proved just as little in the other direction. An
encryptor storing ROT13 of the plaintext, or the plaintext with a single byte
flipped, contains no literal `secret` and passed.

The replacement is a deterministic invariant that holds for EVERY sample rather
than almost all of them: the stored blob's LENGTH must equal
nonce + len(plaintext) + tag. GCM is a stream mode, so the ciphertext is exactly
as long as the plaintext, and the identity is exact. It is also the invariant a
passthrough genuinely violates (26 bytes, not 54), and it catches the encoding
wrappers a substring search cannot see at all — a hex or base64 blob, or one
missing its nonce or tag, is the wrong length.

The sizes are READ, not pinned. `gcmSizes` builds a `cipher.AEAD` the way
`crypto.NewAESGCM` builds its own and takes `NonceSize()`/`Overhead()` off it.
Hard-coding 12 and 16 would turn a future nonce-length change into a false
failure, and exporting the constants from the `crypto` package would widen the
production API for a test — the instinct this ticket's own log already records
as wrong.

Mutation, and the CONTRAST is the evidence. A plain passthrough is not a valid
probe here: the `bytes.Equal(blob, plaintext)` arm above already kills it, so it
says nothing about either assertion. Hex-encoding `Encrypt`'s output is not
valid either — it is text-SAFE, so `sealTextHostile` fails first and the blob
never reaches the assertion. The probe that isolates exactly this arm keeps the
output binary and changes only the shape: append 8 random bytes to the sealed
blob (with `Decrypt` stripping them, so the round-trip still closes and nothing
but the shape is under test).

    OLD (bytes.Contains) : ok    ...postgres/dbtest  0.205s
    NEW (length identity): FAIL  the stored blob is 62 bytes, want 54 (a 12-byte
                                 nonce + 26-byte ciphertext + 16-byte tag); it
                                 does not have the shape of AES-GCM output, so
                                 it is not a sealed credential

The old assertion SURVIVES the mutation the new one kills, which is the whole
claim: the replacement is not merely deterministic, it is strictly stronger.

**The schema snapshot could not see a sequence.** `schemaObjects` in
`migrate_down_live_test.go` selected tables, columns, indexes and constraints —
and no sequences, though migration 000010 creates `index_outbox_id_seq` via
`id BIGSERIAL PRIMARY KEY`. A sequence's increment, bounds, cache size and cycle
flag are carried by no other object in that list, so a down file that restores
the sequence with a different increment leaves every row the query produces
byte-identical, and the test's stated exact-inverse guarantee passed over a
schema that hands out different primary keys.

This is the SAME defect the previous commit on this branch fixed for column
type and default: fixed for one object class, left for another. The snapshot now
carries a `pg_sequences` arm with the defining parameters. `last_value` is
deliberately excluded — it is the sequence's runtime position, advanced by
whatever rows a test inserted, so including it would make the snapshot depend on
activity rather than schema and fail on a correct down file. Ownership is
omitted too; the column arm already renders it as the DEFAULT expression
`nextval('index_outbox_id_seq'::regclass)`.

Proven by mutation. The site is 000011's DOWN file — chosen because it runs
while `index_outbox` and its sequence still exist, and because only a down file
is a valid mutation site here: `schemaAtVersion` builds its reference by
migrating a fresh database UP, so mutating an up file moves both sides equally.
That mutation-design note is recorded in the sibling 2026-08-20 entry and it
applies unchanged. Appending to 000011's down:

    ALTER SEQUENCE index_outbox_id_seq INCREMENT BY 7;

leaves every table, column, index and constraint untouched. The contrast:

    PRE-FIX  : ok    ...postgres/dbtest  4.780s
    POST-FIX : FAIL  stepping DOWN from version 11 did not restore the schema
                     that migrating UP to version 10 produces:
                       UNEXPECTED (the down did not remove it):
                         sequence:index_outbox_id_seq bigint START 1 MIN 1
                         MAX 9223372036854775807 INCREMENT 7 CACHE 1 NO CYCLE
                       MISSING (the down removed too much):
                         sequence:index_outbox_id_seq bigint START 1 MIN 1
                         MAX 9223372036854775807 INCREMENT 1 CACHE 1 NO CYCLE

With the mutation reverted the fixed test is green, so the new arm does not fire
on the real migration set.

**What generalises.** Both fixes landed on top of commits that had just fixed
the same shape of bug in a neighbouring line, and neither swept for the rest of
the class. A snapshot is a claim about a set of object KINDS, and adding one
kind is not the same as enumerating them; an assertion whose failure depends on
sampled bytes is a flake whether the sample is a precondition or a conclusion.
The question to ask of either is not "does it pass" but "what input would make
it wrong", asked of every arm at once rather than of the line under review.

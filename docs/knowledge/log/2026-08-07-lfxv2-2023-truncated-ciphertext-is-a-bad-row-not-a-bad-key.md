# 2026-08-07 — LFXV2-2023: a truncated credential blob paged ops instead of naming the bad row

**Update** — `AESGCM.Decrypt` rejected a sealed value only when it was shorter than the
NONCE. GCM output is nonce + ciphertext + a 16-byte authentication tag, and `Seal` appends
that tag to every message including an empty one, so the real minimum is
`NonceSize()+Overhead()`. Any value in the window between the two minima — at least a full
nonce, but short of nonce+tag — is provably truncated: it cannot be output this package ever
produced, and no key could open it.

**Fix** — That window fell through to `aead.Open`, failed authentication, and was therefore
classified as `ErrDecryptionFailed` → `domain.ErrCredentialDecryptionFailed` → **500, page
ops**. That arm means "the deployment's application key is wrong or rotated", which implies
every project's connection is broken at once. A single damaged row would send someone to
look at the key. The length guard now covers the full minimum, so the window is
`ErrCiphertextTooShort` → `domain.ErrCredentialsMalformed`, which `credsSource.resolve`
wraps with `ErrConnectionNotUsable` → **400 about one row**. Both ends of the boundary are
pinned by test: every length in `[nonce, nonce+tag)` must be rejected as malformed, and a
genuine seal of empty plaintext — exactly `nonce+tag` bytes — must still open. Deleting the
`+overhead` term fails that test.

**Fix** — The doc comment on `ErrCiphertextTooShort` said this condition maps to 422. It
does not and never did: `resolve` wraps `ErrCredentialsMalformed` with
`ErrConnectionNotUsable`, whose contract is 400. A comment naming a status the code does not
produce is worse than none — the next person mapping a new caller would have propagated it.

**Note** — The two failures are classified apart precisely because their BLAST RADII are
opposite: one bad row versus every connection. That is only useful if the boundary between
them is drawn where the evidence actually is, and "shorter than a nonce" was not that
boundary. What remains on the 500 path is genuinely ambiguous — a wrong key and a corrupted
(but full-length) row are indistinguishable to GCM — and the service arm now says so
instead of asserting the deployment-wide reading.

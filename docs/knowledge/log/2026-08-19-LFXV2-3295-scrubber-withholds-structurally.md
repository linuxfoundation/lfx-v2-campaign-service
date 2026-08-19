# 2026-08-19 — LFXV2-3295 the image-URL scrubber withholds structurally

**Fix** — `scrubURLFromErr` was fail-open. It replaced the caller's image URL by
exact substring match and then verified the result carried no recognizable fragment
of the URL's query/fragment (`urlSecretResidueFree`, prefix-matched). A reviewer
supplied a counterexample that defeats both halves, and it reproduces exactly:

    raw URL: https://cdn.example.org/a.png?sig=SECRET_SIG
    echo:    ...refused https://cdn.example.org/a.png?sig=SEC\nRET_SIG
    urlSecretResidueFree -> true   (message emitted, credential persisted)

Two properties combine. The parameter NAME `sig` is 3 runes, below the 6-rune floor
the residue scan used to avoid matching ordinary prose, so it was skipped outright.
The VALUE is wrapped mid-token by the upstream renderer, so no contiguous run of
`SECRET_SIG` at or above the floor survives for `strings.Contains` to find. The
exact-match `ReplaceAll` misses for the same reason. The credential then reached the
persisted, unencrypted `Steps` sink.

The previous test suite could not catch this. Its "line-wrapped echo" case wrapped a
LONG parameter name (`X-Amz-Signature`), whose own surviving prefix tripped the
scan — so the test passed for a reason unrelated to the value it claimed to protect.
Confirmed by probing the helper directly: in that fixture the full value
`SECRET_SIG_ABCDEF` was still present in the message the check called clean.

The deeper problem is that the approach cannot be repaired by widening the scan.
The text arriving at this sink has been through transformations the replacement
cannot invert and a verifier cannot enumerate — `do` truncates a non-Graph body at
300 runes, clipping a signature mid-value, and a proxy or WAF may re-encode, wrap,
or otherwise re-render the echo. Any substring verifier only rejects the residues it
thought to look for; lowering the floor to catch `sig` would make it withhold on
ordinary prose instead. Proving arbitrary transformed text clean is not a thing
substring checks can do.

So the rule is now STRUCTURAL, keyed on the input rather than the output. If the
image URL carries a query or fragment — where a pre-signed URL keeps its signature —
upstream-derived text is never emitted; the step is `redactURL(raw)` plus a fixed
withheld note. If it carries neither, there is no secret to protect and the message
is kept with the URL replaced in place, so the diagnostic survives wherever keeping
it is safe. `urlSecretResidueFree` and its rune floor are deleted. An unparseable
URL is treated as secret-bearing: that is the case where a delimiter scan is least
trustworthy, and withholding is the safe answer.

Two smaller corrections rode along, both from suppressed reviewer comments that were
right. The withheld branch now honors `max` — `redactURL` preserves the
caller-controlled path, so a long path produced an unbounded persisted Step while
callers passed 300. And the source comments describing a per-variant image UPLOAD
were rewritten: there is no upload, the URL travels as `link_data.picture` on the
creative.

Scheme, host and path deliberately still survive. A separate suppressed comment
argued that retaining them is itself a leak; `pkg-redact.md` records the opposite as
the repo-wide convention — "the host and path survive wherever they can be
identified. They are the whole diagnostic value" — and `redactURLForError` in the
googleads and twitter clients behaves the same way. Without them the step no longer
says which image failed.

**Verification** — three mutations, each compiling and each reverted:

- Removing the structural withholding (`_ = urlHasSecretMaterial`) fails
  `TestScrubURLFromErrFailsClosed` on four sub-cases, printing the surviving
  credential, including the short-param wrapped case the old verifier passed.
- Dropping `truncate` from the withheld branch fails the clamp case at 510 runes.
- Reporting an unparseable URL as non-secret initially SURVIVED — no test covered
  that branch. That is a real gap in the fix as first written, not a nit: it is the
  fail-closed arm. `an unparseable URL is treated as secret-bearing` was added and
  the mutation now fails with the leaked message printed.

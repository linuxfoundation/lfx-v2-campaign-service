# 2026-08-21 — LFXV2-3295 the /adimages part name is undocumented; the one-entry guard is the real fix

Review of PR #172 raised six findings against the by-stored-bytes creative path.
Two concerned the `/adimages` upload, and checking them against Meta's actual
published surface INVERTED which one was the bug.

## The field-name finding was a false choice — and the docs are silent, not silent-on-my-search

Two reviewers independently claimed the multipart part named `source` was wrong and
that Meta's create surface requires `bytes`. The strongest-looking evidence was
internal: `uploadImage`'s own doc comment named `bytes` and `copy_from` as the
accepted parameters while the code sent `source`. A function contradicting its own
contract note is a real smell, and it is what made the finding persuasive.

It was still wrong as stated. What Meta actually publishes:

- The `POST /act_<id>/adimages` reference documents exactly TWO create parameters,
  `bytes` ("Image file", typed **"Base64 UTF-8 string"**) and `copy_from`.
- It documents **no multipart file field at all** and describes no multipart upload.

So the parameter list is about a DIFFERENT transport — base64 in a scalar field —
and "the list says `bytes`, therefore the multipart part must be named `bytes`" does
not follow. The docs are SILENT on the mechanism the code uses.

The load-bearing evidence is that Meta's two **official SDKs disagree with each
other**, both in production against this same endpoint:

- Python (`facebook_business/api.py`, `FacebookRequest.add_file`):
  `file_key = 'source' + str(self._file_counter)` ⇒ uploads under **`source0`**
- PHP (`AdImage.php`): `getFileParams()->offsetSet(AdImageFields::FILENAME, ...)`
  where `FILENAME === 'filename'` ⇒ uploads under **`filename`**

Two vendor SDKs, two names, one endpoint ⇒ the handler accepts any file part. Note
neither sends `bytes`, and neither sends bare `source`. **The field name was
therefore left unchanged** — changing it on the strength of the review would have
been churn justified by a claim no source supports. What was fixed is the doc
comment that created the contradiction.

The filename **extension** question stayed unresolved in the honest sense: no
authoritative source states a rule either way, and Meta sniffs content for format.
It is asserted nowhere in the code or tests. *"The docs do not say"* was the answer,
not a failure to find one.

## The response-shape finding was the real bug, and worse than "randomized iteration"

The documented response is `Map { string: Map { string: Struct { hash, url, ... } } }`
under `images`, and the PHP SDK shows what the outer key IS: `images[basename($filename)]`
— **the basename of the file we uploaded**. A key this client never derives.

`uploadImage` had been iterating the map and returning the first non-empty hash. That
is not merely non-deterministic; it is a workaround for never computing the key. And
because Go **randomizes map iteration order**, a response with more than one entry
returned an ARBITRARY hash — which becomes `link_data.image_hash` on a creative that
spends money. The failure mode is the wrong creative on a live paid ad, silently.

Fix: enforce the invariant instead of guessing. One upload yields one entry, so
`len(out.Images) != 1` is refused as a `transportError`, and an empty hash is refused
as before. The single entry is still read BY VALUE — deriving the basename key would
pin the client to a contract documented only in SDK source. If the uploaded filename
ever changes, the response key changes with it; reading by value is what makes that
harmless.

## Two mutations, one survivor, and what caught it

- Deleting the count guard entirely → **killed** (multi-entry fixture, 3 distinct
  non-empty hashes, so every iteration order returns a hash and none errors; the old
  code fails 20/20 runs rather than 2/3 — the assertion is on the REFUSAL, not on
  which hash won).
- Weakening `!= 1` to `> 1` → **SURVIVED** at first. A zero-entry response fell
  through to the pre-existing "returned no hash" error, so both arms produced an
  error and a coarse "did it fail?" assertion could not tell them apart. This is the
  behaviour-preserving-mutation shape: the test agreed with itself. Killed by
  asserting WHICH arm fired (`want exactly 1` vs the no-hash message) per fixture.

## The container wiring was green with the wiring deleted

Both container tests called `registerDispatchers(nil, nil, nil, nil)` and the
dispatch tests bound the repo by hand, so **deleting
`metaDispatcher.SetCreativeAssetRepo(creatives)` compiled and left the whole suite
green** while every asset-backed production dispatch failed with "the creative-asset
store is not configured" — the brief's creative uploads fine, then no ad is ever
created. An error-based assertion cannot close this: unbound and bound-but-not-found
both fail the variant. Fixed with `MetaDispatcher.CreativeAssetRepoIsSet()`, the same
introspector `BriefService` already exports for this exact class, plus a test binding
the real `postgres.NewCreativeAssetRepo`. Mutation-verified: deleting the setter now
fails, and `internal/dispatch` stays green under it — confirming the gap was real.

## Asset resolution is now bounded, aligned with #170 rather than a third scheme

Nothing caps variant count and each asset may be 30 MiB, so resolution performed one
DB read and retained one buffer PER VARIANT — and the cheapest trigger was the SAME
asset id repeated, needing no extra stored data. Two changes:

- **Dedupe by asset id**: N variants naming one asset cost one read and one buffer;
  the variants alias it (read-only on the way to the wire).
- **Aggregate ceiling** `maxVariantAssetBytes = 240 MiB` (8 maximum-size assets),
  charged once per DISTINCT asset and checked BEFORE the buffer is retained.

240 MiB is derived from the caps #170 already set — all anchored to the same 30 MiB
per-asset upload ceiling (`MaxRequestBodyBytes` 42 MiB, decode budget 80 MiB) —
rather than being invented here. Against a legitimate campaign it is generous: real
Meta creatives are a few hundred KiB (the recommended feed image is 1936x1936), so
the bound refuses only already-pathological configs. A 50-variant test at 400 KiB
each passes, which is the assertion that the bound does not reject real work.

## Catalog

`imageAssetId` was undocumented in `docs/api-catalog.md` while `imageUrl` was fully
described. Since this provider config is opaque JSON rather than a typed Goa
attribute, the catalog IS the consumer-facing contract — the same gap raised on
cs#158. Now documents the asset route, brief-scoping, the pre-spend failure
semantics, the resolution bounds, and the mutual exclusivity with `imageUrl`
including what happens when both are supplied.

## Carry-forward

- **A function contradicting its own doc comment is a real signal, but it does not
  tell you WHICH side is wrong.** Here the comment was wrong and the code was fine.
- **Two official SDKs disagreeing is stronger evidence than a parameter table**, and
  it proves leniency that no amount of doc-reading would have shown.
- Never promote "the reference does not list X" into "the API rejects X" — the
  reference here described a different transport entirely.

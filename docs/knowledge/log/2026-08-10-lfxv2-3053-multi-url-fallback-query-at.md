# 2026-08-10 — LFXV2-3053: the multi-URL fallback read a query `@` as userinfo

**Fix** — the fourth shape `redact.URLUserinfo` leaked, in the branch the previous three fixes
did not touch.

The three leaks in `2026-08-10-lfxv2-3053-redact-credential-leaks.md` were all found and closed
in `redactOne`'s SINGLE-url path. The multi-URL fallback — the branch that runs when the value
holds a comma and a second `://`, i.e. the ambiguous NATS list `URLUserinfo` deliberately
refuses to split — still searched the WHOLE string for the last `@`. It is the same defect,
one branch over.

## The value

Copilot named it on PR #102:

```
nats://a,nats://b?access_token=prefix@live-secret,nats://c
```

The last `@` sits inside the query, between the token's `prefix` and its `live-secret` tail.
Rebuilding around it (`u[:authStart] + "***@" + u[last+1:]`) deletes everything before that `@`,
INCLUDING the `?`. `trimQueryAndFragment` then has no query to cut and the output is:

```
nats://***@live-secret,nats://c
```

`***` stands in for the harmless half of the token and the SECRET half is printed verbatim,
which is the worst possible arrangement — it reads like a successful redaction.

This shape is the sibling of the already-fixed
`https://idp.example?contact=a@b&access_token=s3cret`, and it is worth naming why the earlier fix
did not cover it: there the `@` was found by the single-url path's authority search, so bounding
THAT search at the query was enough. Here the value never reaches that code.

## The fix

`b7f0b3ee` bounds the fallback's `LastIndexByte` at the first `?`/`#`, matching what the
single-url path already does. Nothing before the query holds an `@` in this value, so it falls
through to `trimQueryAndFragment` and both hosts survive: `nats://a,nats://b`.

The bound stays safe when a real credential IS present before the query —
`nats://u:p@a,nats://b?x@c` still redacts to `nats://***@a,nats://b` — and stays conservative
when one sits after it: `nats://a?x=1,nats://u:p@b` prints `nats://a` and drops the rest rather
than reasoning about a region the query has already made untrustworthy.

## Regression guard

`TestURLUserinfo_NeverEmitsACredential` gains
`"multi-URL fallback does not leak a credential suffix after a query @"` with Copilot's exact
value, asserting absence of `live-secret`, `access_token` and `prefix` rather than only checking
the formatted output. Verified binding: removing the `preQueryMulti` bound reproduces
`nats://***@live-secret,nats://c` character for character, and fails the sibling
`"multi-URL fallback does not leak a query @ as userinfo"` case alongside it.

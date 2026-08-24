# 2026-08-24 — a per-request bound says nothing about the aggregate

**Security** — the creative-upload path had three carefully-derived size limits and still had no
answer to "how many at once".

`MaxRequestBodyBytes` (42 MiB) bounds one body. The design's `MaxLength(31457280)` bounds one
decoded slice. `maxCreativeDecodedBytes` (80 MiB) bounds one pixel buffer. Each was correct, each
was tested, and together they bounded exactly one request. Against a fixed pod memory limit that
is the whole problem: N concurrent, entirely LEGAL uploads multiply a per-request allocation
until the pod is OOM-killed, restarting the process and denying service to every tenant on it.
No amount of tightening a per-request number fixes an aggregate — the quantity being bounded is
different in kind.

The fix is a weighted semaphore (`internal/middleware/upload_admission.go`) whose budget is
derived from the chart's pod limit rather than chosen.

## Where the guard goes is the guard

The natural placement — wrap `image.Decode` inside `UploadCreativeAsset` — **cannot fix the
finding**, and it took reading the generated code to see why.

Goa's `NewUploadCreativeAssetHandler` calls `decodeRequest(r)` and only then `endpoint(...)`;
`NewUploadCreativeAssetEndpoint` calls `authJWTFn` as its FIRST statement. So the ~42 MiB body
read and the ~30 MiB base64 decode both complete **before any JWT is examined**. A semaphore in
the service method acquires after that allocation has already happened on behalf of an
unauthenticated caller: it would bound the 80 MiB decode tail, leave the pre-auth half wide open,
and — worst of all — read as though the finding had been fixed.

Admission therefore sits in the middleware chain OUTSIDE the mux, where the permit is taken
before the decoder touches the body.

The general shape: **an allocation guard is only as good as the earliest allocation it precedes.**
Before placing one, find where the first byte is actually committed — in generated or framework
code, that is often earlier than the handler you own.

## What the layer can and cannot own

Stated plainly in the code, because the honest scope is narrower than "fixed": this bounds the
pod's memory. It does not authenticate, and it does not stop an unauthenticated body from being
read at all — auth-before-body-read belongs at the gateway, not in this service. What it owns is
that such a read cannot happen without first taking a permit from a budget tied to the pod's real
limit. Claiming more than that would be the same species of error as the guard placement above.

## Two figures for one quantity

`20,000,000 px x 8 B/px = 160,000,000 B`. That is **160 MB decimal and 152.6 MiB binary** — one
quantity, two units. The file called it "~153 MiB" in one comment and "~160 MiB" in another, and
a previous log entry defended the second as "a correct historical statement". Both figures were
arithmetically defensible and the pair was still wrong, because a reader sizing a budget sees two
numbers ~7 MiB apart and cannot tell which unit either is in.

Corrected to state the unit explicitly and show the conversion, so the ambiguity cannot recur.

## Name what the bound accounts

A bound that under-counts is worse than none, because it reads as protection. So the weight's
scope is stated in the code rather than left to inference: it accounts the INBOUND request's
allocation, and nothing else.

The case that forces the distinction is dispatch-side amplification. Meta's `createVariantAd`
uploads per variant, so five variants naming one 30 MiB asset hold five copies. That is real, but
it is not an undercount of this bound: fan-out reads from Postgres in a dispatch worker, long
after the HTTP request returned and released its permit. Different lifetime, different code path,
and no request gated here can multiply its own resident bytes that way. Bounding it needs a
control in the dispatch path; this middleware neither provides nor claims one.

The general form: when documenting a resource bound, state the quantity it accounts AND name the
adjacent quantity it does not, or a reader will assume the larger scope.

## The weight I first chose under-counted, and my own comment said why

The first revision charged 64 MiB per upload, reasoning that the 42 MiB body buffer is released
before the decode peaks so the sum was pessimistic. A reviewer pointed at the line the reasoning
had to survive:

```go
image.Decode(bytes.NewReader(p.Bytes))
```

`p.Bytes` is the decode's own INPUT. It cannot be collected while the decode runs, so the ~30 MiB
slice and the up-to-80 MiB pixel buffer **necessarily coexist** — ~110 MiB, before counting the
body buffer Go has not yet reclaimed. Two permits at 64 MiB would admit ~220 MiB against a
128 MiB budget: the bound under-counted precisely where it was supposed to bind.

The argument I had made was true of the body buffer and simply did not apply to the decoded
slice. Corrected to 128 MiB, which at the current budget admits one maximum-size upload at a time
— the honest answer for a 512Mi pod.

The test that now pins it derives its expectation from the SERVICE's limits (the design's 30 MiB
`MaxLength` plus `maxCreativeDecodedBytes`), never from the weight under test: an assertion
derived from the constant it guards proves only that a value equals itself, and that is exactly
how the 64 MiB figure passed its first review.

## A deadline I introduced outlived the one that answers

Adding `ReadTimeout = 120s` against an unchanged `WriteTimeout = 60s` created a window I had not
considered: `net/http` installs the WRITE deadline when the request HEADERS are read, and it keeps
expiring while the handler reads the body. An upload taking 60-120s could satisfy the new read
deadline and then have no budget left to send anything — the caller sees a dropped connection
rather than a response or an error.

My first correction was to hold `ReadTimeout` **equal** to `WriteTimeout`. A second reviewer
caught that this is still wrong, from the other side: the write deadline keeps expiring through
the handler too, so equal budgets let a slow body consume the whole write deadline and leave
nothing for `image.Decode`, the insert and the response — the same dropped connection, reached a
different way. **Equality was not sufficient; the read budget has to be strictly smaller by a
reserved margin.**

Final shape: 15s headers, 90s body read, 30s named `UploadHandlerHeadroom`, inside a 120s write
budget. `WriteTimeout` was raised rather than the read budget squeezed, because a 42 MiB body is
~67s at 5 Mbps and cutting it short would reject legitimate uploads. The inequality
`ReadTimeout + UploadHandlerHeadroom <= WriteTimeout` is asserted on the real server object.

The general shape: **a new timeout is not independent of the existing ones.** Before adding one,
find which deadlines share a clock and in what order the runtime installs them — and note that
correcting an inequality to an equality can leave the same defect, because the margin, not the
ordering, was what the failure needed.

## The rule

A bound on one request is not a bound on the service. When a per-request limit is added, ask what
happens when N of them arrive together — and derive the aggregate from the real resource ceiling
(here, the chart's `limits.memory`), with a test that fails when the two drift apart, rather than
from a constant chosen to look reasonable.

When stating a byte quantity in a comment, name the unit and show the conversion. "160" and "153"
can be the same number.

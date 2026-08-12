# 2026-08-10 — LFXV2-2775: two review nits that landed after #104 merged

**Fix** — PR #104 merged with two reviewer nits still open. Neither is a behaviour change; both
are recorded here because the reasoning for the second one is the part worth keeping.

## 1. `len([]rune(s))` where `utf8.RuneCountInString(s)` was already the house style
(dealako, `internal/service/email_copy.go:159`)

The body-length bound in `parseEmailCopyResponse` counted runes by materialising a `[]rune`
— an allocation of 4 bytes per rune, thrown away one line later. Every other rune count in
this file (the prompt bounds in `GenerateEmailCopy`) already used `utf8.RuneCountInString`,
which counts without allocating. Same result.

**8000 is the wrong number to size that allocation by**, which is the part worth writing down.
`maxBodyRunes` bounds what the function ACCEPTS; it does not bound `parsed.Body`, because
`parsed.Body` is whatever the model returned and the check exists precisely to reject it when
it is larger. The real ceiling is `internal/platform/llm`'s `maxResponseBody = 8 << 20`, so the
discarded slice could reach ~32 MiB — a thousandfold more than the accepted case, and reached
on exactly the oversized-body path the guard is there for. So the allocation was proportional
to model-controlled input rather than to the limit, which makes this a little more than style;
still not a defect (the 8 MiB read cap keeps it bounded), but the reason it is worth removing
is not the one the nit was filed under.

## 2. `llmCalled` is now an `atomic.Bool` (Cursor, `internal/service/email_copy_test.go`)

The flag is written on the `httptest` handler's goroutine and read on the test's, and the atomic
is what synchronizes them.

**An earlier version of this entry got the reasoning wrong and is corrected here**, because the
wrong version is the more instructive one. It said the race "is not reachable" since reading the
response body to completion establishes a happens-before edge with the handler's return, citing
`go test -race -count=50` clean.

The first half does not hold: a body read orders nothing with respect to the handler's RETURN,
since the client can consume bytes the handler has already written while the handler is still
running.

The second half was ALSO wrong, in a second attempt at this entry, and is worth recording as its
own mistake. That attempt said the clean runs proved nothing because the detector only reports
races it "observes", i.e. that interleave in wall-clock time. That is not how it works: TSan
records executed accesses and orders them by happens-before, not by whether they overlapped. Both
accesses here execute on every successful request, so wall-clock interleaving was never the
question. The honest reading is narrower — the clean runs are evidence about the schedules that
ran, and the atomic is what makes the ordering a property of the code rather than of net/http's
internals.

So the atomic is not belt-and-braces over an unreachable race; it is the synchronization. That
also makes the sibling `internal/platform/llm` tests' discipline the right precedent rather than
merely a consistent one. Both occurrences were converted
(`TestGenerateEmailCopy_AcceptsSizeablePrompt` as well as the one Cursor flagged) — a single
converted site would have read as if the two were different.

This is **not** a fix for a bug, and no test was added, because there is no failing behaviour to
pin. Adding one would have asserted something already true.

## Verification

`go build ./...`, `go vet ./...`, `golangci-lint run ./...` clean; `go test -race -count=3` green
on `internal/service`.

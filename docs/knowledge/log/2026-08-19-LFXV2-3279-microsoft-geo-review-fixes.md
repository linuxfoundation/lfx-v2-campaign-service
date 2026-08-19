# 2026-08-19 — LFXV2-3279 Microsoft geo review fixes

**Fix** — Pre-PR reviewer simulation on the LFXV2-3279 geo branch found six real defects in the
original commit, three of them proven with throwaway probe tests rather than argued. All are
fixed here, and each fix is pinned by a test verified with a compiling revert.

**A reused campaign was re-attached, duplicating location criteria on every retry.** The attach
step did not consult `alreadyExisted`, and the dispatcher retries by design — `NameSuffix =
brief.ID` composes the same name so the lookup REUSES the campaign. The keyword precedent does
not transfer: `AddKeywords` may be re-posted because Microsoft actively refuses a duplicate
keyword (1517/1542), and `AddCampaignCriterions` publishes no equivalent refusal for a location
criterion. So every retry would have widened the criterion list on a live paid campaign. The
attach is now skipped on a reused campaign, with an explicit steps line saying why. The concept
doc had already claimed this behaviour while the code did the opposite — the doc sentence was
false, and the code has been moved to match it rather than the other way round.

**One caller's cancellation failed unrelated concurrent creates.** The single-flight leader ran
`fetchGeoLocations` inline on its OWN context and published the result to every follower, so a
leader that timed out handed `context canceled` to followers whose contexts were still live.
Because a geo failure aborts `CreateCampaign` before any mutating call, that turned one client's
timeout into other campaigns refusing to create. The comment claimed to mirror
`accessTokenValue`, which is exactly what it did not do — that helper runs its refresh on a
`WithoutCancel`-detached goroutine for precisely this reason. Now it does too.

**A panic in the fetch wedged geo resolution for the process.** `inflight` was published and
cleared by straight-line code with no `defer`, so a panic anywhere in the fetch (JSON, gzip, CSV,
or a caller-supplied `RoundTripper`) unwound past the clear, leaving `inflight` set and `done`
never closed. Every later caller then waited on that channel forever. Publishing now happens
under a `defer` with a `recover` that converts the panic into an error for the waiters.

**`geoDownloadTimeout` was dead configuration.** The shared client carries
`Timeout: msAdsRequestTimeout` (30s), and `http.Client.Timeout` covers the BODY READ and is not
extended by a longer context — so the stated 3-minute budget did not exist and a multi-MiB CSV
on a slow link died at 30s, failing the whole create. The download now runs on a shallow copy of
the client whose `Timeout` is the download budget, preserving the no-follow redirect policy.

**The pre-signed FileUrl could leak through one error path.** `http.NewRequestWithContext`
failures were wrapped with `%w`, and `net/url` builds a `*url.Error` carrying the full URL —
whose query string IS the credential. Every neighbouring path was already careful about this;
this one is now too, matching `downloadReport` in `metrics.go`.

**Two lower-severity fixes.** A non-2xx download returned without draining the body, costing
connection reuse against the repo's own established pattern; it now drains a bounded amount to
`io.Discard` first. And the row cap silently truncated an oversized file into a partial map that
parsed cleanly and was then cached for 24h, failing every create whose country sat past the cap
with an error blaming the operator's input — it now refuses, matching the byte cap, which is
deliberately sized `+1` so overflow is DETECTED rather than silently applied.

**Also removed a dead field and two unread ones.** `targeting.geoCriterionIDs` was written and
never read — the ids reach every result through the `campaignPartial()` closure, not through
that struct, so the field's comment claimed a mechanism that was not the real one. The
`GeoLocationsFileUrl/Query` response struct decoded `FileUrlExpiryTimeUtc` (into a typo'd
`FileURLExpiryTimeUv`) and `LastModifiedTimeUtc`, neither ever read; a field that is parsed but
never read is a claim that something checks it.

**Coverage gaps the review found.** A coverage run showed two of the three geo
error-classification arms were never executed, and the partial-with-ids return in
`createCampaignCriterions` never ran — the arm a reconciler depends on to avoid re-attaching
criteria that already exist. Both are now driven, along with the dispatch-layer field mapping
(`internal/dispatch/microsoft_geo_test.go`, mirroring the `googleads_geo_test.go` precedent),
which nothing had covered: every platform-client test constructs `CampaignInput` directly and so
cannot see the dispatcher's assignment.

**Mutation results.** Nine further mutations run against these fixes, all killed: re-attaching on
reuse, dropping the `errNoID` disjunct from the UNCONFIRMED arm, returning nil instead of the
created ids on a partial, breaking instead of erroring at the row cap, undoing the context
detach, and removing the `recover`. Two initially SURVIVED and are recorded because a surviving
mutation is the finding: the row-cap fix had no test at all (added), and the dispatch ordering
test passed against a reordered implementation because its own fixture returned a bare `{}` for
the lookup, making the create unreachable for an unrelated reason — the fixture now returns the
shaped absent responses so the mutant is caught by the guard under test rather than by an
accident.

**Non-findings, checked rather than assumed.** `strings.ToLower` is locale-independent in Go, so
the Turkish dotted/dotless I does not break the `Türkiye` join in either direction. Unicode
normalisation IS a real gap for the four non-ASCII names (`AN`, `CI`, `ST`, `TR`) if Microsoft
ships them decomposed or ASCII-folded — but it fails CLOSED, refusing the create rather than
mis-targeting, which is the correct direction; it is left as a known limitation rather than
fixed speculatively. `bufio.Peek(2)` does not short-read on a slow chunked body. The
decompression cap is correctly re-applied after gzip and overflow is detected, not truncated.

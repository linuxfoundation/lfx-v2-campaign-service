# 2026-08-07 — LFXV2-2993: shape-based redaction cannot cover this client's own token

**Update** — Review found two defects in the error-hygiene work, both of the same
family: a guard that looks complete and is not.

`redactCredentials` is shape-based. Its Bearer alternative matches
`[A-Za-z0-9._~+/=-]`, but `Credentials.AccessToken` is accepted after trimming and
nothing else, and is sent verbatim. Meta's own app access tokens are
`{app-id}|{app-secret}`; `|` is outside that alphabet. So a reflected
`Authorization: Bearer 12345|SECRET` was redacted to `Bearer [REDACTED]|SECRET` — the
app secret intact, behind a redaction marker that made it read as handled. A leak
wearing the marker survives review in a way a bare one does not.

The fix is not a wider regex. Widening the alphabet is another guess at the token's
shape, and the next unanticipated character defeats it the same way. `Client.redactSecrets`
now replaces the CONFIGURED token by exact value first, then runs the shape-based pass
for credentials this client never held (a Meta-constructed `paging.next` URL carries its
own `access_token`). Secrets under 8 bytes are skipped — at that length a substring
replace matches ordinary prose and shreds the diagnostic for nothing.

The second defect was in the test that was supposed to pin the no-echo property.
`TestGetCampaignMetrics_MalformedValuesAreNotEchoed` planted `-1SECRETMARKER` for the
"negative impressions" case — which does not parse, so it stopped at the syntax branch
and the `n < 0` guard was never executed. Three spend cases (`NaN`, `-1.00`, `1e300`)
carried no marker at all, so restoring `%q` interpolation would have left them green.
Branches reached only AFTER a value parses cannot be probed with a free-text marker;
they need a distinctive NUMERIC literal. Reverting the guards to `%q` now fails exactly
the four cases that were previously unbinding — negative impressions, negative clicks,
non-finite spend, negative spend — which is the check that should have been run when the
test was written.

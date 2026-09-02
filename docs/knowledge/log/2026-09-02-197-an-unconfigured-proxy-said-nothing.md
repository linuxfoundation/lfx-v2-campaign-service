# 2026-09-02 — #197: an unconfigured AI proxy said nothing

**Fix** — `newLLMClient` returned `nil` with no log line when `AI_PROXY_URL` or `AI_API_KEY` was
absent. The warning that existed fires only when `llm.NewClient` ERRORS, which a missing value
never reaches, so the missing-config path — the actual production state — was silent.

Found by investigating HubSpot failures on `app.lfx.dev`: three replicas, no LLM line in any
startup log, and no way to tell a configured deployment from an unconfigured one without reading
the secret. `sso-readonly` cannot read secrets, so the question could not be answered from the
cluster at all. The only evidence email copy generation was disabled was a 503 in a browser.

It was also inconsistent. The same startup announces every other optional dependency —
`"snowflake not configured; audiences will be built from the event's country only"` and the index
relay's `"no service credential configured (set INDEXER_SERVICE_TOKEN)"` — each naming the variable
and the consequence. The AI proxy now matches them.

WHICH of the two values is missing is deliberately not logged: it would narrow the fix, but they
are a URL and a credential, and naming the present one tells a log reader something about a secret.
The chart provisions both together.

Two tests, both mutation-confirmed: the warning must fire for each unconfigured shape (neither set,
url only, key only) and must NOT fire when configured — a false alarm on a healthy deployment
devalues the real one. They assert the emitted LOG rather than the nil return, because the nil
return was never the defect; the silence was.

# 2026-11-13 — #577 Log Google's error code and message when a mutate fails

**Fix** — A refused Google Ads request logged only its method, path and status
(`apiError.Error()`), so a refusal such as `INVALID_CALL_TO_ACTION_TEXT` could only be diagnosed
by re-running it live.

- `doRequest` now logs "google-ads request refused" at WARN with `method`, `path`, `status` and
  `google_errors` for every non-2xx answer.
- `parseErrorSummaries` (`campaign.go`) renders each Google error as
  `<category>.<CODE>: <message> (<field path>)`.
  - It keeps at most 16 errors.
  - Each message is capped at 200 runes.
  - The field path is capped at 32 elements, and an over-long element is dropped.
  - Control characters are stripped, so a body cannot forge log lines.
- `Error()`, the job result and `DispatchFailureMessage` are unchanged. Google's text stays in the
  log (#208).
- Tests: `TestParseErrorSummaries` and `TestDoRequest_LogsGooglesErrorOnRefusal`.

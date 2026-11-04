# 2026-11-04 — #470 a repeated resource id in a confirmed mutate result is UNCONFIRMED

**Fix** — A Copilot review thread on #470 (PRRT_kwDOTI9SG86zZo13) found that the Performance Max
signals loop accepted a 2xx naming the same `assetGroupSignals` resource twice. The fix was
promised for every loop, not just that one.

No confirmed-result loop in `internal/platform/googleads` checked that the resource ids in a
2xx were distinct. Each checked the result count, the resource kind, the account and the
parent. A response that repeated one resource therefore confirmed n operations while
accounting for n-1 resources, and recorded one id where two may exist.

`result_ids.go` adds `resultIDSet`, the one shared check. A repeat is UNCONFIRMED and the error
names both indexes. Each loop keeps its existing id contract:

- The criteria loops return no ids. These are campaign criteria, both geo loops, campaign
  negative keywords, ad-group criteria, Demand Gen audience and language, Display audiences,
  placement exclusions, both #475 audience exclusion loops and the Performance Max signals.
  The Search ads loop and the extension asset loop also return no ids.
- The creative asset loops treat the repeat as an unusable entry and return every valid id
  once (`uniqueIDs`, #482). These are the Demand Gen image ad, the Demand Gen video ad's atomic
  request, Display, Video and Performance Max.

Not changed: the extension link loop and the Performance Max link count match each result
against their own asset ids, which are now distinct. `goalMutateConfirmed`,
`checkStatusMutateResults` and the keyword actions match against the exact names their
operations sent. In each, a repeat already leaves one expected name unmatched. The Demand Gen
video logo upload sends exactly one operation.

`result_ids_test.go` adds one repeated-id case per changed loop. Disabling the helper fails 20
tests, and disabling each of the 18 call sites fails that site's own case.

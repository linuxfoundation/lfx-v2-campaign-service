# 2026-09-01 — LFXV2-2641: four findings hidden behind "Suppressed comments"

**Fix** — Copilot collapses some findings into a `<details>Suppressed comments (N)` block inside
the review BODY. They are not review threads, so `reviewThreads(isResolved: false)` returns zero
for them — which is how this PR was reported as having **0 open threads** while four real findings
sat unread. Misha spotted the block in the GitHub UI; no query I was running would have.

Two were behavioural and both are fixed:

- **`NoUpstreamCreate()` ignored.** `CreateHubspotCampaign`'s setup block enumerated four
  sentinels. `connLoadFailed` — a repository error loading the connection row — is none of them,
  yet it carries the `NoUpstreamCreate()` marker precisely because the create never started. It
  fell through to "the campaign may already exist", sending an operator to hunt in HubSpot for a
  create that provably never happened. The block now honours the MARKER as well as the sentinels,
  so the next pre-send failure the credential layer adds is covered without another edit.
- **401/403 rendered as a retryable 503.** `SearchCampaigns`' error was wrapped bare, so
  `classifyDiscoveryError`'s default arm made an invalid token look transient. `IsPermissionRejection`
  already existed for exactly this, with a docblock explaining the remedy differs — the search path
  simply never called it. Now tagged `ErrConnectionNotUsable`, which maps to an actionable 400.

Two were text: `"UNSPECIFIED (" + "creation)"` was a string-concat artifact that reached the
generated OpenAPI docs as nonsense, and a PR-description tenancy claim that outlived the code.

**How to find these:** sweep review BODIES, not only threads —

```bash
gh api repos/<org>/<repo>/pulls/<n>/reviews --paginate \
  --jq '.[]|select(.body|test("Suppressed";"i"))|.body'
```

**The general shape:** a count is only as honest as the query behind it. "0 open threads" was
true of the thing I measured and false of the thing it claimed. When a number is the basis for
"ready to merge", check what the query cannot see before reporting it.

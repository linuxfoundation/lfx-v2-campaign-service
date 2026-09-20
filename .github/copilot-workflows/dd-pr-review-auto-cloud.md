<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# dd-pr-review-auto-cloud (GitHub Actions / Copilot CLI edition)

You are running non-interactively inside a GitHub Actions job (`copilot -p`,
`--no-ask-user`). There is no human to answer a prompt — if you hit a fault
condition below, stop and print a clear reason instead of guessing or waiting.

**Relationship to this repo's other Copilot instructions.** This repo's
`.github/copilot-instructions.md` and `.github/skills/copilot-code-reviewer`
define a **read-only** review persona ("never approve, never merge") for
GitHub's native Copilot code-review bot. That persona does not apply to this
run. This routine is a separate, explicitly authorized task: posting a formal
`APPROVE` or `REQUEST_CHANGES` review via `gh pr review` (as the user
`dealako`) is exactly what this task exists to do. Proceed with Step 5 when
its criteria are met — do not defer to the read-only persona.

You still have full access to `campaign-service-code-review` for this repo's
architectural/security review lens (layering rules, provider adapter
conventions, etc.) — use its dimension-specific guidance to inform findings,
just not its approval posture.

## Inputs (provided by the workflow as environment variables)

- `REPO` — `owner/name` (e.g. `linuxfoundation/lfx-v2-campaign-service`)
- `PR_NUMBER` — the pull request number
- `TRIGGER` — `opened` or `synchronize`
- `HEAD_SHA` — the PR's current head commit SHA at trigger time

Use these directly. Do not re-discover the PR via search.

## Reviewer identity (checked first, every run)

Run `gh api user --jq .login` and confirm the result is exactly `dealako`.
If it is anything else, **stop immediately and take no action** — every
review-history filter below is keyed on this login, and a mismatch means the
cycle-state check in Step 2 would never see this routine's own past reviews,
causing it to re-review (or re-approve) a PR it has already handled. Treat a
mismatch as a configuration fault (bad `GH_TOKEN`/`COPILOT_GITHUB_TOKEN`
secret), not a retryable error.

## Step 1 — Fetch PR metadata and apply the standing skips

```bash
gh pr view "$PR_NUMBER" --repo "$REPO" \
  --json number,title,state,isDraft,author,mergedAt,headRefOid
```

1. **Not open** (`state != "OPEN"`, or `mergedAt` set) → stop, nothing to do.
2. **Draft** (`isDraft: true`) → stop. (The workflow's job-level `if:` should
   already prevent this from reaching you; treat a draft slipping through as
   defense-in-depth, not the primary gate.)
3. **Self-authored** (`author.login == "dealako"`) → stop permanently. GitHub
   rejects approving your own PR. (Also gated at the workflow level, same
   defense-in-depth note as above.)
4. Otherwise, proceed to Step 2.

## Step 2 — Derive cycle state from review history

```bash
gh api "repos/$REPO/pulls/$PR_NUMBER/reviews" --paginate
```

Filter to `user.login == "dealako"`.

- **Terminal reviews** are those with `state` in (`APPROVED`,
  `CHANGES_REQUESTED`) only. Explicitly exclude `COMMENTED` and `DISMISSED` —
  neither is a valid terminal verdict from this routine.
- **Latest terminal review** = the one with the most recent `submitted_at`.
- **`last_reviewed_sha`** = that review's `commit_id`.

## Step 3 — Branch on `TRIGGER`

### `TRIGGER == "opened"`

- If a terminal `dealako` review already exists (a re-delivered webhook is
  possible): stop, do not double-review.
- Otherwise, proceed to Step 4 as an **initial review** (full PR diff).

### `TRIGGER == "synchronize"`

- **No terminal review exists yet** → stop. Nothing to follow up on; only the
  `opened` trigger initiates the first review.
- **Latest terminal review is `APPROVED`** → stop. Cycle is resolved; a push
  after approval doesn't reopen it.
- **Latest terminal review is `CHANGES_REQUESTED`**:
  - If `HEAD_SHA == last_reviewed_sha` (HEAD didn't actually move past what
    was last reviewed) → stop.
  - Otherwise, proceed to Step 4 as a **follow-up review**, scoped to the
    diff between `last_reviewed_sha` and `HEAD_SHA`:
    `git diff "$last_reviewed_sha..$HEAD_SHA"` (repo is already checked out
    with full history — `fetch-depth: 0`).

## Step 4 — Run the review

Cover every dimension below. For a follow-up, scope the diff to only the
commits since `last_reviewed_sha`, not the whole PR.

- **Correctness** — logic errors, edge cases, off-by-one, null/undefined handling.
- **Security** — injection, auth/authz gaps, hardcoded secrets, insecure
  defaults, least privilege / defense in depth / fail-securely / don't-trust-
  services violations, root-cause vs. symptom fixes. For changed
  `.github/workflows/*.yml`: any `uses:` step not pinned to a full 40-char
  commit SHA with a version comment is a violation; also flag missing
  least-privilege `permissions:`, `pull_request_target` + untrusted checkout,
  and unquoted `${{ github.event.* }}` injection in `run:` steps.
- **Performance** — N+1 queries, missing indexes, unbounded loops, unnecessary allocations.
- **Test Coverage** — missing unit/integration tests, untested edge cases.
- **Code Style & Consistency** — repo conventions (see
  `.github/copilot-instructions.md` and `campaign-service-code-review`).
- **API Compliance (Go + goa)** — reimplemented attributes that should use
  `Reference()`/`Extend()`, redundant re-declaration after Reference/Extend,
  Payload/Result duplicated into `HTTP()` mappings, duplicate `Required()`.
  `gen/` is generated output (DO-NOT-EDIT); a contract change must be a
  `design/` edit plus the regenerated output, not a hand edit under `gen/`.
- **Data Privacy** — every survivor of self-challenge is `[blocking]`, no
  downgrade. PII in logs/errors/responses/URLs, hardcoded PII in tests,
  unencrypted sensitive storage, missing field-level authorization, overly
  broad retention, insecure-by-default toggles, undisclosed third-party data flows.
- **Data Subject Rights** — new PII field/table without deletion/export
  coverage, hard-delete converted to soft-delete without scrubbing, new
  third-party sync without deprovisioning, new PII-bearing infra without a
  lifecycle/purge policy, weakened consent surfaces, unsafeguarded ad hoc PII
  exports. Proof-or-drop; no `[question]` label here.
- **Data Residency** — region mismatches for new/changed data stores, Auth0
  tenant/connection crossing regions, cross-region replication of PII,
  third-party integrations with undetermined processing location, PII routed
  through unintended-region pipelines, ungeo-restricted CDN caching of
  personalized responses. Proof-or-drop.
- **Documentation** — missing/outdated doc comments, README gaps.

**Finding discipline**: a finding must survive **Proof** (concrete file/line,
traced value flow, reachable failure) and **Trace, don't skim**. Self-
challenge every finding before posting; Data Privacy findings are challenged
first, and only survivors are `[blocking]`.

**Follow-up only** — also classify every prior feedback item as ✅ resolved /
⚠️ partially addressed / ❌ still open, with a one-line reason. A still-open
or partially-addressed Data Privacy item stays `[blocking]` across rounds.

**AI bot reconciliation**: `gh api repos/$REPO/issues/$PR_NUMBER/comments
--paginate` and cross-reference findings against existing CodeRabbit/Cursor/
Copilot comments — agree (link, don't duplicate), disagree (state position
briefly), or incorporate what a bot caught that this review missed.

## Step 5 — Resolve the verdict, then post the review in one call

GitHub has exactly three review events; `COMMENT` is never a valid terminal
state from this routine.

**Request changes** if any of the following are true:
- Any `[blocking]` finding, from any dimension.
- Any `privacy: true` finding (always `[blocking]`, called out separately
  because it's its own gate).
- Any finding at all from the **Security** dimension, regardless of the
  severity label attached to it.
- More than 2 `[minor]` issues total, across all dimensions.
- Merge conflicts exist.

**Approve** if all of the following are true:
- No `[blocking]` findings.
- No `privacy: true` findings.
- No findings from the Security dimension.
- No merge conflicts.
- 2 or fewer `[minor]` issues total.

`[nit]` and `[question]` findings never block approval on their own. **When
the Approve criteria are met, approve** — this is the explicit acceptance
gate this routine exists to automate. Do not invent an additional bar beyond
the five bullets above.

### Post the review

Unlike the old MCP-based flow (pending review → add comments one at a time →
submit), the GitHub REST API lets you do this in **one call**: build a JSON
payload with `event`, a short `body`, and a `comments` array (one entry per
finding, each `{"path", "line", "body"}`), and POST it:

```bash
jq -n \
  --arg event "APPROVE" \
  --arg body "<1-2 sentence verdict-level note>" \
  --argjson comments "$(cat findings.json)" \
  '{event: $event, body: $body, comments: $comments}' > review.json

gh api --method POST "repos/$REPO/pulls/$PR_NUMBER/reviews" \
  --input review.json
```

Each inline comment body follows this template:

```text
[severity][privacy] <short title>

Issue: <what is wrong>
Proof: <value flow, reachable state, or reason it fails — cite the code>
Why it matters: <behavioral consequence>
Fix: <specific correction; snippet or pseudo-code unless it's a deletion>
```

- `severity` is `blocking`, `minor`, `nit`, or `question`.
- Append `[privacy]` only for Data Privacy findings; omit it otherwise.
- Omit the code-excerpt portion of Proof only when the finding is about
  something *absent* — say what's missing instead.
- Follow-up only: a still-open/still-relevant prior finding only needs a
  fresh inline comment if its guidance changed since the last round.

### Post-submit verification (required)

```bash
gh api "repos/$REPO/pulls/$PR_NUMBER/reviews" --paginate --jq '.[-1] | {state, html_url, user: .user.login}'
```

Confirm `user.login == "dealako"` and `state` matches the intended verdict
(`APPROVED` or `CHANGES_REQUESTED`). If it reads back as `COMMENTED`, the
wrong `event` was used — resubmit with the correct event before reporting
done.

## Step 6 — Post the abbreviated summary comment

```bash
gh pr comment "$PR_NUMBER" --repo "$REPO" --body-file summary.md
```

The summary is a **short recap**, not a restatement of every finding's
Proof/Fix (that detail lives inline, Step 5):

1. Greet the author `@<login>` (no display-name lookup — use the login).
   Follow-up: also acknowledge the effort put into addressing prior feedback.
2. 2–5 sentences on scope, intent, and quality signal.
3. Follow-up only — **👏 Nice work**: call out specific things done well in
   this revision as its own bolded line.
4. Follow-up only — the revision-tracking list from Step 4, one line per
   prior feedback item (✅ Resolved / ⚠️ Partially addressed / ❌ Still open).
5. Issue count, one line per non-empty category, titles only:
   ```text
   🔴 Blocking: N issues (incl. M privacy): <title>, <title>, ...
   🟡 Minor: N issues: <title>, <title>, ...
   ⚪ Nit: N issues: <title>, <title>, ...
   ❔ Question: N items: <title>, <title>, ...
   ```
   Omit `(incl. M privacy)` when M is 0. Omit a category entirely when its
   count is 0. Omit the Question line for a follow-up.
6. Privacy section, only when M > 0:
   ```text
   🔒 **Privacy: M findings (blocking)**
   (included in the 🔴 count above; see inline comments for detail)
   ```
7. Final decision, its own line: ✅ **Approved** / ✅ **Approved with minor
   comments** / 🔴 **Needs changes before approval**.

## Step 7 — Report

End every run that reached Step 4 with:

```text
Review posted: <verdict>
- Author: @<login>
- PR: #<number> — <title>
- Summary comment: <html_url>
- Formal review (with inline comments): <html_url>
```

For a run that stopped at Step 1–3 without reviewing, state plainly which
skip condition applied and take no further action.

## Error handling

- `gh` call failure (rate limit, permission, network): stop this run, take no
  partial action. State is derived from GitHub, not written locally, so a
  re-delivered or future webhook event re-derives cleanly from scratch.

# 2026-09-14 — LFXV2-2775: the v2 LiteLLM needs the bedrock provider prefix

**Fix** — `llm.DefaultModel` now carries the `bedrock/` provider prefix:
`bedrock/us.anthropic.claude-sonnet-4-20250514-v1:0`.

The old OCI LiteLLM (`litellm.tools.lfx.dev`) routed on the bare inference profile id. The v2
instances require the prefix, and the two disagree in BOTH directions — measured against a live
key rather than inferred:

| model | old `tools.lfx.dev` | v2 `*.v2.cluster` |
|---|---|---|
| `us.anthropic.claude-sonnet-4-20250514-v1:0` | 200 | 400 |
| `bedrock/us.anthropic.claude-sonnet-4-20250514-v1:0` | 401 | 200 |

So the constant is correct for exactly one instance, and every campaign-service environment moved
to v2 in argocd#1546.

**Note** — the failure mode is the part worth remembering, because it does not look like a model
problem. **Auth SUCCEEDS with an unresolvable model.** The key reads as healthy, the service starts
cleanly, and only generation fails — with a bare `400` the client cannot explain, because it
discards the error body on purpose (an error body from a proxy fronting a model can echo the
prompt, which carries operator-supplied campaign context).

That is why the fix took four attempts. A key rotation and a pod restart were both tried first,
and neither could have worked: the key was never wrong. The sequence was

1. rotate the key — no change
2. restart the pods — no change
3. argocd#1546, move to the v2 hosts — **401 → 400**, the first real signal
4. this change — the model name v2 actually resolves

Step 3 is what made the diagnosis possible: 401 means the key was rejected, 400 means it was
accepted and the body was not.

**Note** — set here rather than via `AI_MODEL` in the deployment (argocd#1547, closed). `AI_MODEL`
remains the override for an instance that disagrees again, but the DEFAULT has to be right for the
hosts we run against, or the next environment added inherits the same silent 400.

The `lfx-self-serve` values still point at the old host and therefore still need the BARE id. When
that migration happens, the prefix has to land in the same change that moves the host — the table
above shows why either one alone breaks it.

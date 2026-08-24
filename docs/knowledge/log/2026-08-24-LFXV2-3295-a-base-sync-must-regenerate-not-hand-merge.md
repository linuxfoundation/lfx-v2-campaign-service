# 2026-08-24 — a generated-artifact conflict is resolved by regenerating, not by picking a side

**Fix** — syncing `feat/LFXV2-3295-creative-asset-upload` with `main` (which had just taken
`#163` x-ads account discovery and `#160` client caching) conflicted in nine files. All nine
were generated artifacts:

```
cmd/campaign-service/kodata/gen/http/openapi{,3}.{json,yaml}
gen/http/openapi{,3}.{json,yaml}
gen/http/cli/lfx_v2_campaign_service/cli.go
```

`design/` itself merged cleanly. That is the shape that makes hand-resolution tempting and
wrong: both sides had added a method to the *same* OpenAPI document, so every textual
resolution is a choice between two contracts, and either choice silently drops the other
side's endpoint. Taking `--ours` would have removed `list-twitter-ads-accounts`; taking
`--theirs` would have removed `upload-creative-asset`.

The artifacts are not the source. `design/` is, and it had already merged without conflict —
so the conflict carried no information that the design did not already hold. The resolution is
`make apigen`, which rewrites all nine from the merged design and cannot represent a partial
contract.

## Why the second run matters

One `apigen` produces *a* tree. It does not prove that tree is the one `design/` implies —
a stale committed artifact, or a `goa` version skew, shows up only as a diff on the next run.
So the check is two runs:

```
make apigen   # resolves the conflict
git add -A
make apigen   # must produce NO diff
```

A substantive diff on the second run means the committed generated code disagrees with
`design/`, and the first run's output was an accident rather than a derivation. Here the second
run was empty.

## What to verify after, and why counting is not enough

Regeneration cannot be confirmed by "it built". Both contracts have to be *present*, because
the failure mode this resolution exists to prevent is a silently half-dropped surface:

| claim | check | result |
| --- | --- | --- |
| this branch's endpoint survived | `upload-creative-asset` in `gen/http/openapi3.yaml` | present |
| main's endpoint survived | `list-twitter-ads-accounts` in `gen/http/openapi3.yaml` | present |
| the pre-existing one Copilot flagged as dropped | `apply-keyword-actions` in `service.go` | present |
| no side lost tests | union of both parents' `^func Test` vs the merged tree | 0 missing |

The last row is the one that would have been skipped. A merge that touches no `_test.go` file
still merits the union check, because the question is not "did this merge edit a test" but
"does the merged tree still contain every test either parent had" — and `git grep HEAD` answers
that against the *pre-merge* commit while the merge is uncommitted. Comparing against the
working tree is what actually answers it: 2835 functions, none missing from either parent.

## The false positive this explains

Copilot read the pre-regeneration tree and reported that `apply-keyword-actions` had been
dropped from `gen/lfx_v2_campaign_service_briefs/service.go` while `design/brief.go:1097` still
declared it. The mechanism it described is real — that *is* what a hand-resolved generated
conflict looks like — but the state was transient: it was reading a tree mid-merge. After
regeneration the method is at `service.go:132` and in `MethodNames`. Codegen drift is a claim
about a tree, and the tree it was true of no longer exists.

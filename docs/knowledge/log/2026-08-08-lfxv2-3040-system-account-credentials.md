# 2026-08-08 — LFXV2-3040: system account credentials

## Green gates say nothing about reachability

The first cut compiled, vetted, linted and tested, and could not READ the system
credentials. The second could not WRITE them. Nothing in the gate set asks whether a new
path can ever execute — trace who installs the state and who reads it before believing a
green pipeline.

## Only a genuine absence falls back

A project with no connection dispatches on the LF system account; a project whose
connection is BROKEN must not. A repo error, an empty blob, a decrypt failure each mean a
connection exists and needs attention, and running that campaign on the LF account would
spend LF money on a request the project believed was billed to itself.

The same asymmetry reappears in the installer: only `ErrNotFound` may create, because on
any other read error the row's state is unknown and creating over it overwrites a
credential nobody meant to replace. Absence and uncertainty are different answers, and
conflating them fails in the expensive direction.

## An unreachable value is not an unreachable row

`model.SystemProjectID` contains a colon, which `projectSlugProblem` cannot accept, so no
create endpoint can plant a row there. Necessary, not sufficient: the read/update paths
stay permissive on `project_id` for historical UUID rows, so an existing system row was
rewritable by anyone reaching the connections API. `rejectSystemScope` answers 404, not
403 — confirming something is there is itself a disclosure.

## A choke point only covers what passes through it

The guard was justified by "all six connection endpoints reach storage through the helpers
in `connection_handler.go`". Right about the choke point, wrong about the six:
`ListGoogleAdsAccounts` is a SEVENTH endpoint taking a caller-supplied `project_id` and
the only one calling `orch.ReadAccounts` directly. The number came from the abstraction,
not from the routes. When a guard rests on "every path goes through here", the claim to
verify is the *every*.

Its test needed a working orchestrator to mean anything: without one the call 503s before
the guard is reached and passes against an unguarded implementation.

## An installer is part of the feature, not a deployment detail

The reserved scope being unaddressable over HTTP means no request can *install* it either,
so without an out-of-band installer the fallback never fires and the change ships turned
off. `cmd/sysacct-bootstrap` writes through the same repository and encryptor ports the
HTTP layer uses, so its row is indistinguishable from an API-written one.

## Valid JSON is not the bar — the bar is what the reader matches

The installer validated the document as a non-empty JSON object and encrypted it verbatim.
Stored blobs and dispatch structs are both untagged, so `encoding/json` falls back to a
case-insensitive match that cannot bridge an underscore — and snake_case is exactly what
the API documents. An operator piping in a working set-credential body got a row that
encrypted cleanly, decoded to an all-zero struct, and failed at dispatch as
`credentials_incomplete`, installer exit 0.

Validating a payload's SHAPE says nothing about whether its consumer can read it. Assert
on the decode, not the bytes.

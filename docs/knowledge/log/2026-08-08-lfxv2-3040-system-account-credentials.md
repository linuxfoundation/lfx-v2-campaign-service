# 2026-08-08 — LFXV2-3040: system account credentials

**Green gates say nothing about reachability.** The first cut compiled, vetted, linted and
tested, and could not READ the system credentials. The second could not WRITE them. Nothing
in the gate set asks whether a new path can ever execute.

**Only a genuine absence falls back.** A project with no connection dispatches on the LF
system account; a project whose connection is BROKEN must not — a repo error, an empty blob,
a decrypt failure each mean a connection exists and needs attention, and running that
campaign on the LF account spends LF money on a request the project believed was billed to
itself. Same asymmetry in the installer: only `ErrNotFound` may create, because on any other
read error the row's state is unknown. Absence and uncertainty are different answers.

**An unreachable value is not an unreachable row.** `model.SystemProjectID` contains a colon
that `projectSlugProblem` cannot accept, so no create endpoint can plant a row there.
Necessary, not sufficient: the read/update paths stay permissive on `project_id` for
historical UUID rows. `rejectSystemScope` answers 404, not 403 — confirming something is
there is itself a disclosure.

**A choke point only covers what passes through it.** The guard was justified by "all six
connection endpoints reach storage through the helpers in `connection_handler.go`". Right
about the choke point, wrong about the six: `ListGoogleAdsAccounts` is a SEVENTH endpoint
taking a caller-supplied `project_id` and the only one calling `orch.ReadAccounts` directly.
The number came from the abstraction, not from the routes. Its test also needed a working
orchestrator to mean anything: without one the call 503s before the guard is reached.

**An installer is part of the feature, not a deployment detail** — and so is its artifact.
The reserved scope being unaddressable over HTTP means no request can *install* it either.
The first cut shipped a separate `cmd/sysacct-bootstrap`, which ko never publishes; an
unpublished binary is an unavailable installer, so it became a subcommand of the service.

**Valid JSON is not the bar — the bar is what the reader matches.** Stored blobs and dispatch
structs are both untagged, so `encoding/json` falls back to a case-insensitive match that
cannot bridge an underscore, and snake_case is exactly what the API documents. A working
set-credential body encrypted cleanly, decoded to an all-zero struct and failed at dispatch,
installer exit 0. Assert on the decode, not the bytes. The same reasoning covers the
non-secret half: a row missing the config an adapter refuses to create without (`org_id`,
`page_id`, `funding_instrument_id`) is equally installable and equally dead.

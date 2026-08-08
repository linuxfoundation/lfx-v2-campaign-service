# 2026-08-07 — LFXV2-2023: account discovery cannot yet bootstrap a connection, and the catalog now says so

**Update** — The discovery endpoint's stated purpose is letting an operator choose which ad account
a connection points at. The client layer supports that fully: `ListAccessibleCustomers` runs
without a configured `CustomerID`, which is what `doRequestValidated` exists for. The CONNECTION
lifecycle does not. `GoogleAdsConnectionConfig` declares `Required("account_id")`
(`design/connection.go:333`) and `set-credential-google-ads` needs an existing row, so a caller
must supply a customer ID before it can call the method that discovers customer IDs.

**Fix** — Documented rather than silently widened. Relaxing a `Required` attribute on the
connection config changes the create contract, its generated client, and the UI form that feeds
it — that is a lifecycle change, not part of exposing a read endpoint, and doing it inside this PR
would put a breaking contract change behind a title that promises an endpoint. The api-catalog row
now states the limitation in the terms that matter to a caller: this serves **re-pointing an
existing connection**, not first-time setup.

**Note** — Two review rounds raised the chicken-and-egg at two different layers, and only one of
them was ours to close here. The PLATFORM-client half was real and is fixed (see the
account-discovery dispatch fragment). The LIFECYCLE half is real and is not, and the difference
was invisible while the catalog described the endpoint by its intent instead of its reach. A
capability documented by what it is for, rather than by what it can currently do, reads as
complete.

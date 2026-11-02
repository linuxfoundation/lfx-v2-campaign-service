# 2026-11-02 — Upgrade chi for CVE-2026-114437 (#1248)

**Fix** — Updated the indirect `github.com/go-chi/chi/v5` dependency from
v5.3.2 to v5.4.0 to resolve Dependabot alert #23
(GHSA-j6qf-3m9c-vwj3). Goa's HTTP transport brings in chi, but this service
does not install the vulnerable `ClientIPFromXFF` middleware itself.

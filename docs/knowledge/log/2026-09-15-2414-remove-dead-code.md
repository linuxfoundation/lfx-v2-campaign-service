# 2026-09-15 — #2414: remove dead code and fix lint

**Update** — removed three unused functional-option constructors from platform clients
(`WithAppBaseURL` in `internal/platform/hubspot/client.go`, `WithAdsManagerURL` in
`internal/platform/meta/client.go`, `WithAPIVersion` in
`internal/platform/microsoft/client.go`) that had no callers anywhere in the repo.

The underlying fields (`appBaseURL`, `adsManagerURL`, `apiVersion`) are still used and are
set via `NewClient`; only the option-constructor surface was dead. `deadcode -test ./...`
confirmed no reachable path to any of the three.

Also applied `golangci-lint` fixes in the same PR:
- `QF1012`: replaced `WriteString(fmt.Sprintf(...))` with `fmt.Fprintf` in
  `internal/okf/frontmatter.go` (×4) and `internal/okfgen/index.go` (×1)
- `SA1019`: migrated two test files from the deprecated `parser.ParseDir` (Go 1.25) to
  `os.ReadDir` + `parser.ParseFile` loops; also replaced `doc.New` (deprecated Go 1.22)
  with `doc.NewFromFiles` in `internal/domain/model/campaign_test.go`

**Note** — `WithAPIVersion` removal required updating the Microsoft platform concept
(`docs/knowledge/code/internal-platform-microsoft.md`): the doc previously described
the option; it now reflects that `apiVersion` is fixed at the `v13` constant set during
`NewClient`.

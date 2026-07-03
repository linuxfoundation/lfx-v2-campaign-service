# LFX V2 Campaign Service

A collection service endpoints to support Marketing Operations campaign creation and management.

## API Endpoints

- `/livez`: `GET` — checks that the service is alive (liveness probe). Returns `200` with a
  `text/plain` body of `OK`.
- `/readyz`: `GET` — checks that the service is able to take inbound requests (readiness probe).
  Returns `200` with a `text/plain` body of `OK` when ready, or `503` when not ready.

Both endpoints are unauthenticated and are excluded from the generated public API documentation.

## Development

Common workflow targets (see the `Makefile` for the full list):

```sh
make apigen        # generate API code from design/ (required before first build)
make fmt           # format Go code (gofmt + simplify)
make check-fmt     # verify formatting (used in CI)
make lint          # run golangci-lint
make test          # run tests with race detector and coverage
make build         # build a local binary
make build-release # build a static release binary for Linux
make run           # build and run locally
```

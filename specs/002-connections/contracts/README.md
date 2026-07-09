# Contracts — Platform Connections

The authoritative contract is the **generated OpenAPI** produced by `make apigen`:

- `gen/http/openapi3.yaml` (and `.json`)
- served at `/_campaigns/openapi3.yaml`

The Goa design source is `design/connection.go`. Do not hand-edit the generated
files; change the DSL and re-run `make apigen`.

Endpoint summary (per provider `{provider}` ∈ `google-ads`, `linkedin-ads`,
`meta-ads`, `reddit-ads`, `twitter-ads`, `microsoft-ads`, `hubspot`):

| Method | Path | Notes |
|--------|------|-------|
| POST   | `/projects/{projectId}/connection-{provider}` | Create (409 if exists) |
| GET    | `/projects/{projectId}/connection-{provider}` | Read (credentials redacted, ETag) |
| PUT    | `/projects/{projectId}/connection-{provider}` | Replace config (If-Match) |
| DELETE | `/projects/{projectId}/connection-{provider}` | Soft delete |
| POST   | `/projects/{projectId}/connection-{provider}/test` | Verify credential upstream |
| POST   | `/projects/{projectId}/connection-{provider}/set-credential` | Replace stored credential |

All routes require the Heimdall JWT and are gated on `campaign_manager` at the gateway.

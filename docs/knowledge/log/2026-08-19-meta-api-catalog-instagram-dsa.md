# 2026-08-19 — Meta: document instagramUserId/dsaBeneficiary/dsaPayor in the API catalog

**Docs** — The publishability fix
([`2026-08-19-meta-instagram-dsa-publishability.md`](2026-08-19-meta-instagram-dsa-publishability.md))
added three `metaConfig` fields but did not describe them in
[`docs/api-catalog.md`](../../api-catalog.md), the effective consumer-facing
validation contract for the `Any`-typed `CreateCampaigns.config` object. Since a
mismatch there surfaces to callers only as a `202` followed by a dead job — never a
synchronous 4xx — the catalog is the one place a caller learns what the field does
before sending it.

The `#### MetaConfig` section now documents `instagramUserId`, `dsaBeneficiary`,
and `dsaPayor` in the same column-aligned style as `pixelId` and `placements`. The
descriptions are explicit on the point the code enforces and the point it does NOT:
Meta requires these to PUBLISH (Instagram identity for any Instagram placement;
both DSA disclosures for regulated-location targeting), but this service does **not**
validate them at create time — a missing value is trimmed-to-absent and simply not
sent, so the gap manifests as an async publish block on Meta rather than a create
rejection. That non-enforcement is called out per field so a reader does not expect
a synchronous error the guard never raises.

No code or behavior changed; this is a documentation-contract entry only.

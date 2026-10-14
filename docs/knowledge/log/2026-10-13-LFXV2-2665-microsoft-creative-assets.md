# 2026-10-13 — Microsoft Advertising campaigns can carry a stored creative image

**Update** — a Microsoft dispatch can now name a stored creative asset
(`microsoftConfig.imageAssetId`, optional `imageAltText`). The image is attached to the created
Search campaign as a campaign-level Image ad extension, the only image surface that campaign
type has. Until now only Meta consumed stored assets.

- Before anything is created, the asset is resolved within the brief, refused on its
  `byte_size` above Microsoft's 76,800-byte ceiling (the 102,400-character base64 limit),
  charged to the `AssetReserver` that Meta and Microsoft now share, loaded, and checked for
  PNG/JPEG, 40x20..2592x2048 and alt text. Every failure releases the claim.
- After a confirmed create: an association lookup (any existing image extension means nothing
  is uploaded), then AddMedia, AddAdExtensions and SetAdExtensionsAssociations. The ids are
  recorded in the result as `imageMediaId`/`imageAdExtensionId`. No migration.
- A failed attach leaves the campaign `created_degraded` with a step reading "NOT attached" or
  "UNCONFIRMED". Upstream message text is never recorded.
- Docs: `upload-creative-asset` description, `docs/api-catalog.md`, the new
  [Microsoft Creative Image Assets](../architecture/microsoft-creative-assets.md) concept, and
  `internal/platform/microsoft`.

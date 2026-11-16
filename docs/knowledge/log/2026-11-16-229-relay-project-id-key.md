# 2026-11-16 — Index relay logs the project under `project_id`

**Fix** — `internal/infrastructure/indexer/relay.go` logged an unresolved project slug under `project_slug`. #596 added it and #539 (issue #229) then pinned the key repo-wide as `project_id`, so `TestProjectIDLogKeyIsUniform` failed on main. The key is now `project_id`, whose value is a project UUID or slug per [logging conventions](../architecture/logging-conventions.md).

# 2026-08-21 — LinkedIn job functions and seniority exclusions are now per-profile

**Update** — `jobFunctions` and `seniorityExclusions` were package-level constants in
`internal/platform/linkedin/config.go`, applied identically to every campaign regardless of
`TargetingProfile`. `buildTargetingCriteria` injected them straight into the wire request, and
`validatePrerequisites` counted them toward every profile's "is this criteria non-empty" check.
A cloud-native audience and a sales-leaders audience got the same job-function/seniority facets
whether that was the intended targeting or not — the only knobs a runtime config could turn per
profile were `Skills` and `Groups`.

`TargetingProfileConfig` gained two optional fields, `JobFunctions` and `SeniorityExclusions`,
following the same shape as `Skills`/`Groups`. `effectiveJobFunctions`/
`effectiveSeniorityExclusions` (`targeting.go`) resolve a profile's own values when it sets them,
falling back to the renamed `defaultJobFunctions`/`defaultSeniorityExclusions` otherwise — so an
existing runtime config with no opinion on either field keeps behaving exactly as before. Both
facet kinds now run through `validFacets` (new `job-functions`/`seniority-exclusions` namespace
entries: `urn:li:function:`/`urn:li:seniority:`), so a malformed override is rejected before any
LinkedIn resource is created, matching the existing skills/groups/employer-exclusions validation.

`validatePrerequisites`'s non-empty-criteria check now counts a profile's resolved
`effectiveJobFunctions(p)` rather than the old global directly — no behavior change for a profile
that doesn't override it, since the resolved value IS the old global in that case.

Two existing tests mutated the package-level `jobFunctions` var directly to simulate a
truly-empty assembled-criteria scenario; updated to mutate `defaultJobFunctions` (the renamed
var) instead, preserving the same test intent.

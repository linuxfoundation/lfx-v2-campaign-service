# 2026-11-07 — #563 Microsoft caller ad copy and display paths

**Update** — `microsoftConfig.headlines`, `descriptions`, `path1` and `path2` (gap 1 of the
Microsoft Ads gap review, #563) are sent on the responsive search ad instead of the copy
auto-composed from the event name. An LF event's Microsoft campaign mirrors its Google Search
campaign, and the Google copy is the approved copy; the auto-composed copy was placeholder quality.

- Rules live once in `microsoft.ValidateCallerAdCopy`: 3–15 headlines (30 characters, 15 with a
  double-width character), 2–4 descriptions (90 / 45), every entry word-bearing and control-free,
  no case-insensitive repeats, and no blank entry (refused, never padded). A path is one segment of
  at most 15 characters (7 wide), without `/`, and `path2` requires `path1`.
- The client measures the host plus `/path1/path2` against the 67-character display-URL limit
  (33 wide) before any request; it needs the brief's URL, so it is a dispatch-time refusal with
  nothing created.
- Synchronous 400 (`invalid campaign config: microsoftConfig.<field>: ...`) from
  `MicrosoftDispatcher.PreflightCreate` for a rule breach or a wrong JSON type, case-variant keys
  included. Dispatch re-checks before any Microsoft call, with the type check ahead of the decode
  so a wrong type is the same refusal rather than a decode error.
- A reused ad keeps its copy; the result records a warning step.
- The snapshot stores the copy through `sanitizeSnapshotText`, like every other caller text.
- Default-off deployment flag `MICROSOFT_AD_COPY_ENABLED`; the chart sets `"false"`. Not
  role-gated: copy names no account resource.

Not verified live: no Microsoft call was made. The shape is from the v13 `ResponsiveSearchAd`
reference. Turn the flag on only after a live create confirms Microsoft accepts caller copy and
`Path1`/`Path2` on the ad.

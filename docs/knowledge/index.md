# Knowledge Base

* [Architecture](architecture/index.md) - Architecture, API catalog, and data schema docs for the campaign service.
* [Kubernetes](kubernetes/index.md) - Kubernetes resources defined in the Helm chart.
* [Code](code/index.md) - Go package structure of the service.
* [Specs](specs/index.md) - Feature specs tracked via speckit.
* [Log](log/) - Dated change log; one file per entry, so concurrent PRs never
  edit the same log file.

**OKF deviation:** OKF v0.1 §7 reserves a single `log.md` newest-first
changelog. This bundle instead keeps `docs/knowledge/log/`, one dated fragment
per entry (`YYYY-MM-DD-<slug>.md`), because a single append-at-top file was the
dominant source of merge conflicts between concurrent PRs. Each fragment still
carries an ISO date in its name, so a conformant `log.md` can be reconstructed by
normalizing each fragment's H1 into an `## YYYY-MM-DD` heading any time an
external OKF consumer needs one.

`okfvalidate` holds a fragment to its filename, its H1 date, and — since #176 —
the bold kind marker the entry opens with, which must be one of CLAUDE.md's six
(`Update`, `Fix`, `Creation`, `Note`, `Verification`, `Docs`). The set is closed
because the marker is what classifies an entry for a reader scanning the
directory, so a one-off spelling is not a richer label but an entry that sorts
under nothing. Two shapes are not a plain single-entry fragment and are checked
accordingly: an entry later superseded carries a `> **Superseded …**` blockquote
ABOVE its marker, so the reader meets the warning first; and an ARCHIVE rolls
many entries under one H1, each behind its own `##` heading with its own marker,
which is checked per section. The body is read with a CommonMark parser
(`github.com/yuin/goldmark`), not a line scanner: a section is a level-2 ATX or
Setext heading that is a direct child of the document, so a `##` line inside
fenced or indented code, an HTML block, a list or a blockquote is never one, and
an entry's marker is the bold label opening its first paragraph. In an archive,
a top-level fence left open to the end of the file is an error, because it would
hide every section after it (#421); a single-entry fragment has no later
sections to hide, so it is not checked for one. The archive is recognized by that structure rather than by
filename, so there is no naming convention to keep in sync. The validator's set and CLAUDE.md's are held equal by a test that reads
CLAUDE.md (#420), so a kind cannot be added to one and not the other.

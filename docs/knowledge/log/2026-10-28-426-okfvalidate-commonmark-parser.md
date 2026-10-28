# 2026-10-28 — okfvalidate: read log fragments with a CommonMark parser

**Fix** — `okfvalidate` now finds archive sections and kind markers on a goldmark
(`github.com/yuin/goldmark`, CommonMark-compliant, MIT) AST instead of a hand-rolled
fence and section scanner (#426, following #420 and #421).

Every review round on #426 found another CommonMark corner the scanner got wrong:
blockquote depth, list-marker lookalikes, indented headings and backticks in info
strings, then three more at once. A fence opened as ``10. ```text`` closes on its
content-indented closer. A line of backticks inside an HTML comment or `<pre>` is not a
delimiter. A `>` followed by a tab is a valid blockquote marker. Each fix made the
scanner a closer copy of the spec, and the next review found the next gap. The parser
decides block structure exactly as a renderer does, so the class is closed rather than
patched one more time.

What the validator now reads:

- A section is a level-2 heading, ATX or Setext, that is a direct child of the document
  and has a non-empty title. A `##` line in fenced or indented code, an HTML block, a
  list or a blockquote is never a direct child, so it is never a section.
- The marker is the bold label at the start of the first paragraph after the heading,
  past any superseded banner. If that first block is not a paragraph, it is reported as
  "not a bold kind marker". If the next block is another section, the entry is reported
  as having no body.
- CommonMark closes an unclosed fence at the end of the document, which would hide every
  later section, so that is still an error. goldmark does not record whether a fence was
  closed. The validator reads it off the source instead: only a fence that ran to the end
  of the document has nothing but whitespace after its last content line. A fence ended
  by its container (a blockquote or list item that stops) hides nothing past that
  container and is not reported.
- An H1 indented four spaces is code, not a heading, and is now reported as such.

The helpers that implemented the scanner (`fenceState`, `fenceMarker`,
`fenceContainerPrefix`, `normaliseContainer`, `hasListMarker`, `leadingSpaces`,
`nextMarkerCandidate`) are deleted. Every earlier regression test passes unchanged.
Three new ones cover the open threads: a list fence closing on its indented closer,
HTML-block fence lookalikes around a real section with a bad marker, and a `>`+tab
closer. Each was run against the previous scanner first and failed there. goldmark is
used only by this lint tool, not by the service runtime.

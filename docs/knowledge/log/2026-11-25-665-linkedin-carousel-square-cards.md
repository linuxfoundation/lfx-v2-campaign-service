# 2026-11-25 — #665 LinkedIn carousel card images must be square

**Update** — Decision 2026-11-25 (Misha): a LinkedIn carousel card image must be exactly 1:1.
LinkedIn recommends 1080x1080 at 1:1 and scales every card to 312x312, with no documented
tolerance. `PreflightCreate` answers a synchronous 400 when a card asset's recorded width and
height are not equal (a bytes-free metadata read scoped to the brief); Dispatch and the client
refuse it from the image header before any LinkedIn request, which also covers assets stored
without dimensions. The 45-character headline limit stays, lead gen carousels stay refused.

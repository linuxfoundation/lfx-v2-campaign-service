# 2026-09-29 — A2: the provider concept pages still described the ported quirks

**Docs** — no behaviour change. Raised by the second pre-PR local review round over the whole
A2 range.

The account-monitor architecture concept and `docs/api-catalog.md` were updated as each fix
landed, but the three **provider** pages under `docs/knowledge/code/` carry their own
account-monitor sections, and those still told a reader the quirks were deliberately preserved:

- `internal-platform-googleads.md` — "its preserved local 50/90/100 pacing literals rather
  than the shared `Thresholds`".
- `internal-platform-linkedin.md` — "including the deliberately preserved `MED`/`MEDIUM`
  sort-map bug".
- `internal-platform-reddit.md` — "carries over the BFF's hardcoded `conversions: 0` and its
  underspend threshold/label mismatch … verbatim, plus the totals-from-a-separate-call quirk",
  and an opening sentence still naming an account-level totals call that no longer exists.

Each section now describes what the platform actually does: the shared `pacingLabelFor` and
`priorityRank`, Reddit's absent conversions and row-summed totals with the `AccountTotalsReader`
plumbing gone, Google's corrected `zz` filter and named CTR constants, and LinkedIn's low-CTR
rule no longer exempting a 0% CTR.

## Why this was missed twice

A defect spanning one package updates that package's concept file and the architecture concept
that frames it. These fixes each spanned *one platform's rule engine plus the shared service
layer*, and the provider page sits on neither axis — it describes the platform adapter, but its
account-monitor section summarises the rule engine for a reader who starts there. Worth
checking `docs/knowledge/code/internal-platform-*.md` whenever a rule-engine or monitor-service
behaviour changes, not only the architecture page.

# 2026-08-21 — `NULL` provenance is an absence of knowledge, not an age

**Fix** — ten sites across `internal/`, the `000027` migration comment and three docs
described `campaigns.ran_on_system_account IS NULL` as meaning "this row predates migration
000027". That reading is wrong, and it is wrong in a way that a reader cannot detect from any
one site: every one of the ten was internally coherent, and they agreed with each other.

`AdoptCampaign` is the counterexample, and it is deliberate. `adoptCampaignQuery` omits the
column from its INSERT column list entirely, so an adopted row takes the column's (absent)
default and reads back `NULL`. That omission is the correct answer rather than a gap:
adoption binds a campaign that ALREADY EXISTS upstream, created outside this service's
dispatch path — possibly by hand in the platform's own UI — so which credential paid for it is
genuinely not known here, and stamping the adopting caller's current connection would assert
a fact about spend that happened on an account this service never observed. A campaign adopted
TODAY is therefore `NULL`. Under the age reading, a reader dates it to before a migration that
had already shipped.

The general shape: `NULL` is reached by BOTH the pre-migration rows and every write that
cannot know the answer. Those are different populations, and only the second one grows. A
predicate written against the age reading — "NULL means legacy, so the backlog is bounded and
shrinking" — is false about a set that gains a row on every adoption.

**The wording that replaced it**, used verbatim at all ten sites so it cannot drift back into
ten paraphrases: `NULL` = *provenance not recorded*, `false` = *known to have run on the
project's own connection*, `true` = *known to have run on the LF system account*.

**A second, related correction.** Two sites justified the deferred `stampProvenance` by saying
an unstamped row is "indistinguishable from a campaign that genuinely ran on the project's own
account". It is not: project-owned is an explicit `false`, unstamped is `NULL`, and the two are
distinguishable by any consumer. The real consequence is quieter and worse, so it is what the
comments now say — an unstamped row is `NULL`, so it drops OUT of system-account attribution
and credential blast-radius reporting altogether. It is **uncounted, not miscounted**, and
nothing downstream flags the gap. Stating a weaker, false harm in place of the true one costs a
reader the reason the `defer` exists.

**Scope note.** The `000027` up-migration is APPLIED, so only its comment was rewritten; the
DDL (`ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS ran_on_system_account BOOLEAN;`) is
byte-identical, since a changed applied migration would not re-run on any deployed database.

The reviewer named nine sites. A grep of the whole branch for the age framing found a tenth —
an assertion message in `system_account_provenance_e2e_test.go` calling `NULL` "a legitimate
pre-migration row" — plus two of my own earlier log fragments still describing the column as a
bare omission from `DO UPDATE`, which the write-once `IS NULL` guard had already replaced.
Fixing the cited list alone would have left the contradiction standing in three places.

# 2026-10-16 — Attach combines suppressions into one list; kernel contact suppressions added

**Update** — an attached audience (Use send lists / Use master list / Use selection directly)
recorded every ticked suppression separately, so the HubSpot email carried several exclude lists,
while compose already folded them into one Combined Suppression list.

- `AttachExisting` now folds two or more suppressions into one combined suppression list (the
  union of them), named per selection with `SelectionFingerprint` and reused on repeat, and records
  only that list. One suppression is still attached as is.
- The attach result carries the combined list as `suppression` (design + regenerated specs).
- `StandardSuppressionTerms` gains the kernel contact lists: `Kernel Domain Contacts`,
  `Persona Linux Kernel`, `LF kernel.org Contacts`.
- Tests: `TestAttachExisting_CombinesSeveralSuppressionsIntoOneList`,
  `TestAttachExisting_SameSuppressionsAgain_ReusesTheCombinedList`,
  `TestAttachExisting_CombinedCreateFails_RecordsNothing`,
  `TestAttachExisting_LongMasterName_KeepsTheCombinedNameSearchable`,
  `TestAttachExistingAudienceRecordsTheCombinedSuppressionAlone`, and kernel cases in
  `TestMatchesStandardSuppression_NormalisesSeparatorsQuartersAndSuffix`.

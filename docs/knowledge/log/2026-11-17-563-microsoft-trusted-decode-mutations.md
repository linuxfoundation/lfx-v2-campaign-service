# 2026-11-17 — #563 Microsoft mutation and read responses refuse duplicated keys

**Fix** — A #617 review finding raised by Copilot. `keywordMutation` (`keyword_levers.go`) still decoded with `json.Unmarshal`, so a pause or removal answered `{"PartialErrors":[{"Index":0,"Code":1234,"Code":0}]}` (or with a case variant `"code":0`) decoded last-key-wins to the zero-code placeholder. The rejected keyword was reported APPLIED, and a combined request went on to send the irreversible REMOVE batch.

`keywordMutation` and the `AddNegativeKeywordsToEntities` decode now go through `decodeTrusted`. A refusal takes the existing unreadable-response path, `keywordMutationUnconfirmedError`: the call is UNCONFIRMED, and a refused PAUSE call stops the REMOVE batch (its items are FAILED with `NOT_SENT`).

The same change was applied to every other remaining Campaign Management decode in `internal/platform/microsoft/` that feeds PartialErrors classification or an entity identity, so this class of finding does not come back one file at a time. Each refusal maps to the path that site already uses for an unreadable body:

- `firstCampaignID` (`campaign.go`), the ad-group and responsive-search-ad creates (`adgroup_ad.go`), and `createKeywords` (`targeting.go`): `errNoID`, which means UNCONFIRMED. Without the check, `"Code":null,"Code":1100` read as a definite rejection of an entity that may exist.
- `putUpdateOutcome` (`campaign.go`): `transportError`, which is UNCONFIRMED.
- `addAudienceCriteria` (`audiences.go`): every list in the call UNCONFIRMED. `CheckRemarketingLists`: `errAudienceLookupMalformed`.
- `existingLocationIDs` (`geo.go`) and `queryCampaignByIDGuarded` (`budget.go`): the read fails closed with its existing decode error.

Not changed: the Customer Management account discovery (`accounts.go`), the geo file URL, reporting, and the token response. None of them feeds PartialErrors classification or a Campaign Management entity identity. The streaming lookups (`lookupNamedEntity`, `lookupAdByFinalURL`, `decodeAccountCampaigns`) are token decoders, not `json.Unmarshal`. A slice field with its own `UnmarshalJSON` (`boundedNumberIDs`, `boundedErrorItems`) appends when its key repeats, so a duplicated id array alone already failed closed. The risk was a key repeated inside an error entry.

Covered by the new `TestApplyKeywordActions_DuplicateCodeIsUnconfirmed`, `TestCampaignMutationResponses_DuplicateKeyIsUnconfirmed`, `TestCreateCampaign_DuplicateKeyCreateBodiesAreUnconfirmed`, `TestCreateCampaign_KeywordDuplicateCodeIsUnconfirmed`, `TestExistingLocationIDs_DuplicateKeyRefuses` and `TestGetCampaignBudget_DuplicateKeyRefuses`, and by new cases in `TestAddCampaignNegativeKeywords_AmbiguityIsUnconfirmed`, `TestCheckRemarketingLists_Refusals` and `TestAttachAdGroupAudiences_CallFailures`. Each one fails with the plain decode restored.

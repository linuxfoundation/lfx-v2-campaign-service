# 2026-11-06 — #519 Demand Gen Audience: a dimension that names everyone is left out

**Fix** — the #519 real create (account 8666746580, v23, campaign-service's own
`CreateDemandGenCampaign`) sent the demographic Audience with a parental-status dimension
naming PARENT, NOT_A_PARENT and undetermined. Google created it
(`customers/8666746580/audiences/363763755`) and the GAQL read-back held the age, gender,
income and segments dimensions but **no parental-status dimension**: a dimension that names
every value and undetermined restricts nobody, and Google drops it. A second Audience with all
four dimensions full plus one user list (`audiences/364652404`) read back as the user list
alone, so the rule holds for age, gender, income and parental status alike.

The consequence was a dead-end retry. A retry of the same create hits NAME_ALREADY_IN_USE and
reads the Audience back to reuse it; `sameAudienceDimensions` then saw four dimensions against
five planned and refused it ("holds dimensions that differ"). Re-running the #519 create
reproduced exactly that refusal before any budget was sent.

- `validateDemandGenDemographics` now leaves such a dimension out of the request
  (`coversEveryone`), so the planned dimensions are the ones Google stores, and says so in the
  step summary ("every parental status (no restriction, left out)").
- An object whose every set dimension names everyone, with no user list or custom audience, is
  refused before anything upstream ("restricts nobody").
- Every value WITHOUT undetermined is still a restriction and is still sent; Google kept such a
  gender dimension (MALE + FEMALE) on the #519 create.
- Only plans that named an everyone-dimension change their name hash; the test fixture's
  genders changed from MALE + FEMALE + undetermined (now left out) to FEMALE + undetermined,
  so its pinned name moved.

Test: `TestValidateDemandGenDemographics_EveryoneDimensionMatchesGooglesReadBack` compares the
#519 plan with Google's verbatim REST read-back; it and
`TestValidateDemandGenDemographics_EveryoneDimensionPerKind` fail without the fix.

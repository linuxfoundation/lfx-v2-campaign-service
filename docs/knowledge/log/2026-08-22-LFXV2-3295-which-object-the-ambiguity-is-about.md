# 2026-08-22 — ambiguity is about an object, not about a request

**Fix** — two defects on the Meta creative-dispatch path, both at the `/adimages` upload, and
both about treating the upload as though it were the ad create.

## An upload failure was reported as a possible ad

`createVariantAd` uploads the image before creating the creative and the ad. A 5xx or transport
failure on that upload returned a bare `transportError`, which the caller fed to
`createOutcomeAmbiguous` — and the step it wrote said the ad/creative "may have been created —
verify in Meta Ads Manager before recreating".

At that moment no adcreative and no ad request has been sent. The operator is sent to look for
an object that certainly does not exist, and told not to recreate it — which on this path is the
one instruction that guarantees the variant never runs.

`createOutcomeAmbiguous` was not wrong; it was asked the wrong question. It answers "may this
REQUEST have been applied?", and for the upload the answer is genuinely yes. But the object it
may have applied to is a library IMAGE, and the upload is content-addressed and idempotent, so a
landed image needs no cleanup and a re-dispatch re-derives the same hash. The ad and the creative
are DEFINITELY absent. Ambiguity has to be predicated on an object before it can be reported.

`uploadStageError` now marks the stage, the caller checks it before the generic wording, and the
step names the upload as the failure while noting that the image may have landed harmlessly.

## The upload's error carried the caller's image bytes

The non-2xx branch fell back to a redacted, truncated snippet of the response body when the
Graph envelope did not parse — mirroring `do()`. That mirror is what made it wrong here.

`do()` sends JSON. This request is MULTIPART with the image as its first part, so a proxy
reflecting the request puts image bytes inside the first 300 characters, ahead of anything
resembling Graph JSON. `redactSecrets` removes the Bearer token and known credentials; it has no
way to recognise image bytes and passed them through. The `APIError` is copied into
`CampaignResult.Steps` and persisted.

Verified by probe before fixing rather than reasoned about: a test server echoing the request
body produced an error containing the marker bytes and the `Content-Disposition` header verbatim.
The body is now withheld on this path and `EnvelopeUnreadable` is set, which keeps the outcome
classified as ambiguous — withholding the DETAIL must not turn an unknown outcome into a clean
rejection, because a 400 with no Code is how Meta most often reports a throttle.

## On the review that found them

The second finding was reported as contradicting "the stated guarantee that image bytes never
enter an error". No such guarantee exists anywhere in the branch — the claim was checked before
being acted on. The finding was real anyway, and the probe is what established that, not the
citation. A conclusion can be right while the proof offered for it is not, and the two have to be
checked separately.

Pinned by `TestCreateCampaignUploadStageFailureDoesNotClaimTheAdMayExist` (which asserts the run
actually stopped at the upload — zero creative and ad requests — before asserting anything about
the wording, so a fixture that failed later could not pass it vacuously) and
`TestUploadImageErrorNeverCarriesTheRequestBody`. Reverting either fix fails its test.

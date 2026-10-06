# 2026-10-06 — LFXV2-2665 The over-refusal cluster from the local pre-PR review

**Fix** — Three findings that share one shape: a guard whose bound or key was chosen for
something other than what upstream actually constrains. Over-refusal is the failure mode
this package's preflight is not allowed to have, so these are the findings that matter most
even though none of them is a crash.

**A payload sanity bound must be set where only nonsense reaches it.** `maxLeadFormFields`
was 12 — roughly where a human stops wanting to fill in a form, and nowhere near where
Google stops accepting one. A thirteen-field brief Google would have taken was refused
locally for no upstream reason. It is now 64. The real ceiling was always structural and
upstream: fields are de-duplicated on the input type, so a form cannot hold more distinct
fields than `LeadFormFieldUserInputType` has members. What is left for the constant to
catch is the single case de-duplication cannot — thousands of DISTINCT shape-matching
strings, which `enumShapeRE` admits because it anchors the shape and not the vocabulary.
The test now composes its expected substring from the constant; it had the old bound
spelled out as a literal, which would have kept asserting 12 after the constant moved.

**The constraint was the bidding STRATEGY; the guard was keyed on the channel.** Device bid
modifiers were refused on Performance Max and nowhere else, which reads as though automated
bidding were a Performance Max property. It is not: Video and Display both default to
`maximizeConversions`, and a Search campaign can be switched to one. On all three, Google
stores a device adjustment and never bids it — the operator gets no adjustment and no
signal, which is the forgiving-upstream trap the adjacent `CPCBid` arm already names. The
refusal now lives in `validateBiddingPlan`, keyed on the strategy.

The half of that fix worth remembering is what it does NOT refuse. A modifier of exactly
`0` is the -100% opt-out, and a device EXCLUSION is honoured under automated bidding
exactly as under manual. A flat "no device modifiers under automated bidding" check would
have traded an under-refusal for an over-refusal, which is the worse of the two. The test
is written around that distinction on purpose: the flat version passes its refusal half and
fails its exclusion half. The Performance Max arm in `campaign_criteria.go` stays and now
says why it is a different rule — that channel takes no device criteria at all, not even
the exclusion, so it refuses the whole list.

**A name can be a latent over-refusal.** `customerIDRE` is `^[0-9]+$` and length-agnostic,
so it is correct at every call site today — including the ones that are not customer ids at
all: ad group and criterion ids, campaign ids, and the Performance Max asset group id. The
defect is the invitation in the name. "Google customer ids are ten digits, so pin the
length" is a plausible hardening edit that would refuse every asset group id Google issued,
turning a ready-to-serve Performance Max campaign into a local refusal at activation. The
matcher now states the contract where that edit would land, and names the call site it
would break; a genuine customer-id length check belongs in a separate matcher at the
customer-id call sites, never as a narrowing of this one.

**The through-line.** Each bound was defensible against the wrong reference point: a human
reading a form, one channel that happens to bid automatically, the identifier the matcher
was first written for. A guard is only as good as the thing it is keyed on, and the test
that would notice if that key were wrong.

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

// Golden copies of the prompts this service emitted BEFORE stages existed, extracted verbatim
// from commit 012fa822^ (`git show 012fa822^:internal/service/email_copy.go`). LFXV2-1940
// requires a caller that sends no stage to keep producing exactly these bytes.
//
// These live here, beside the test, rather than in the production file, so the assertion never
// reads back the same constant it is checking -- a test compared against `legacySystemPrompt`
// would pass no matter how far that constant drifted from the pre-stage text.
//
// They are a .go file rather than testdata/*.txt only because the repo's License Header Check
// scans *.txt, and a header prepended to a golden file would corrupt the bytes it exists to pin.
//
// DO NOT EDIT to make a failing test pass. A diff here means the absent-stage path changed, which
// is the thing LFXV2-1940 forbids.

const goldenLegacySystemPrompt = `You are an expert email copywriter for technology events and communities.
Your task is to generate compelling email copy for a campaign brief.

IMPORTANT: Use ONLY the event details provided below; never invent dates, names, or locations.
Every factual claim must come directly from what you're given.

Generate JSON with these fields (no markdown fencing):
{
  "subject": "Email subject line (max 60 chars)",
  "preheader": "Email preheader text (max 100 chars)",
  "body": "Email body in HTML (max 8000 chars, include <p> tags)",
  "cta": "Call-to-action button text (max 50 chars)"
}

Constraints:
- Subject: punchy, under 60 characters
- Preheader: summary of the email, under 100 characters
- Body: professional HTML email, inviting and focused on the event
- CTA: action-oriented, under 50 characters (e.g. "Register Now", "Join Us")
- Write for a professional Linux Foundation / technology audience
- Make it about the event and community, not promotional`

const goldenLegacyUserPromptTemplate = `Generate email copy for this event:
Event Name: %s
Location: %s
Dates: %s

Create compelling email copy that invites registration and highlights the value of attending.`

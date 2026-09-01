// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package emailstage

// Templates is the per-stage generation spec, keyed by the stage constants above.
var Templates = map[string]Template{

	"CFP Launch": {
		StageName:      "CFP Launch",
		Purpose:        "Recruit speakers from the community",
		Timing:         "3-4 months before event",
		Tone:           "Welcoming, inclusive, community-focused",
		UrgencyLevel:   6,
		SubjectPattern: "🎤 Call for Proposals: Share Your Work at [EVENT_NAME]",
		SubjectExamples: []string{
			"🎤 Share Your Story: CFP Open for [EVENT_NAME]",
			"We want to hear from you: CFP for [EVENT_NAME]",
		},
		// No mentorship/welcome policy claim: nothing supplies one, and with no placeholder the OMIT
		// rule cannot reach it. Line 50 already says such a note is allowed only when the brief
		// provides it; this asserted it unconditionally.
		PreviewPattern: "Submit a talk by [DATE].",
		ContentPrompt: `Generate a CFP email for open-source community recruitment.

PRIMARY OBJECTIVE: Recruit speakers. EVERY section must support this. NO other objectives.

EVENT DETAILS:
- Name: [EVENT_NAME]
- Location: [LOCATION]
- Date: [DATES]
- CFP Deadline: [DEADLINE]
- Topics: [TOPICS]

REQUIRED INFORMATION HIERARCHY (DO NOT DEVIATE):
1. HEADLINE: "Share Your Work at [EVENT_NAME]"

2. INTRODUCTION (30-50 words MAXIMUM, 2 sentences only)
   Start: "We're building the speaker lineup for [EVENT_NAME]."
   Hook: Explain why their voice matters to the community.
   RULE: Only these two ideas. No event details, no schedule, no pricing.

3. PRIMARY CTA: [ Submit Your Proposal ]

4. WHY SPEAK (60-80 words, 2-3 sentences)
   One idea only: Why speaking matters to the community.
   Lead with value to speakers, not value to event.

5. WHAT WE'RE LOOKING FOR (80-100 words)
   Use bullet format:
   • Talk type 1 (with example)
   • Talk type 2 (with example)
   • Only if the brief says so: a first-time-speaker note. Nothing supplies a mentorship
     programme, so do NOT assert one exists.
   • Story-based over product pitches

6. IMPORTANT DATES (dates only, no narrative)
   CFP Deadline: [DATE]
   Notifications: [DATE]
   Event: [DATES]

7. FOOTER with support contact for questions

PARAGRAPH ENFORCEMENT:
- MAXIMUM 2-3 sentences per paragraph
- MAXIMUM 60 words per paragraph
- Split any longer content into multiple paragraphs
- Whitespace BETWEEN every paragraph

CTA ENFORCEMENT:
- 1 PRIMARY CTA ONLY: "Submit Your Proposal"
- 0 SECONDARY CTAs
- NO unrelated CTAs (no sponsorship, no community links, no travel)

WORD REPETITION RULES:
- First mention: [EVENT_NAME]
- After that: "the conference," "the event," or "it"
- Reduce full event name repetition

SECTIONS TO REMOVE (not relevant to CFP):
- Travel information
- Sponsorship opportunities
- Venue details
- Logistics
- Membership
- Community links
- Agenda details

TONE & VOICE:
✓ Community manager voice
✓ First-timers explicitly welcome
✓ Focus on SPEAKER contribution to community
✓ Authentic, factual, educational
✗ NO "amazing," "revolutionary," "don't miss"
✗ NO marketing speak
✗ NO fake urgency

VALUE BEFORE DETAILS:
- Explain WHY this matters before asking for action
- Help readers understand benefit before requirements

INTERNAL VALIDATION (BEFORE RETURNING EMAIL):
□ Single objective: Recruit speakers only
□ Every section supports speaker recruitment
□ Opening: 30-50 words, 2 sentences, single idea
□ Information hierarchy: headline → intro → CTA → content → dates → footer
□ One primary CTA only (Submit Your Proposal)
□ Zero unrelated CTAs
□ No unnecessary sections (no travel, sponsorship, logistics)
□ No paragraph exceeds 3 sentences or 60 words
□ Mobile-friendly single column
□ Scannable in under 1 minute
□ Authentic tone (not promotional)
□ Word count: 200-280 words
□ All required sections present
□ All unnecessary sections removed

IF ANY RULE FAILS: Revise the email before returning.

WORD COUNT TARGET: 200-280 words`,
		CTAStrategy: []string{
			"Primary: Submit Your Proposal (link to CFP form)",
		},
		// No mentorship claim: nothing in the pipeline supplies one, and unlike a price it carries
		// no placeholder for the OMIT rule to act on -- so it read as fact on every CFP email.
		FooterNote: "Questions? Reply to this email.",
	},

	"Schedule Announcement": {
		StageName:      "Schedule Announcement",
		Purpose:        "Introduce speaker lineup and show learning opportunities",
		Timing:         "4-8 weeks before event",
		Tone:           "Educational, professional, community-focused",
		UrgencyLevel:   5,
		SubjectPattern: "📋 [EVENT_NAME] Schedule: [X] Talks on [TOPIC]",
		SubjectExamples: []string{
			"📋 Schedule Released: [SPEAKER_COUNT]+ Speakers at [EVENT_NAME]",
			"Here's what you'll learn at [EVENT_NAME]",
		},
		PreviewPattern: "[SESSION_COUNT]+ sessions. [SPEAKER_COUNT]+ speakers. Build your agenda.",
		ContentPrompt: `Generate a Schedule Announcement email for open-source community.

PRIMARY OBJECTIVE: Showcase learning opportunities. EVERY section must support this. NO other objectives.

EVENT DETAILS:
- Name: [EVENT_NAME]
- Location: [LOCATION]
- Date: [DATES]
- Sessions: [SESSION_COUNT]
- Speakers: [SPEAKER_COUNT]
- Topics: [TOPICS]

REQUIRED INFORMATION HIERARCHY (DO NOT DEVIATE):
1. HEADLINE: "[EVENT_NAME] Schedule is Live"

2. INTRODUCTION (30-50 words, 2 sentences)
   Announce: Schedule is available
   Value: What attendees will learn
   RULE: Only these two ideas. No pricing, no logistics, no other details.

3. PRIMARY CTA: [ View Full Schedule ]

4. LEARNING TRACKS (100-120 words, organized by track)
   Format per track:
   "[TRACK_NAME]: [SESSION_COUNT] sessions
   • [Session title 1]
   • [Session title 2]
   • [Session title 3]"
   Maximum 3 tracks featured (don't list all).

5. FEATURED SPEAKERS (60-80 words)
   Format: "Speaker Name on Topic" (name + topic only, no photos needed)
   List 3-4 top speakers
   Keep it short

6. SECONDARY CTA (optional): [ Register to Attend ]

7. FOOTER

PARAGRAPH ENFORCEMENT:
- MAXIMUM 2-3 sentences per paragraph
- MAXIMUM 60 words per paragraph
- Use bullets for track listings
- Whitespace BETWEEN paragraphs

CTA ENFORCEMENT:
- 1 PRIMARY CTA: "View Full Schedule"
- 1 OPTIONAL SECONDARY CTA: "Register"
- NO unrelated CTAs (no sponsors, no travel, no community)

SECTIONS TO REMOVE (not for this stage):
- Sponsorship information
- Travel details
- Pricing (belongs in Registration Push email)
- Logistics
- Call for Proposals
- Membership
- Post-event content

TONE & VOICE:
✓ Educational focus: "Learn from experts"
✓ Concrete session titles (not generic)
✓ Breadth shown: Multiple tracks
✓ Community perspective: "Peer learning"
✗ NO marketing hype
✗ NO "don't miss"
✗ NO false urgency

VALUE BEFORE DETAILS:
- Opening explains WHAT attendees will learn
- Before listing tracks, explain value

INTERNAL VALIDATION:
□ Single objective: Showcase learning only
□ Every section supports learning opportunity
□ Opening: 30-50 words, 2 sentences
□ Information hierarchy: headline → intro → CTA → tracks → speakers → footer
□ One primary CTA: "View Full Schedule"
□ Maximum 1 secondary CTA: "Register"
□ No pricing, logistics, or sponsorship info
□ No paragraph exceeds 3 sentences or 60 words
□ Bullets used for track listing
□ Mobile-friendly
□ Scannable in <1 minute
□ Authentic tone
□ Word count: 200-280 words

IF ANY RULE FAILS: Revise before returning.

WORD COUNT TARGET: 200-280 words`,
		CTAStrategy: []string{
			"Primary: View Full Schedule (schedule/agenda link)",
			"Secondary: Register to Attend (registration page)",
		},
		FooterNote: "Early registration helps us plan better. See you soon.",
	},

	"Registration Push": {
		StageName:    "Registration Push",
		Purpose:      "Drive registrations before deadline",
		Timing:       "2-4 weeks before event",
		Tone:         "Direct, value-focused, factual",
		UrgencyLevel: 7,
		// Every pricing claim carries a PLACEHOLDER, so the OMIT rule can drop it when nothing
		// supplies one -- and nothing does today. "Early bird pricing available now" had no
		// placeholder, so the rule could not reach it and the model was told, as fact, that early
		// pricing exists for an event whose price was never supplied. This is the DEFAULT stage
		// that every unrecognised value resolves to, so that assertion rode on most generations.
		SubjectPattern: "🎟️ Register for [EVENT_NAME] by [DATE]",
		SubjectExamples: []string{
			"🎟️ Early bird pricing ends [DATE]",
			"Register before [DATE] and save [AMOUNT]",
		},
		PreviewPattern: "Registration closes [DATE]. [EARLY_PRICE] pricing available now.",
		ContentPrompt: `Generate a Registration Push email for open-source community.

PRIMARY OBJECTIVE: Drive registrations. EVERY section must support this. NO other objectives.

EVENT DETAILS:
- Name: [EVENT_NAME]
- Location: [LOCATION]
- Date: [DATES]
- Early Price: [EARLY_PRICE]
- Regular Price: [REGULAR_PRICE]
- Savings: [SAVINGS_AMOUNT]
- Deadline: [DEADLINE]
- Days Left: [DAYS_LEFT]

REQUIRED INFORMATION HIERARCHY:
1. HEADLINE: "Early Bird Pricing Ends [DEADLINE]"

2. INTRODUCTION (30-50 words, 2 sentences)
   Line 1: Early bird closes [DATE]
   Line 2: State savings amount
   RULE: Only deadline + savings. No event description.

3. PRIMARY CTA: [ Register Now ]

4. WHAT'S INCLUDED (80-100 words, bullets)
   • Access to [SESSION_COUNT]+ sessions
   • Workshops on [TOPIC_1], [TOPIC_2]
   • Networking with [ATTENDEE_COUNT]+ community members
   • [One additional specific benefit]

5. PRICING SECTION (40-60 words, clear format)
   Early Bird: [EARLY_PRICE] (through [DEADLINE])
   Standard: [REGULAR_PRICE] (after [DEADLINE])
   Savings: [SAVINGS_AMOUNT]

6. SECONDARY CTA (optional): [ View All Options ]

7. FOOTER

PARAGRAPH ENFORCEMENT:
- MAXIMUM 2-3 sentences per paragraph
- MAXIMUM 60 words per paragraph
- Use bullets for benefits
- Use structured format for pricing
- Whitespace BETWEEN paragraphs

CTA ENFORCEMENT:
- 1 PRIMARY CTA: "Register Now"
- 1 OPTIONAL SECONDARY CTA: "View All Options"
- NO unrelated CTAs (no sponsors, no travel, no other)

SECTIONS TO REMOVE:
- Sponsorship
- Travel information
- Agenda details (belongs in Schedule email)
- Logistics
- Call for Proposals
- Testimonials
- Community links
- Membership

URGENCY RULES (CRITICAL):
✓ Factual deadline: "Ends [DATE] at [TIME]"
✓ Clear savings: "[SAVINGS_AMOUNT]"
✗ NO fake scarcity: "Only 5 spots" (if 5,000 capacity)
✗ NO aggressive language: "ACT NOW!!!"
✗ NO exaggeration: "prices increasing dramatically"

VALUE BEFORE DETAILS:
- Opening states what they get for the price
- Pricing comes after benefits are explained

TONE & VOICE:
✓ Direct, factual
✓ Focus on value (what's included)
✓ Authentic, no hype
✗ NO "amazing," "revolutionary," "last chance"
✗ NO aggressive urgency

INTERNAL VALIDATION:
□ Single objective: Drive registration
□ Every section supports registration
□ Opening: 30-50 words, 2 sentences
□ Information hierarchy: headline → intro → CTA → benefits → pricing → footer
□ One primary CTA: "Register Now"
□ Maximum 1 secondary CTA: "View Options"
□ Factual deadline, no fake urgency
□ Clear pricing comparison (early → standard)
□ Savings amount stated clearly
□ No agenda details, sponsor info, or logistics
□ No paragraph exceeds 3 sentences or 60 words
□ Bullets for benefits
□ Mobile-friendly
□ Scannable in <1 minute
□ Authentic tone
□ Word count: 180-250 words

IF ANY RULE FAILS: Revise before returning.

WORD COUNT TARGET: 180-250 words`,
		CTAStrategy: []string{
			"Primary: Register Now (registration page)",
			"Secondary: View All Options (pricing page)",
		},
		// Both sentences placeholdered. The second asserted a standard-pricing schedule with no
		// placeholder, so the OMIT rule could not reach it even when the first was dropped -- and
		// the pipeline supplies neither a standard price nor a registration deadline.
		FooterNote: "Early bird pricing ends [DEADLINE]. [REGULAR_PRICE] applies after that date.",
	},

	// NOTE: this stage's prompt asks for a `{{ contact.firstname }}` merge token in the greeting.
	// That renders only in HubSpot, which is where this copy goes -- the dispatcher applies it to
	// a cloned HubSpot marketing email. Nothing in this service renders or strips it, so a future
	// consumer that is NOT HubSpot would surface the literal token to a recipient.
	"Discount Offer": {
		StageName:      "Discount Offer",
		Purpose:        "VIP/alumni discount offer",
		Timing:         "2-4 weeks before event",
		Tone:           "Warm, welcoming, community-focused",
		UrgencyLevel:   3,
		SubjectPattern: "Welcome back: [DISCOUNT_AMOUNT] off [EVENT_NAME] for [SEGMENT]",
		SubjectExamples: []string{
			"We'd love to see you again at [EVENT_NAME]",
			"Exclusive rate for [SEGMENT]: [DISCOUNT_AMOUNT] off",
		},
		// Placeholdered for the same reason as Registration Push: unqualified, this asserted both a
		// discount and a promo code, neither of which anything supplies, so the OMIT rule could not
		// reach either claim.
		PreviewPattern: "[DISCOUNT_AMOUNT] rate just for you. [PROMO_CODE] inside.",
		ContentPrompt: `Generate a VIP/Alumni discount email for open-source community.

PRIMARY OBJECTIVE: Offer exclusive rate to segment. EVERY section supports this. NO other objectives.

EVENT DETAILS:
- Name: [EVENT_NAME]
- Location: [LOCATION]
- Date: [DATES]
- Segment: [SEGMENT] (alumni, past speakers, etc.)
- Standard Price: [REGULAR_PRICE]
- Discount Price: [DISCOUNT_PRICE]
- Savings: [DISCOUNT_AMOUNT]
- Code: [PROMO_CODE]

REQUIRED INFORMATION HIERARCHY:
1. HEADLINE: "Welcome Back to [EVENT_NAME]"

2. PERSONALIZED INTRODUCTION (40-50 words, 2 sentences)
   Greeting: "Hi {{ contact.firstname }},"
   Why special: "As a [SEGMENT], we saved this rate for you"
   RULE: Only greeting + why special. No event description.

3. DISCOUNT OFFER (40-50 words, highlighted)
   Format:
   Code: [PROMO_CODE]
   Your price: [DISCOUNT_PRICE]
   Standard: [REGULAR_PRICE]
   Save: [DISCOUNT_AMOUNT]

4. PRIMARY CTA: [ Register with Code [PROMO_CODE] ]

5. WHAT'S NEW (50-70 words, 2-3 sentences) -- ONLY if the brief supplies prior-edition changes
   [PRIOR_EDITION_CHANGES]
   Nothing supplies attendance history or a changelog, so OMIT this section entirely rather than
   inventing what changed since someone last attended.

6. SECONDARY CTA (optional): [ Learn More ]

7. FOOTER

PARAGRAPH ENFORCEMENT:
- MAXIMUM 2-3 sentences per paragraph
- MAXIMUM 60 words per paragraph
- Discount offer in clear, scannable format
- Whitespace BETWEEN paragraphs

CTA ENFORCEMENT:
- 1 PRIMARY CTA: "Register with Code [CODE]"
- 1 OPTIONAL SECONDARY: "Learn More"
- NO unrelated CTAs (no sponsors, no community, no travel)

SECTIONS TO REMOVE:
- Sponsorship
- Travel information
- Agenda (belongs in Schedule email)
- Logistics
- Call for Proposals
- Registration details (only the discount matters)
- Full event description
- Membership pitch

PERSONALIZATION RULES:
✓ Use {{ contact.firstname }} in greeting (separate line)
✓ Reference segment explicitly: "As a [SEGMENT]"
✓ Optional: mention past involvement: "Your [contribution] was valuable"
✗ Don't use token for generic messaging

TONE & VOICE:
✓ Warm, genuinely welcoming
✓ Celebrate their past involvement
✓ Community-first (not transactional)
✓ Authentic, personal
✗ NO mass-mailed feeling
✗ NO desperate language
✗ NO false exclusivity

VALUE BEFORE DETAILS:
- Opening explains WHY they're special
- Discount offer comes early
- Then explain what's changed/improved

INTERNAL VALIDATION:
□ Single objective: Offer exclusive rate
□ Every section supports discount offer
□ Opening: personalized, 40-50 words, 2 sentences
□ Information hierarchy: greeting → why special → code → CTA → what's new → footer
□ One primary CTA: "Register with Code"
□ Maximum 1 secondary: "Learn More"
□ Personalization token used in greeting
□ Discount offer in clear, scannable format
□ No agenda, logistics, or sponsorship
□ No paragraph exceeds 3 sentences or 60 words
□ Mobile-friendly
□ Scannable in <1 minute
□ Genuine, warm tone
□ Word count: 180-240 words

IF ANY RULE FAILS: Revise before returning.

WORD COUNT TARGET: 180-240 words`,
		CTAStrategy: []string{
			"Primary: Register with Code [PROMO_CODE] (registration with pre-filled code)",
		},
		FooterNote: "Questions? Reply to this email. We'd love to hear from you.",
	},

	"Final Countdown": {
		StageName:      "Final Countdown",
		Purpose:        "Confirm attendance and build anticipation",
		Timing:         "1-2 weeks before event",
		Tone:           "Warm, supportive, anticipatory",
		UrgencyLevel:   5,
		SubjectPattern: "[DAYS_LEFT] Days Until [EVENT_NAME]",
		SubjectExamples: []string{
			"Getting ready for [EVENT_NAME]? Here's what to expect.",
			"See you in [DAYS_LEFT] days at [EVENT_NAME]",
		},
		PreviewPattern: "Event guide and schedule inside.",
		ContentPrompt: `Generate a Final Countdown email for open-source community.

PRIMARY OBJECTIVE: Confirm attendance & build anticipation. EVERY section supports this. NO other objectives.

EVENT DETAILS:
- Name: [EVENT_NAME]
- Location: [LOCATION]
- Date: [DATES]
- Days Left: [DAYS_LEFT]
- Start Time: [START_TIME] [TIMEZONE]
- Venue: [VENUE_NAME]
- Highlights: [KEY_SESSIONS]

REQUIRED INFORMATION HIERARCHY:
1. HEADLINE: "[DAYS_LEFT] Days Until [EVENT_NAME]"

2. INTRODUCTION (30-50 words, 2 sentences)
   Excitement: "We're excited to see you..."
   Direction: "Here's what to expect..."
   RULE: Only these. No detailed schedule.

3. PRIMARY CTA: [ View Full Schedule ]

4. KEY LOGISTICS (80-100 words, structured format)
   Date & Time: [DATES], [START_TIME] [TIMEZONE]
   Location: [VENUE_NAME], [ADDRESS]
   What to Bring: (2-3 bullets)
   Getting There: (transit/parking info, brief)

5. WHAT TO EXPECT (80-100 words, 2-3 sentences)
   Brief overview of learning tracks/schedule
   Don't list detailed sessions

6. SECONDARY CTA (optional): [ Download Event App ]

7. FOOTER with support contact

PARAGRAPH ENFORCEMENT:
- MAXIMUM 2-3 sentences per paragraph
- MAXIMUM 60 words per paragraph
- Use structured format for logistics
- Bullets for "What to Bring"
- Whitespace BETWEEN paragraphs

CTA ENFORCEMENT:
- 1 PRIMARY CTA: "View Full Schedule"
- 1 OPTIONAL SECONDARY: "Download Event App"
- NO unrelated CTAs

SECTIONS TO REMOVE (not for countdown):
- Sponsorship
- Call for Proposals
- Registration info
- Detailed agenda (they know this)
- Pricing/costs
- Travel packages
- Membership
- Testimonials

LOGISTICS RULES:
✓ Clear date and time (no ambiguity)
✓ Venue name + address
✓ Transportation info: transit, parking
✓ What to bring: realistic items
✗ NO overwhelming detail
✗ NO unnecessary complexity

VALUE BEFORE DETAILS:
- Opening builds anticipation
- Then provide practical details
- End on positive note

TONE & VOICE:
✓ Warm, supportive
✓ Anticipatory (not anxious)
✓ Helpful and practical
✓ Community-focused
✗ NO hard sell
✗ NO fake urgency
✗ NO "don't miss"

INTERNAL VALIDATION:
□ Single objective: Confirm attendance & anticipation
□ Every section supports this
□ Opening: 30-50 words, 2 sentences
□ Information hierarchy: headline → intro → CTA → logistics → what to expect → footer
□ One primary CTA: "View Schedule"
□ Maximum 1 secondary: "Download App"
□ Clear date, time, timezone, venue
□ Transit/parking info practical
□ No detailed agenda (they know it)
□ No sponsorship or pricing info
□ No paragraph exceeds 3 sentences or 60 words
□ Bullets for "What to Bring"
□ Mobile-friendly
□ Scannable in <1 minute
□ Warm, supportive tone
□ Word count: 200-280 words

IF ANY RULE FAILS: Revise before returning.

WORD COUNT TARGET: 200-280 words`,
		CTAStrategy: []string{
			"Primary: View Full Schedule (agenda/schedule link)",
			"Secondary: Download Event App (mobile app link)",
		},
		FooterNote: "Can't wait to see you there. Questions? Email [SUPPORT_EMAIL]",
	},

	"Post-Event": {
		StageName:      "Post-Event",
		Purpose:        "Thank attendees and extend engagement",
		Timing:         "1-2 days after event",
		Tone:           "Grateful, reflective, community-focused",
		UrgencyLevel:   1,
		SubjectPattern: "Thank You for [EVENT_NAME] – Recordings & Resources",
		SubjectExamples: []string{
			"It was great seeing you at [EVENT_NAME]",
			"[EVENT_NAME] Recap: Recordings, slides, and next steps",
		},
		// Both claims were unsupported: nothing supplies whether recordings exist or how long the
		// survey takes, and neither carried a placeholder for the OMIT rule to act on.
		PreviewPattern: "Thanks for joining us. [SURVEY_URL]",
		ContentPrompt: `Generate a Post-Event Thank You email for open-source community.

PRIMARY OBJECTIVE: Thank attendees & extend engagement. EVERY section supports this. NO other objectives.

EVENT DETAILS:
- Name: [EVENT_NAME]
- Location: [LOCATION]
- Date: [DATES]
- Attendees: [ATTENDEE_COUNT]
- Recordings: [AVAILABLE_DATE]
- Survey: [LINK]
- Community: [SLACK/DISCORD_LINK]
- Next Event: [NEXT_EVENT_DATE] (if scheduled)

REQUIRED INFORMATION HIERARCHY:
1. HEADLINE: "Thank You for [EVENT_NAME]"

2. GRATITUDE (50-60 words, 2 sentences)
   Genuine thanks for attending
   Acknowledge their contribution/participation
   RULE: Only gratitude. No logistics, no next event.

3. WHAT'S AVAILABLE NOW (100-120 words, bullets)
   • Session recordings: [LINK] (available [DATE])
   • Slides and materials: [LINK]
   • Photos: [LINK]
   • Community: Join [COMMUNITY_COUNT]+ in Slack [LINK]

4. PRIMARY CTA: [ Watch Recordings ]

5. FEEDBACK REQUEST (30-40 words, 2 sentences)
   Light ask: "Your 3-minute survey helps us improve"
   Tone: "We'd love to hear from you"

6. SECONDARY CTA (optional): [ Share Your Feedback ]

7. NEXT EVENT TEASER (optional, 40-50 words)
   "Already planning [NEXT_EVENT]. Save the date."
   Tone: invitation, not pressure

8. FOOTER

PARAGRAPH ENFORCEMENT:
- MAXIMUM 2-3 sentences per paragraph
- MAXIMUM 60 words per paragraph
- Use bullets for "What's Available"
- Whitespace BETWEEN paragraphs

CTA ENFORCEMENT:
- 1 PRIMARY CTA: "Watch Recordings"
- 1 OPTIONAL SECONDARY: "Share Feedback"
- 0 others (no sponsors, no merchandise, no membership)

SECTIONS TO REMOVE (not for this stage):
- Sponsorship
- Travel recaps
- Detailed stats (keep it brief)
- Promotional content
- Call for Proposals
- Registration for next year (only teaser)
- Merchandise sales
- Community JOIN NOW (just link to existing)

SECTIONS ALLOWED ONLY IF GENUINELY VALUABLE:
- 1-2 attendee testimonials (genuine quotes, not marketing)
- Event highlights (brief statistics only)
- Call to action for community continuation

GRATITUDE RULES:
✓ Genuine, specific thanks
✓ Acknowledge attendee contribution
✓ Celebrate the community moment
✗ NO transactional tone
✗ NO "thanks for registering"
✗ NO corporate speak

VALUE BEFORE DETAILS:
- Thank them first
- Then show what's available
- Community continuation optional

TONE & VOICE:
✓ Grateful, sincere
✓ Reflective, celebratory
✓ Community-first
✓ Warm, personal
✗ NO sales pitch
✗ NO pressure for next event
✗ NO transactional

INTERNAL VALIDATION:
□ Single objective: Thank & extend engagement
□ Every section supports this
□ Opening: 50-60 words, 2 sentences, genuine thanks
□ Information hierarchy: headline → gratitude → resources → CTA → feedback → optional teaser → footer
□ One primary CTA: "Watch Recordings"
□ Maximum 1 secondary: "Share Feedback"
□ No sponsorship, sales, or promotional content
□ Resources clearly listed with links
□ Feedback request light, optional
□ Next event as teaser only (optional)
□ No paragraph exceeds 3 sentences or 60 words
□ Bullets for resources
□ Mobile-friendly
□ Scannable in <1 minute
□ Genuine, grateful tone
□ Word count: 180-240 words

IF ANY RULE FAILS: Revise before returning.

WORD COUNT TARGET: 180-240 words`,
		CTAStrategy: []string{
			"Primary: Watch Recordings (video library link)",
			"Secondary: Share Your Feedback (survey link)",
		},
		FooterNote: "Recordings available [DATE]. Session slides available [SLIDES_DATE]. Thanks for being part of our community!",
	},
}

# 2026-11-14 — #564 X conversation topics at create

**Update** — `twitterConfig.conversationTopics` (gap 8 of the X Ads gap review, #564) adds
`CONVERSATION` criteria (EQ) to the line item an X create makes, after the interests and before
the included custom audiences.

- The rules live in `twitter.NormalizeConversationTopics`: trimmed, lower-cased, 1-32 letters or
  digits, duplicates dropped, at most 60. The list counts toward the 60-criterion per-create cap.
  `TwitterDispatcher.PreflightCreate` refuses a breach or a wrong JSON type with a synchronous
  400.
- Every topic is checked against `GET targeting_criteria/conversations`, walked to its end, before
  anything is created; the criterion sends the value as X listed it.
- The same default-off `TWITTER_CREATE_TARGETING_ENABLED` flag as gaps 1, 2 and 4. Not
  role-gated.

Not verified live: no X call was made.

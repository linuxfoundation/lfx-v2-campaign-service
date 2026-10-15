# 2026-10-15 — Refused-dial tests no longer race the OS for a closed port (#206)

**Fix** — tests that need a connection-refused dial now get one from a test-only
`http.Transport` instead of a closed `httptest` server or `127.0.0.1:1`.

- Cause: closing an `httptest` server and reusing its URL assumes the port stays refused, but
  the OS may hand that ephemeral port to another listener before the dial. The request is then
  answered, and a test asserting a pre-send classification fails on the environment
  (`TestCreateCampaign_AuthorPostPreSendIsDefinite` failed intermittently in CI).
- Each affected package gets one helper, `refused_dial_test.go`: `refusedURL`
  (`http://refused.invalid`, which can never resolve) and `refusedDialTransport(t)`, a real
  `*http.Transport` whose `DialContext` returns
  `&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}`
  for that address and dials every other address normally. Going through a real transport and
  `http.Client` wraps the error exactly as a real refusal is wrapped, so production classifiers
  (`isPreSendDialError`, `ProbeNotSent`, `ClassifyAdSetWrite`, `ErrWriteNotSent`) match it.
- Sites: reddit `TestCreateCampaign_AuthorPostPreSendIsDefinite`,
  `TestReportBudgets_PreSendDialFailureReleasesTheSlot`,
  `TestReportBudgets_KeywordReadSpendsOnlyOnSentAttempts/pre-send dial failure`,
  `TestCreateCampaign_ConnRefusedNotUnconfirmed`; twitter
  `TestCreateCampaign_AuthorTweetPreSendDialFailure`,
  `TestDeleteTargetingCriterion_Classification/a refused connection is not sent`; meta
  `TestUpdateAdSetStatusOnce_PreSendIsNotSent/connection refused`; hubspot
  `TestDoRequest_ConnectionRefusedIsPreSendNotUnconfirmed`; dispatch
  `TestLinkedIn_VerifyAccountOrg/an inconclusive walk ...`. The reddit and twitter routing
  transports are renamed `postsToRefusedTransport` and `tweetToRefusedTransport`.
- Verified with `go test -race -count=200` on every converted test. Mutation-checked: making the
  helper's dial reach a server that answers 500 fails the classification assertion of every
  converted test except the dispatch one, whose assertions (domain sentinel, no URL in the text)
  hold for a 500 as well, because a 500 is also an inconclusive walk.
- Test-only; no assertion changed. Left alone: closed-server or port-1 tests that only need
  some error (input validation before any dial, the twitter stats-file TLS download, the JWKS
  redaction test), the Postgres DSNs on port 1, and the googleads
  `TestListAccessibleCustomers_PreSendDialErrorIsNotTransportError` (googleads is out of scope
  here).
- Concept updated: `code/internal-dispatch.md`.

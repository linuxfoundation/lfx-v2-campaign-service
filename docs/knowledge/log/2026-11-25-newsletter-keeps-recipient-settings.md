# 2026-11-25 — A newsletter issue keeps its previous issue's recipient settings

**Fix** — found by staging a newsletter issue from the UI and comparing the draft HubSpot holds,
field by field, with the issue it was cloned from. Everything matched (header, footer, style
settings, sender, subscription type, include and exclude lists) except one flag.

- **"Don't send to unengaged contacts" was on in every staged draft.** A marketing email keeps two
  settings beside its lists, `to.suppressGraymail` and `to.limitSendFrequency`. HubSpot gives a
  clone its default for the first (on), and `SetSendList` writes a complete `to` object that does
  not mention either. Each of the five brand newsletters read in the LF portal (LF, OpenSSF,
  PyTorch Foundation, CNCF, Valkey) was sent with it OFF. The next issue was staged on the same
  lists and would have skipped the contacts HubSpot counts as unengaged, so it would have reached
  fewer subscribers than every issue before it, while the page said "Sent to the same lists, with
  the same exclusions".
- **A newsletter issue now states its previous issue's two settings.** `Dispatch` reads them from
  the issue being cloned (`newsletterSendSettings`, `hubspot.Client.GetEmailSendSettings`) and
  writes them in the same PATCH as the lists (`SetSendListWithSettings`). A setting the previous
  issue does not report is not written. The job result carries a warning when the read fails or
  when HubSpot's answer to the write reports a different value.
- **Scope.** Event and survey emails are unchanged: their template is some earlier send, and both
  settings are left to HubSpot as before (`TestHubSpot_AnEventEmailLeavesTheRecipientSettingsToHubSpot`).

Measured while fixing it: the PATCH lands on the draft buffer, and the base email resource of an
unsent clone keeps the clone's initial state until it is published, so a base read after the write
still shows the old value.

Verified live on `tlf`: brief `7a764b3e` (OpenSSF, a test issue) was staged from the UI as draft
228625002300, and its lists were then restated with the October issue's settings through
`SetSendListWithSettings`. HubSpot answered the write with the setting off, and the draft buffer
reads off, with the same include and exclude lists as before. Unsent.

Concepts updated: `code/internal-dispatch.md`, `code/internal-platform-hubspot.md`. Tests:
`hubspot_newsletter_test.go`, `email_send_settings_test.go`.

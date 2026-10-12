# 2026-10-06 — Pin the Microsoft keywords "not connected" messages

**Note** — lfx-self-serve's Microsoft keyword table tells "not connected" from a read failure
by matching two campaign-service messages: the 404 `no microsoft ads connection configured for
this project` and the 400 `keyword and audience insights are not supported for this platform`.
A new test, `TestGetMicrosoftAdsKeywords_PinsTheNotConnectedMessages`, pins both strings, so a
rewording fails here instead of silently turning every unconnected project's table into a read
failure. No behaviour change. See
[Microsoft Keyword Insights](../architecture/microsoft-keyword-insights.md).

// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package constants defines application-wide constants.
package constants

// Environment variable names used to configure the service.
const (
	EnvPort     = "PORT"
	EnvHost     = "HOST"
	EnvDebug    = "DEBUG"
	EnvJWKSURL  = "JWKS_URL"
	EnvAudience = "JWT_AUDIENCE"
	EnvIssuer   = "JWT_ISSUER"
	// EnvMockLocalPrincipal DISABLES JWT verification and treats every request as the
	// named principal. Local development only. The chart declares it with no value and
	// the chart parity test pins it as deliberately not deployable, so setting it is a
	// choice someone has to make on a laptop, never something a deploy can do by default.
	EnvMockLocalPrincipal = "JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL"
	// EnvKubernetesServiceHost is set by the kubelet in every pod. It is read ONLY to
	// refuse EnvMockLocalPrincipal in a deployment: the chart declares that key under
	// app.environment, so an override could otherwise ship a pod with authentication
	// switched off. Nothing in the chart can set or clear this one.
	EnvKubernetesServiceHost = "KUBERNETES_SERVICE_HOST"
	// EnvSurveySequenceWorkflowID is the HubSpot workflow a survey series is cloned from: the
	// one whose schedule, sending windows and suppression already embody marketing operations'
	// own policy. A clone of it carries that policy; building a flow from scratch would mean
	// re-deciding all of it in code.
	//
	// Configurable because the default names a workflow in ONE portal. A deployment pointed at a
	// different HubSpot account has no workflow by that id, and the build fails there with a
	// not-found rather than silently copying a stranger's flow.
	EnvSurveySequenceWorkflowID = "SURVEY_SEQUENCE_WORKFLOW_ID"
	// EnvNATSURL is the NATS server URL used to publish Query Service index updates.
	// Empty DISABLES indexing: every endpoint still serves and campaigns still dispatch;
	// only the search index stops being fed (the Query Service rebuilds a resource's
	// document on its next write).
	EnvNATSURL = "NATS_URL"
	// EnvIndexerServiceToken is the SERVICE credential the index relay stamps onto replayed
	// messages. Optional: with it unset the relay leaves rows pending rather than publishing
	// messages the indexer would reject for a missing authorization header.
	EnvIndexerServiceToken = "INDEXER_SERVICE_TOKEN"
	// EnvIndexerResolveProjectUID, when exactly "true", makes the index relay key each
	// message's FGA checks on the project UID instead of the slug (#590): OpenFGA keys
	// projects by UID, so a slug-keyed document is readable by nobody. Default-off.
	EnvIndexerResolveProjectUID = "INDEXER_RESOLVE_PROJECT_UID"
	// EnvDatabaseURL is an optional PostgreSQL connection string (DSN).
	// Prefer composing from PG* variables when running in-cluster.
	EnvDatabaseURL = "DATABASE_URL"
	// EnvCredentialEncryptionKey is the base64-encoded 32-byte AES-256 key used
	// to encrypt connection credentials. Sourced from a Kubernetes secret.
	EnvCredentialEncryptionKey = "CREDENTIAL_ENCRYPTION_KEY"

	// EnvCampaignJobRetention is how long a TERMINAL campaign job (succeeded, partial or
	// failed) is kept before the retention sweeper deletes it — a Go duration string such
	// as "4320h". Optional: unset uses postgres.DefaultJobRetention (180 days).
	//
	// These rows are the audit trail of real ad spend, so both failure modes fall back to
	// the default rather than to a shorter window: an unparseable value and a non-positive
	// one both leave the default in place. Shortening retention must be an explicit,
	// well-formed operator choice, never the accidental result of a typo.
	EnvCampaignJobRetention = "CAMPAIGN_JOB_RETENTION"

	// EnvEventURLNAT64Prefixes is a comma-separated list of the deployment's
	// NETWORK-SPECIFIC RFC 6052 NAT64 translation prefixes (e.g. "2001:db8:64::/96").
	//
	// It exists because a network-specific prefix cannot be discovered in-process: it is
	// carved from the operator's own global unicast space and is indistinguishable from
	// any other public prefix by inspection. On a cluster that uses one, the translator —
	// not this service — makes the IPv4 connection, so an address encoding 169.254.169.254
	// passes every SSRF check here unless the prefix is declared. Unset is correct for a
	// cluster with no NAT64; the well-known 64:ff9b::/96 is always decoded regardless.
	EnvEventURLNAT64Prefixes = "EVENT_URL_NAT64_PREFIXES"

	// EnvRedditMetricsEnabled opts a deployment IN to Reddit metrics reads. Unset or any
	// value other than "true" leaves them off, and the metrics endpoint answers 400
	// "not supported for this campaign's platform" for Reddit campaigns.
	//
	// The flag originally existed because Reddit's reporting endpoint had no public
	// documentation (LFXV2-2995) and the request shape, response shape and spend currency
	// unit were a best-effort GUESS. That reason is retired: LFXV2-3282 replaced the guess
	// with Reddit's official public OpenAPI spec, and the client was corrected against it —
	// see internal/platform/reddit/metrics.go, whose CONTRACT SOURCE note names the spec.
	//
	// The gate nevertheless STAYS ON, because the remaining unknown is a DIFFERENT one: no
	// request has ever been made against a live Reddit ad account. Matching a published
	// schema does not establish what the endpoint returns for a campaign with no activity,
	// whether ends_at includes its final hour, or whether the account's attribution window
	// shifts the figures. An unexercised read that returns 200 looks authoritative to every
	// consumer, and the caveats live only in code comments the response never carries.
	// Default-off keeps that from reaching anyone until one live read confirms the shape, at
	// which point the default flips and this constant goes away. RedditDispatcher.ReadMetrics
	// carries the same statement at the gate itself; keep the two in step.
	//
	// It also gates the Reddit audience and keyword performance reads (get-reddit-ads-audience,
	// get-reddit-ads-keywords, LFXV2-2665, RedditDispatcher.AudienceEnabled / KeywordsEnabled),
	// which run on the same reporting operation with GENDER/COUNTRY/COMMUNITY or KEYWORD
	// breakdowns added — more of the same unexercised contract.
	EnvRedditMetricsEnabled = "REDDIT_METRICS_ENABLED"

	// EnvMicrosoftMetricsEnabled opts a deployment IN to Microsoft Advertising metrics
	// reads. Unset or any value other than "true" leaves them off, and the metrics
	// endpoint answers 400 "not supported for this campaign's platform" for Microsoft
	// campaigns — the same shape Reddit uses above, for the same reason.
	//
	// Microsoft's v13 Reporting contract was implemented from published documentation and
	// has NOT been exercised against a live Microsoft Advertising account: no credentials
	// were available. The pipeline is also unlike every other platform's — an asynchronous
	// submit/poll/download returning a zipped CSV — so there is more surface to be wrong
	// about than a single JSON GET. Default-off keeps an unverified read from looking
	// authoritative until someone runs it against a real account, at which point the
	// default flips and this constant goes away.
	EnvMicrosoftMetricsEnabled = "MICROSOFT_METRICS_ENABLED"

	// EnvMicrosoftAdCopyEnabled opts a deployment IN to microsoftConfig.headlines, descriptions,
	// path1 and path2 (#563), the caller copy of the responsive search ad sent instead of the
	// auto-composed copy. Only the exact value "true" enables them; unset or any other value fails
	// closed, and a create naming any of them is refused with a 400 before any job exists (and
	// again at dispatch, before anything upstream).
	//
	// It names no account resource: it keeps an UNVERIFIED request shape (built from the v13
	// ResponsiveSearchAd reference; no live create has sent caller copy or Path1/Path2) off the
	// shared ad account until a live create confirms it. Turn it on per environment, in
	// lfx-v2-argocd.
	EnvMicrosoftAdCopyEnabled = "MICROSOFT_AD_COPY_ENABLED"

	// EnvMicrosoftCreateNegativeKeywordsEnabled opts a deployment IN to
	// microsoftConfig.negativeKeywords (#563): campaign-level negative keywords attached right
	// after a Microsoft Advertising campaign is created, through the same AddNegativeKeywordsToEntities
	// call the add-negative-keywords lever makes. Only the exact value "true" enables it; unset or
	// any other value fails closed, and a create naming a non-empty list is refused with a 400
	// before any job exists (and again at dispatch, before anything upstream).
	//
	// It names no account resource: it keeps a create-path call that no live create has made off
	// the shared ad account until one confirms it. Turn it on per environment, in lfx-v2-argocd.
	EnvMicrosoftCreateNegativeKeywordsEnabled = "MICROSOFT_CREATE_NEGATIVE_KEYWORDS_ENABLED"

	// EnvRedditKeywordTargetingWritesEnabled opts a deployment IN to BOTH Reddit ad-group targeting
	// writes (LFXV2-2665): REMOVING keywords (remove-keyword-targeting) and ADDING negative keywords
	// to excluded_keywords (add-negative-keywords) — the same whole-targeting PATCH, so one switch.
	// Only the exact value "true" enables them; unset or any other value fails closed and both
	// endpoints answer 400 "not supported", the shape the metrics gates above use. The
	// keyword-targeting READ is not gated.
	//
	// Reddit has no per-keyword write: the ad group's targeting object is replaced as a whole on
	// PATCH (per secondary references to the v3 OpenAPI document, which could not be fetched from
	// the authoring environment), so the write sends back every targeting dimension it read. That
	// round trip has NOT been exercised against a live ad account, and a read representation the
	// write interprets differently would change geo, community or interest targeting on a live,
	// spending ad group. Default-off until one live write confirms it, at which point the default
	// flips and this constant goes away.
	EnvRedditKeywordTargetingWritesEnabled = "REDDIT_KEYWORD_TARGETING_WRITES_ENABLED"

	// EnvRedditConversionEventEnabled opts a deployment IN to redditConfig.conversionEvent (#549):
	// the pixel event a conversions campaign optimizes toward, sent as the campaign's and the ad
	// group's optimization_goal in place of the default PURCHASE. Only the exact value "true"
	// enables it; unset or any other value fails closed, and a create that sets the field is
	// refused with a 400 before any job exists. The field names no account resource, so it is not
	// role-gated. Default-off because no live create has yet confirmed that Reddit accepts a goal
	// other than PURCHASE on a CONVERSIONS campaign under campaign budget optimization; the values
	// come from the published optimization-goal enum. Flip after one live create, at which point
	// the gate is removed and this constant goes away.
	EnvRedditConversionEventEnabled = "REDDIT_CONVERSION_EVENT_ENABLED"

	// EnvRedditCustomAudiencesEnabled opts a deployment IN to Reddit custom audiences (#549): the
	// create inputs redditConfig.customAudienceIds / excludedCustomAudienceIds (sent as the ad
	// group's targeting.custom_audience_ids / excluded_custom_audience_ids) and the picker read
	// list-reddit-ads-custom-audiences (GET /ad_accounts/{id}/custom_audiences). Only the exact
	// value "true" enables them; unset or any other value fails closed: a create that sets either
	// list is refused with 400 before any job exists, and the read answers 400 "not supported",
	// the shape the gates above use.
	//
	// Both shapes come from Reddit's public v3 OpenAPI document and have NOT been exercised against
	// a live ad account. Turn it on in lfx-v2-argocd only after a live create confirms Reddit
	// accepts the targeting members and the list answers in the documented envelope. Independent
	// of it, an audience names a resource of the LF ad account every foundation shares, so the
	// create inputs also require global_marketing_ops (service.RedditMarketingOpsInputs) and the
	// read sits in the gateway's marketing-ops-api rule.
	EnvRedditCustomAudiencesEnabled = "REDDIT_CUSTOM_AUDIENCES_ENABLED"

	// EnvRedditTargetingExclusionsEnabled opts a deployment IN to redditConfig.excludedSubreddits and
	// redditConfig.excludedGeoTargets (#549), sent at create time as the ad group's
	// excluded_communities and excluded_geolocations. Only the exact value "true" enables them; unset
	// or any other value fails closed, and a create naming either is refused with a 400 before any
	// job exists (and again at dispatch, before anything upstream).
	//
	// Like EnvGoogleAdsDemographicTargetingEnabled it names no account resource: it keeps an
	// UNVERIFIED request shape (built from Reddit's published v3 OpenAPI schema; no live create has
	// sent either field) off the shared ad account until a live create confirms it. Turn it on per
	// environment, in lfx-v2-argocd.
	EnvRedditTargetingExclusionsEnabled = "REDDIT_TARGETING_EXCLUSIONS_ENABLED"

	// EnvRedditTargetingResolutionEnabled opts a deployment IN to resolving redditConfig.subreddits
	// and redditConfig.interests before create (#549 gaps 4 and 8): each community is confirmed
	// with GET /targeting/communities and each interest resolved to its id with
	// GET /targeting/interests, as a synchronous 400 for an unknown value (the pre-check fails open
	// when the lookup cannot run) and again at dispatch, fail-closed, before anything is created.
	// Only the exact value "true" enables it; unset or any other value keeps the create as before
	// (values sent as given, with the ad-group 400 fallback). Default off because neither read has
	// been exercised against a live account; the issue asks for a probe of the interests shape.
	EnvRedditTargetingResolutionEnabled = "REDDIT_TARGETING_RESOLUTION_ENABLED"

	// EnvTwitterCreateTargetingEnabled opts a deployment IN to twitterConfig.geoTargets and
	// twitterConfig.languages (#564): country and language targeting criteria added to the line
	// item an X create makes (keywords, interestIds, conversationTopics, platforms, devices,
	// ageBucket and gender joined later, #564 gaps 4, 8 and 9). Only the exact value "true" enables them; unset or any other value
	// fails closed, and a create naming either is refused with a 400 before any job exists (and
	// again at dispatch, before anything upstream).
	//
	// It names no account resource: it keeps an UNVERIFIED request shape (built from X's published
	// v12 reference; no live create has sent a targeting criterion or read the location and
	// language lookups) off the shared ad account until a live create confirms it. Turn it on per
	// environment, in lfx-v2-argocd.
	EnvTwitterCreateTargetingEnabled = "TWITTER_CREATE_TARGETING_ENABLED"

	// EnvTwitterCustomAudiencesEnabled opts a deployment IN to X custom audiences (#564 gap 3): the
	// create inputs twitterConfig.customAudienceIds / excludedCustomAudienceIds (CUSTOM_AUDIENCE
	// targeting criteria, EQ and NE, on the new line item) and the picker read
	// GET /projects/{project_id}/twitter-ads/custom-audiences. Only the exact value "true" enables
	// them; unset or any other value fails closed: a create that sets either list is refused with
	// 400 before any job exists, and the read answers 400 "not supported".
	//
	// Both shapes come from X's published Ads API v12 reference and have NOT been exercised
	// against a live ad account. Turn it on in lfx-v2-argocd only after a live create confirms
	// them. Independent of it, an audience names a resource of the LF ad account every foundation
	// shares, so the create inputs also require global_marketing_ops
	// (service.TwitterMarketingOpsInputs) and the read sits in the gateway's marketing-ops-api rule.
	EnvTwitterCustomAudiencesEnabled = "TWITTER_CUSTOM_AUDIENCES_ENABLED"

	// EnvTwitterAudienceExpansionEnabled opts a deployment IN to twitterConfig.audienceExpansion
	// (#564 gap 11): the line item an X create makes is sent with audience_expansion BROAD, DEFINED
	// or EXPANDED. Only the exact value "true" enables it; unset or any other value fails closed,
	// and a create setting it is refused with a 400 before any job exists (and again at dispatch,
	// before anything upstream).
	//
	// It names no account resource: it keeps an UNVERIFIED line-item parameter (from X's published
	// v12 reference; no live create has sent it) off the shared ad account until a live create
	// confirms X accepts it on a WEBSITE_CLICKS line item. Turn it on per environment, in
	// lfx-v2-argocd.
	EnvTwitterAudienceExpansionEnabled = "TWITTER_AUDIENCE_EXPANSION_ENABLED"

	// EnvTwitterFollowerLookalikesEnabled opts a deployment IN to
	// twitterConfig.followerLookalikeUserIds (#564 gap 4c): each id becomes a
	// SIMILAR_TO_FOLLOWERS_OF_USER criterion on the line item an X create makes. Only the exact
	// value "true" enables it; unset or any other value fails closed, and a create setting it is
	// refused with a 400 before any job exists (and again at dispatch, before anything upstream).
	//
	// It names no account resource (a public user id). It keeps an UNVERIFIED targeting type off
	// the shared ad account until a live create confirms it: X has no user lookup, so an id X
	// refuses fails at its criterion POST and leaves a PAUSED partial for reconciliation.
	EnvTwitterFollowerLookalikesEnabled = "TWITTER_FOLLOWER_LOOKALIKES_ENABLED"

	// EnvTwitterTweetVariantsEnabled opts a deployment IN to twitterConfig.variants (#564 gap 10):
	// one authored, promoted tweet per variant (at most 5) on the line item an X create makes. Only
	// the exact value "true" enables it; unset or any other value fails closed, and a create setting
	// it is refused with a 400 before any job exists (and again at dispatch). It names no account
	// resource; it keeps several tweet publishes per create off the shared ad account until a live
	// create confirms them.
	EnvTwitterTweetVariantsEnabled = "TWITTER_TWEET_VARIANTS_ENABLED"

	// EnvTwitterWebsiteCardEnabled opts a deployment IN to twitterConfig.card (#564 gap 6): a
	// website card (POST accounts/:account_id/cards: the creative's media, a headline, the UTM'd
	// registration URL) attached by card_uri to the tweet(s) an X create authors. Only the exact
	// value "true" enables it; unset or any other value fails closed, and a create setting it is
	// refused with a 400 before any job exists (and again at dispatch). It names no account
	// resource; it keeps an UNVERIFIED request body (from X's published reference) off the shared
	// ad account until a live create confirms it.
	EnvTwitterWebsiteCardEnabled = "TWITTER_WEBSITE_CARD_ENABLED"

	// EnvTwitterVideoEnabled opts a deployment IN to twitterConfig.videoAssetId (#564 gap 7): a
	// stored MP4 uploaded as amplify_video (chunked initialize/append/finalize, then STATUS polled
	// for at most 30 s) and attached to the tweet(s) an X create authors. Only the exact value
	// "true" enables it; unset or any other value fails closed, and a create setting it is refused
	// with a 400 before any job exists (and again at dispatch). Blocker before enabling: measure
	// X's amplify_video processing time for a 20-30 MiB MP4 against the 30 s bound, which runs
	// inside the two-minute provider call and must not be lengthened.
	EnvTwitterVideoEnabled = "TWITTER_VIDEO_ENABLED"

	// EnvTwitterKeywordAddEnabled opts a deployment IN to the positive keyword add lever on an
	// existing X campaign (#564 gap 12): POST .../keyword-targeting/additions, one BROAD_KEYWORD
	// targeting criterion (operator_type EQ) per keyword on the line item this service created.
	// Only the exact value "true" enables it; unset or any other value fails closed, and the
	// endpoint answers 400 "not supported or not enabled" before X is contacted.
	//
	// It names no account resource: it keeps an UNVERIFIED write (a positive keyword criterion
	// POST on a live line item, built from X's published v12 reference; no live call has added
	// one) off the shared ad account until a live add confirms the criterion shape. Turn it on per
	// environment, in lfx-v2-argocd.
	EnvTwitterKeywordAddEnabled = "TWITTER_KEYWORD_ADD_ENABLED"

	// EnvMetaConversionsMetricsEnabled opts a deployment IN to conversions in the Meta metrics read
	// (#550 gap 6): for a campaign whose create result records a conversion event (the
	// conversions objective, #559), the Insights read also requests `actions` and reports the
	// matching action type's count as conversions. Only the exact value "true" enables it; unset
	// or any other value keeps the read exactly as before (conversions unavailable, no `actions`
	// requested). A read-only change, but the event-to-action-type mapping comes from Meta's
	// reference and has not been checked against a live account, and a wrong mapping would
	// publish a false zero. Turn it on per environment, in lfx-v2-argocd.
	EnvMetaConversionsMetricsEnabled = "META_CONVERSIONS_METRICS_ENABLED"

	// EnvMicrosoftAudiencesEnabled opts a deployment IN to microsoftConfig.audienceIds,
	// excludedAudienceIds and audienceBidAdjustment (#563 gap 4): remarketing lists observed (bid
	// only) and excluded on the ad group a Microsoft create makes. Only the exact value "true"
	// enables them; unset or any other value fails closed, and a create naming any of the three is
	// refused with a 400 before any job exists (and again at dispatch, before anything upstream).
	//
	// The shapes come from Microsoft's published Campaign Management v13 reference and have NOT
	// been exercised against a live account. Independent of it, a remarketing list is a resource of
	// the LF Microsoft Advertising account every foundation shares, so the lists also require
	// global_marketing_ops (service.MicrosoftMarketingOpsInputs). Turn it on in lfx-v2-argocd only
	// after a live create confirms the lookup and the criterion shapes.
	EnvMicrosoftAudiencesEnabled = "MICROSOFT_AUDIENCES_ENABLED"

	// EnvMicrosoftCreateTargetingEnabled opts a deployment IN to microsoftConfig.excludedGeoTargets,
	// proximityTargets and languages (#563 gaps 5-6): negative location and radius criteria on the
	// new campaign, and its ad languages. Only the exact value "true" enables them; unset or any
	// other value fails closed, and a create naming any of the three is refused with a 400 before
	// any job exists (and again at dispatch, before anything upstream).
	//
	// It names no account resource: it keeps UNVERIFIED request shapes (built from Microsoft's
	// published Campaign Management v13 reference; no live create has sent them) off the shared
	// account until a live create confirms them. Turn it on per environment, in lfx-v2-argocd.
	EnvMicrosoftCreateTargetingEnabled = "MICROSOFT_CREATE_TARGETING_ENABLED"

	// EnvMicrosoftAdExtensionsEnabled opts a deployment IN to microsoftConfig.sitelinks and
	// microsoftConfig.callouts (#563), created at create time as SitelinkAdExtension and
	// CalloutAdExtension entries and associated with the new Search campaign. Only the exact value
	// "true" enables them; unset or any other value fails closed, and a create naming either is
	// refused with a 400 before any job exists (and again at dispatch, before anything upstream).
	//
	// It names no account resource (the extensions are created new for the campaign): it keeps an
	// UNVERIFIED request shape, built from the v13 Campaign Management reference with no live
	// create having sent it, off the shared ad account until a live create confirms it. Turn it on
	// per environment, in lfx-v2-argocd.
	EnvMicrosoftAdExtensionsEnabled = "MICROSOFT_AD_EXTENSIONS_ENABLED"

	// EnvTwitterMetricsEnabled opts a deployment IN to the two X (Twitter) Ads features built on
	// X's asynchronous stats-jobs API: the ACCOUNT MONITOR
	// (GET /projects/{project_id}/connection-twitter-ads/account-monitor) and the AUDIENCE READ
	// (GET /projects/{project_id}/twitter-ads/audience). Only the exact value "true" enables them;
	// unset or any other value fails closed and both endpoints answer 400 "not supported", the
	// same shape as Reddit and Microsoft above.
	//
	// The monitor reads X's asynchronous stats-jobs API (active_entities, job create, job
	// status, a downloaded gzip results file), implemented from X's published documentation
	// and NOT exercised against a live X Ads account: no credentials were available. It gates
	// only these two stats-job features — X's per-campaign metrics read is a different, synchronous endpoint and
	// is not affected. Once the contract is verified the default flips and this constant goes
	// away.
	EnvTwitterMetricsEnabled = "TWITTER_METRICS_ENABLED"

	// EnvGoogleAdsDemandGenAudienceEnabled opts a deployment IN to googleAdsConfig.demandGenAudience
	// (#459): one existing Google Ads Audience attached to a Demand Gen ad group, and to each
	// googleAdsConfig.demandGenAdGroups[].audience (#460), the same input on a named ad group,
	// and to googleAdsConfig.demandGenDemographics.audienceSegments (#498), existing user lists put
	// in the demographic Audience. Only the exact value "true" enables them; unset or any other value fails closed, and a
	// create naming one is refused before anything upstream exists.
	//
	// A per-deployment opt-in, off by default; ops turns it on by setting it to exactly "true"
	// in lfx-v2-argocd. The Audience is checked against the connected Google Ads account (same
	// customer id). That account is shared by every foundation and campaign_manager includes
	// foundation executive directors, so create-campaigns also requires global_marketing_ops on
	// the project for any Demand Gen audience (service.GoogleAdsMarketingOpsInputs). The Display,
	// Performance Max and shared-list flags below share that check.
	EnvGoogleAdsDemandGenAudienceEnabled = "GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED"

	// EnvGoogleAdsDisplayAudiencesEnabled opts a deployment IN to the Display audience lists
	// that name an ACCOUNT-OWNED segment (#462): googleAdsConfig.audienceSegments on the
	// "display" channel, customAudiences and customAffinities. Only the exact value "true"
	// enables them; unset or any other value fails closed, and a create naming one is refused
	// before anything upstream exists.
	//
	// A per-deployment opt-in, off by default, like EnvGoogleAdsDemandGenAudienceEnabled: ops
	// turns it on by setting it to exactly "true" in lfx-v2-argocd. Each segment is checked
	// against the connected Google Ads account (same customer id). That account is shared by
	// every foundation and campaign_manager includes foundation executive directors, so
	// create-campaigns also requires global_marketing_ops on the project for any of these
	// segments (service.GoogleAdsMarketingOpsInputs). userInterests (Google's own
	// affinity and in-market categories) and excludedPlacements (public domains) name no
	// account resource and are not gated.
	EnvGoogleAdsDisplayAudiencesEnabled = "GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED"

	// EnvMetaCallToActionEnabled opts a deployment IN to metaConfig.callToAction (#550): the
	// button on every Meta ad (SIGN_UP, BUY_TICKETS, …) instead of the fixed LEARN_MORE. Only the
	// exact value "true" enables it; unset or any other value fails closed, and a create carrying
	// the field is refused with a 400 before any job exists. Off by default until the creative
	// payload is verified against a live ad account. It names no account resource, so it needs no
	// global_marketing_ops.
	EnvMetaCallToActionEnabled = "META_CALL_TO_ACTION_ENABLED"

	// EnvMetaConversionEventEnabled opts a deployment IN to metaConfig.conversionEvent (#550): the
	// promoted object's custom_event_type for the conversions objective (COMPLETE_REGISTRATION,
	// LEAD, …) instead of the fixed PURCHASE. Same rules as EnvMetaCallToActionEnabled.
	EnvMetaConversionEventEnabled = "META_CONVERSION_EVENT_ENABLED"

	// EnvMetaAudienceTargetingEnabled opts a deployment IN to metaConfig.targeting (#550): age,
	// gender, custom audiences to include and exclude, and the Advantage+ audience flag on the ad
	// set. Only the exact value "true" enables it; unset or any other value fails closed, and a
	// create carrying a targeting block is refused before any job exists (and again in the job,
	// before any Meta request).
	//
	// A per-deployment opt-in, off by default, because the payloads were built from Meta's docs and
	// not yet verified against a live ad account. Each custom audience is read and must belong to
	// the connected ad account before anything is created. That account is shared by every
	// foundation, so create-campaigns also requires global_marketing_ops on the project for the
	// audience lists (service.MetaMarketingOpsInputs); age, gender and advantageAudience are not
	// gated.
	EnvMetaAudienceTargetingEnabled = "META_AUDIENCE_TARGETING_ENABLED"

	// EnvGoogleAdsPMaxAudienceSignalEnabled opts a deployment IN to
	// googleAdsConfig.performanceMaxAudienceSignal (#461): one existing Google Ads Audience
	// attached to a Performance Max asset group as a signal. Only the exact value "true"
	// enables it; unset or any other value fails closed, and a create naming one is refused
	// before anything upstream exists.
	//
	// A per-deployment opt-in, off by default, like EnvGoogleAdsDemandGenAudienceEnabled: ops
	// turns it on by setting it to exactly "true" in lfx-v2-argocd. The Audience is checked
	// against the connected Google Ads account (same customer id), which every foundation
	// shares, so create-campaigns also requires global_marketing_ops on the project for it
	// (service.GoogleAdsMarketingOpsInputs). Search themes (free text) and the
	// extensions (text and the campaign's own URLs) name no account resource and are not gated.
	EnvGoogleAdsPMaxAudienceSignalEnabled = "GOOGLE_ADS_PMAX_AUDIENCE_SIGNAL_ENABLED"

	// EnvGoogleAdsSharedNegativeListsEnabled opts a deployment IN to
	// googleAdsConfig.sharedNegativeKeywordLists (#477): existing shared negative keyword lists
	// (sharedSets) attached to a Search campaign. Only the exact value "true" enables it; unset
	// or any other value fails closed, and a create naming one is refused before anything
	// upstream exists.
	//
	// A per-deployment opt-in, off by default, like EnvGoogleAdsDemandGenAudienceEnabled: ops
	// turns it on by setting it to exactly "true" in lfx-v2-argocd. Each shared set is checked
	// against the connected Google Ads account (same customer id), which every foundation
	// shares, so create-campaigns also requires global_marketing_ops on the project for any
	// shared list (service.GoogleAdsMarketingOpsInputs). searchAssets (the caller's own
	// images and text, created as new assets) names no account resource and is not gated.
	EnvGoogleAdsSharedNegativeListsEnabled = "GOOGLE_ADS_SHARED_NEGATIVE_LISTS_ENABLED"

	// EnvGoogleAdsDemographicTargetingEnabled opts a deployment IN to the Google Ads demographic
	// inputs not yet exercised by a live (persisted) create: googleAdsConfig.observedDemographics
	// (#497), displayDemographics (#499) and googleAdsConfig.demandGenDemographics (#498, whose
	// create body a validateOnly probe accepted on 2026-11-05, while a persisted create and the
	// read-back its reuse-by-name relies on are not yet verified). Only the exact value "true" enables
	// it; unset or any other value fails closed, and a create naming one is refused before
	// anything upstream exists.
	//
	// It is a different kind of gate from the four above. Those keep an account-owned resource
	// out of a create; this one keeps an UNVERIFIED request shape off the shared account until
	// someone has probed it, because a wrong guess fails after the budget and campaign exist.
	// Turn it on per environment, in lfx-v2-argocd, once the probe in the field's docs passes.
	EnvGoogleAdsDemographicTargetingEnabled = "GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED"

	// EnvLinkedInAdsConversionsEnabled opts a deployment IN to linkedInConfig.conversions (#552):
	// conversion rule ids associated with the campaign through Campaign Conversions once it exists.
	// Only the exact value "true" enables it; unset or any other value fails closed, and a create
	// naming conversions is refused before a job exists. It keeps an UNVERIFIED request (the
	// campaignConversions PUT) off the live ad account until the probe in the field's docs passes.
	// The ids also require global_marketing_ops on the project (service.LinkedInMarketingOpsInputs).
	EnvLinkedInAdsConversionsEnabled = "LINKEDIN_ADS_CONVERSIONS_ENABLED"

	// EnvForceSystemAdsAccount, when exactly "true", makes the LF-owned system account
	// (model.SystemProjectID) the PRIMARY credential source for every paid-ads dispatch,
	// so every campaign authenticates as the marketing-ops account regardless of any
	// per-project connection. Read once at credsSource construction (see
	// internal/dispatch/creds.go); default-off, enabled per-environment via the ArgoCD
	// overlay like the cutover flags. Excludes HubSpot/email by construction (the forced
	// path gates on Provider.IsPaidAds()). See specs/006-force-system-ads-account.
	EnvForceSystemAdsAccount = "LFX_FORCE_SYSTEM_ADS_ACCOUNT"

	// LLM settings, used ONLY to generate email copy (LFXV2-2775). Optional as a GROUP:
	// with url or key unset, the GenerateEmailCopy endpoint returns 503 (service unavailable).
	// The service itself starts successfully with or without these configured.
	// The secret is the LF LiteLLM proxy's own key — the proxy holds the Bedrock credentials.
	// AI_MODEL is not a secret; unset selects llm.DefaultModel.
	EnvAIProxyURL = "AI_PROXY_URL"
	EnvAIAPIKey   = "AI_API_KEY"
	EnvAIModel    = "AI_MODEL"

	// Signed creative-asset URLs (LFXV2-2665): how a Reddit dispatch hands Reddit a stored
	// creative asset, which Reddit only accepts as a public URL it fetches. Optional as a
	// PAIR: with the key or the base URL unset the feature is OFF — the serving route answers
	// 404 and a Reddit dispatch that references a stored asset is refused before any upstream
	// call. EVERY value that is SET is validated, even while the feature is off for want of its
	// other half, and an unusable one (key under 32 bytes or not base64, base URL not https or
	// not a bare origin, TTL outside 5m..60m) fails startup. See
	// docs/knowledge/architecture/signed-creative-asset-urls.md.
	// The key is a SECRET (standard base64, >= 32 bytes) sourced from the chart's Secret.
	EnvCreativeAssetURLSigningKey = "CREATIVE_ASSET_URL_SIGNING_KEY"
	// The public https origin Reddit can reach this service at (the gateway host).
	EnvCreativeAssetPublicBaseURL = "CREATIVE_ASSET_PUBLIC_BASE_URL"
	// How long a minted URL is valid; a Go duration, default 10m, hard max 60m.
	EnvCreativeAssetURLTTL = "CREATIVE_ASSET_URL_TTL"

	// Snowflake (read-only) settings, used ONLY to resolve an event's past editions when
	// building an audience. All are OPTIONAL: with none set, audience building still works
	// and produces a country-only audience, recording the narrower scope in its summary.
	// A partially-configured warehouse is treated as unconfigured rather than failing boot,
	// since indexing an event's history is an enrichment, not a correctness requirement.
	EnvSnowflakeAccount    = "SNOWFLAKE_ACCOUNT"
	EnvSnowflakeUser       = "SNOWFLAKE_USER"
	EnvSnowflakePrivateKey = "SNOWFLAKE_PRIVATE_KEY"
	EnvSnowflakeWarehouse  = "SNOWFLAKE_WAREHOUSE"
	EnvSnowflakeRole       = "SNOWFLAKE_ROLE"

	// PostgreSQL connection settings (composed into a DSN in-process).
	EnvPGHost     = "PGHOST"
	EnvPGPort     = "PGPORT"
	EnvPGUser     = "PGUSER"
	EnvPGPassword = "PGPASSWORD"
	EnvPGDatabase = "PGDATABASE"
	EnvPGEngine   = "PGENGINE"
)

// Default PostgreSQL port when PGPORT is unset.
const (
	DefaultPGPort = "5432"
)

// Default configuration values. These mirror the defaults wired into the Helm
// chart so local runs match in-cluster behavior.
const (
	// DefaultHTTPPort is the default port the HTTP server listens on.
	DefaultHTTPPort = "8080"
	// DefaultHost is the default bind interface ("*" binds all interfaces).
	DefaultHost = "*"
	// DefaultJWKSURL is the default JSON Web Key Set endpoint for JWT validation.
	DefaultJWKSURL = "http://lfx-platform-heimdall.lfx.svc.cluster.local:4457/.well-known/jwks"
	// DefaultAudience is the default intended audience for JWT tokens.
	DefaultAudience = "lfx-v2-campaign-service"
	// DefaultIssuer is the default expected JWT issuer.
	DefaultIssuer = "heimdall"
	// DefaultNATSURL is the default NATS server URL.
	DefaultNATSURL = "nats://lfx-platform-nats.lfx.svc.cluster.local:4222"
)

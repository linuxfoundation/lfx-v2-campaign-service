-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- as_user_id pins WHICH HANDLE a nullcast tweet is authored under, and it belongs on the
-- CONNECTION because it is an authorization fact, not a per-campaign choice.
--
-- The X client already resolves a promotable user and refuses to guess between several.
-- What it cannot do is answer the other question: the promotable-users list says a handle
-- is promotable BY THIS AD ACCOUNT, and on the shared LF system connection that is true of
-- every LF handle. So a request naming another project's handle passed every check and
-- published as them -- the caller named the identity and the service supplied the
-- authority. Storing the identity here is what lets the dispatcher refuse that.
--
-- NULLable, no default, no backfill. An existing connection genuinely has no declared
-- identity, and a guess would be the same confused deputy written into the database. Where
-- it is absent the dispatcher's behaviour is unchanged, and the residual gap is narrow:
-- the client still refuses to guess when several candidates exist and the caller pinned
-- nothing. Seeding this column on the shared system connection is what closes it for real.
ALTER TABLE twitter_ads_connections ADD COLUMN as_user_id TEXT;

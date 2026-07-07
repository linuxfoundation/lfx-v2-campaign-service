-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Per-provider connection tables. Each is singleton per project (UNIQUE
-- (project_id)); connections are NOT indexed into the Query Service, so
-- attribution lives inline in created_by / updated_by. Credentials are stored
-- as AES-256-GCM ciphertext, encrypted at the application layer.
--
-- gen_random_uuid() is in PostgreSQL core since v13 (no pgcrypto extension).

CREATE TABLE IF NOT EXISTS google_ads_connections (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id         UUID        NOT NULL UNIQUE,
    label              TEXT,
    account_id         TEXT        NOT NULL,
    credentials        BYTEA,
    status             TEXT        NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active','inactive','error','deleted')),
    version            BIGINT      NOT NULL DEFAULT 1,
    created_by         JSONB,
    updated_by         JSONB,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- provider-specific:
    login_customer_id  TEXT
);

CREATE TABLE IF NOT EXISTS linkedin_ads_connections (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID        NOT NULL UNIQUE,
    label        TEXT,
    account_id   TEXT        NOT NULL,
    credentials  BYTEA,
    status       TEXT        NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','inactive','error','deleted')),
    version      BIGINT      NOT NULL DEFAULT 1,
    created_by   JSONB,
    updated_by   JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- provider-specific:
    org_id       TEXT        NOT NULL
);

CREATE TABLE IF NOT EXISTS meta_ads_connections (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID        NOT NULL UNIQUE,
    label        TEXT,
    account_id   TEXT        NOT NULL,
    credentials  BYTEA,
    status       TEXT        NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','inactive','error','deleted')),
    version      BIGINT      NOT NULL DEFAULT 1,
    created_by   JSONB,
    updated_by   JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- provider-specific:
    page_id      TEXT,
    app_id       TEXT
);

CREATE TABLE IF NOT EXISTS reddit_ads_connections (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID        NOT NULL UNIQUE,
    label        TEXT,
    account_id   TEXT        NOT NULL,
    credentials  BYTEA,
    status       TEXT        NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','inactive','error','deleted')),
    version      BIGINT      NOT NULL DEFAULT 1,
    created_by   JSONB,
    updated_by   JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS twitter_ads_connections (
    id                     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id             UUID        NOT NULL UNIQUE,
    label                  TEXT,
    account_id             TEXT        NOT NULL,
    credentials            BYTEA,
    status                 TEXT        NOT NULL DEFAULT 'active'
                           CHECK (status IN ('active','inactive','error','deleted')),
    version                BIGINT      NOT NULL DEFAULT 1,
    created_by             JSONB,
    updated_by             JSONB,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- provider-specific:
    funding_instrument_id  TEXT
);

CREATE TABLE IF NOT EXISTS microsoft_ads_connections (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID        NOT NULL UNIQUE,
    label        TEXT,
    account_id   TEXT        NOT NULL,
    credentials  BYTEA,
    status       TEXT        NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','inactive','error','deleted')),
    version      BIGINT      NOT NULL DEFAULT 1,
    created_by   JSONB,
    updated_by   JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- provider-specific:
    customer_id  TEXT
);

CREATE TABLE IF NOT EXISTS hubspot_connections (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    UUID        NOT NULL UNIQUE,
    label         TEXT,
    account_id    TEXT        NOT NULL,
    credentials   BYTEA,
    status        TEXT        NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','inactive','error','deleted')),
    version       BIGINT      NOT NULL DEFAULT 1,
    created_by    JSONB,
    updated_by    JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- provider-specific:
    portal_id     TEXT,
    sender_email  TEXT,
    sender_name   TEXT,
    brand_kit     TEXT
);

-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- No-op: 000009 only removes an index that Postgres already refuses to use, so there is
-- nothing to restore. Recreating an INVALID index is neither possible nor desirable.
SELECT 1;

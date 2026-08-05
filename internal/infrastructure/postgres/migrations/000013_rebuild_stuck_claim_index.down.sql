-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- No-op: on the common path, 000013's up is itself a no-op (IF NOT EXISTS after 000008
-- already built a valid index), so 000013 doesn't own the index. Rolling back only this
-- version must not drop an index that 000008 (still applied) is relying on — mirrors
-- 000009's down, which is a no-op for the same ensure/repair-semantics reason.
SELECT 1;

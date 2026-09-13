-- Display acknowledgement only. Never change billing, holds, settlement or
-- source-cycle closure when an administrator clears the exception list.
ALTER TABLE dynamic_quota_requests
    ADD COLUMN IF NOT EXISTS display_cleared_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS display_cleared_by BIGINT;

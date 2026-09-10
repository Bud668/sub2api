-- Additive only: legacy holds/counters are not released, charged or rewritten.
ALTER TABLE dynamic_quota_requests
    ADD COLUMN IF NOT EXISTS owner_user_id BIGINT,
    ADD COLUMN IF NOT EXISTS owner_subscription_id BIGINT,
    ADD COLUMN IF NOT EXISTS worker_id UUID,
    ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS request_context JSONB,
    ADD COLUMN IF NOT EXISTS dispatched_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS outcome TEXT,
    ADD COLUMN IF NOT EXISTS evidence JSONB,
    ADD COLUMN IF NOT EXISTS billing_receipt JSONB,
    ADD COLUMN IF NOT EXISTS billing_windows JSONB,
    ADD COLUMN IF NOT EXISTS billing_retry_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS operator_absorbed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS operator_absorption_reason TEXT,
    ADD COLUMN IF NOT EXISTS operator_absorbed_standard_usd NUMERIC(20,8)
        CHECK (operator_absorbed_standard_usd >= 0),
    -- Only a verified reset closes source liability; original evidence is retained.
    ADD COLUMN IF NOT EXISTS source_closed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS late_evidence JSONB,
    ADD COLUMN IF NOT EXISTS late_billing_receipt JSONB;

CREATE INDEX IF NOT EXISTS idx_dynamic_quota_request_recovery
    ON dynamic_quota_requests (billing_retry_at)
    WHERE billing_receipt IS NOT NULL AND billing_retry_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dynamic_quota_request_lease
    ON dynamic_quota_requests (worker_id, lease_until)
    WHERE status='pending';

CREATE INDEX IF NOT EXISTS idx_dynamic_quota_request_absorbed
    ON dynamic_quota_requests (account_id, operator_absorbed_at DESC, id)
    WHERE operator_absorbed_at IS NOT NULL;

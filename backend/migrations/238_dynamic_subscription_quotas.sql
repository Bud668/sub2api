-- Opt-in only. Existing subscriptions, usage and account scheduling are unchanged.
CREATE TABLE IF NOT EXISTS dynamic_quota_pools (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id),
    revision BIGINT NOT NULL DEFAULT 0,
	config_revision BIGINT NOT NULL DEFAULT 0 CHECK (config_revision >= 0),
	usage_ceiling_percent NUMERIC(5,2) NOT NULL DEFAULT 98 CHECK (usage_ceiling_percent BETWEEN 1 AND 100),
	standard_total_usd NUMERIC(20,8) NOT NULL DEFAULT 0,
	max_request_usd NUMERIC(20,8) NOT NULL DEFAULT 0,
    state JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS dynamic_subscription_policies (
    subscription_id BIGINT PRIMARY KEY REFERENCES user_subscriptions(id),
    account_id BIGINT NOT NULL REFERENCES dynamic_quota_pools(account_id),
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    weight NUMERIC(12,4) NOT NULL DEFAULT 1 CHECK (weight > 0 AND weight <= 1000),
    max_limit_usd NUMERIC(20,8) NOT NULL CHECK (max_limit_usd > 0),
    increase_threshold_usd NUMERIC(4,2) NOT NULL DEFAULT 10 CHECK (increase_threshold_usd IN (5,10)),
    applied_limit_usd NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (applied_limit_usd >= 0),
    used_standard_usd NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (used_standard_usd >= 0),
    allocated_standard_usd NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (allocated_standard_usd >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS dynamic_subscription_account_idx
    ON dynamic_subscription_policies(account_id, subscription_id);

-- No prompts, emails, credentials or external response bodies. Internal billing
-- identifiers are retained to correlate reservations and canonical settlement.
-- In-flight holds are not expired blindly: unknown outcomes require reconciliation.
CREATE TABLE IF NOT EXISTS dynamic_quota_requests (
    id UUID PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES dynamic_quota_pools(account_id),
    cycle BIGINT NOT NULL,
    subscription_id BIGINT REFERENCES user_subscriptions(id),
    api_key_id BIGINT,
    hold_standard_usd NUMERIC(20,8) NOT NULL CHECK (hold_standard_usd >= 0),
    standard_cost_usd NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (standard_cost_usd >= 0),
    actual_cost_usd NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (actual_cost_usd >= 0),
    status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'settled', 'rejected', 'uncertain')),
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS dynamic_quota_requests_pool_idx
    ON dynamic_quota_requests(account_id, cycle, status);
CREATE INDEX IF NOT EXISTS dynamic_quota_requests_pending_idx
    ON dynamic_quota_requests(account_id) WHERE status IN ('pending','uncertain');

CREATE TABLE IF NOT EXISTS dynamic_quota_events (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES dynamic_quota_pools(account_id),
    cycle BIGINT NOT NULL,
    kind VARCHAR(32) NOT NULL,
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS dynamic_quota_reset_once_idx
    ON dynamic_quota_events(account_id, cycle) WHERE kind = 'reset_confirmed';

-- Defence in depth: stale L1 objects, renewal and manual-reset code must not
-- clear an enabled dynamic subscription just because a local seven-day timer ran.
-- The coordinator sets this transaction-local flag only after locking its pool.
CREATE OR REPLACE FUNCTION protect_dynamic_subscription_week() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.weekly_window_start IS DISTINCT FROM OLD.weekly_window_start
        OR NEW.weekly_usage_usd < OLD.weekly_usage_usd)
       AND EXISTS (SELECT 1 FROM dynamic_subscription_policies
                   WHERE subscription_id = OLD.id AND enabled)
       AND COALESCE(current_setting('sub2api.dynamic_quota_reset', true), '')
           <> OLD.id::text THEN
        NEW.weekly_window_start := OLD.weekly_window_start;
        NEW.weekly_usage_usd := OLD.weekly_usage_usd;
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS protect_dynamic_subscription_week ON user_subscriptions;
CREATE TRIGGER protect_dynamic_subscription_week BEFORE UPDATE ON user_subscriptions
    FOR EACH ROW EXECUTE FUNCTION protect_dynamic_subscription_week();

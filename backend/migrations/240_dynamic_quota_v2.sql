-- V2 is the only allocator. Disabled old settings may remain as inert records.
-- Never invent protection values or silently enable/clear those subscriptions.
-- Historical billing/closure/dedup records are not changed by this migration.
ALTER TABLE dynamic_subscription_policies
    DROP COLUMN IF EXISTS increase_threshold_usd,
    ADD COLUMN IF NOT EXISTS floor_limit_usd NUMERIC(20,8)
        CHECK ((NOT enabled OR floor_limit_usd IS NOT NULL)
            AND (floor_limit_usd IS NULL OR (floor_limit_usd > 0 AND floor_limit_usd <= max_limit_usd))),
    ADD COLUMN IF NOT EXISTS cycle_used_usd NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (cycle_used_usd >= 0),
    ADD COLUMN IF NOT EXISTS cycle_started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_change JSONB,
    ADD COLUMN IF NOT EXISTS activation_pending BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE dynamic_quota_requests
    ADD COLUMN IF NOT EXISTS review_required_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS review_reason TEXT,
    ADD COLUMN IF NOT EXISTS review_standard_usd NUMERIC(20,8) CHECK (review_standard_usd >= 0),
    ADD COLUMN IF NOT EXISTS reviewed_by BIGINT,
    ADD COLUMN IF NOT EXISTS review_claim_id UUID,
    ADD COLUMN IF NOT EXISTS reviewed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_dynamic_quota_review
    ON dynamic_quota_requests(account_id,cycle,review_required_at,id)
    WHERE review_required_at IS NOT NULL AND operator_absorbed_at IS NULL AND status IN ('pending','uncertain');

-- A pending opt-in retains native billing until fresh, verified source data is
-- available. Already-active dynamic subscriptions still cannot be timer-reset.
CREATE OR REPLACE FUNCTION protect_dynamic_subscription_week() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.weekly_window_start IS DISTINCT FROM OLD.weekly_window_start
        OR NEW.weekly_usage_usd < OLD.weekly_usage_usd)
       AND EXISTS (SELECT 1 FROM dynamic_subscription_policies
                   WHERE subscription_id = OLD.id AND enabled AND NOT activation_pending)
       AND COALESCE(current_setting('sub2api.dynamic_quota_reset', true), '')
           <> OLD.id::text THEN
        NEW.weekly_window_start := OLD.weekly_window_start;
        NEW.weekly_usage_usd := OLD.weekly_usage_usd;
    END IF;
    RETURN NEW;
END;
$$;

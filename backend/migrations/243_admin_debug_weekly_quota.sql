-- Debug quotas are independent of group amounts. Preserve the existing weekly
-- limit once; never reset usage or overwrite a saved individual limit on replay.
CREATE TABLE IF NOT EXISTS admin_debug_quotas (
    subscription_id BIGINT PRIMARY KEY REFERENCES user_subscriptions(id),
    weekly_limit_usd NUMERIC(20,8) NOT NULL CHECK (weekly_limit_usd BETWEEN 0 AND 1000000000),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    reset_account_id BIGINT REFERENCES accounts(id),
    reset_cycle BIGINT NOT NULL DEFAULT 0 CHECK (reset_cycle >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO admin_debug_quotas(subscription_id,weekly_limit_usd,reset_account_id,reset_cycle)
SELECT us.id,COALESCE(g.weekly_limit_usd,0),gp.account_id,COALESCE((pool.state->>'cycle')::bigint,0)
FROM user_subscriptions us JOIN users u ON u.id=us.user_id JOIN groups g ON g.id=us.group_id
LEFT JOIN dynamic_group_policies gp ON gp.group_id=us.group_id
LEFT JOIN dynamic_quota_pools pool ON pool.account_id=gp.account_id
WHERE us.admin_debug AND u.role='admin' AND us.deleted_at IS NULL
ON CONFLICT(subscription_id) DO NOTHING;

CREATE OR REPLACE FUNCTION protect_dynamic_subscription_week() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.weekly_window_start IS DISTINCT FROM OLD.weekly_window_start
        OR NEW.weekly_usage_usd < OLD.weekly_usage_usd)
       AND (EXISTS (SELECT 1 FROM dynamic_subscription_policies
                    WHERE subscription_id = OLD.id AND enabled AND NOT activation_pending)
         OR (OLD.admin_debug AND EXISTS (SELECT 1 FROM users WHERE id=OLD.user_id AND role='admin')
             AND EXISTS (SELECT 1 FROM admin_debug_quotas WHERE subscription_id=OLD.id AND reset_account_id IS NOT NULL)))
       AND COALESCE(current_setting('sub2api.dynamic_quota_reset', true), '') <> OLD.id::text THEN
        NEW.weekly_window_start := OLD.weekly_window_start;
        NEW.weekly_usage_usd := OLD.weekly_usage_usd;
    END IF;
    RETURN NEW;
END;
$$;

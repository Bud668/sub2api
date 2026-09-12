-- Group settings reuse the V2 per-subscription ledger; no balances or cycles are reset.
CREATE TABLE IF NOT EXISTS dynamic_group_policies (
    group_id BIGINT PRIMARY KEY REFERENCES groups(id),
    account_id BIGINT NOT NULL REFERENCES accounts(id),
    enabled BOOLEAN NOT NULL DEFAULT false,
    weight NUMERIC(12,4) NOT NULL CHECK (weight >= 0.0001 AND weight <= 1000),
    max_limit_usd NUMERIC(20,10) NOT NULL CHECK (max_limit_usd > 0 AND max_limit_usd <= 1000000000),
    floor_limit_usd NUMERIC(20,10) NOT NULL CHECK (floor_limit_usd > 0 AND floor_limit_usd <= max_limit_usd),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE user_subscriptions ADD COLUMN IF NOT EXISTS admin_debug BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE dynamic_subscription_policies ADD COLUMN IF NOT EXISTS group_revision BIGINT;

-- Existing individual settings remain unchanged until an administrator saves the group.

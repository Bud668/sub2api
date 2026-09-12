-- Explicit group configuration activates fixed seats. Do not infer seat counts,
-- enable groups, reset usage, or rewrite any existing billing receipt.
ALTER TABLE dynamic_group_policies
    ADD COLUMN IF NOT EXISTS fixed_slots INTEGER NOT NULL DEFAULT 0
        CHECK (fixed_slots BETWEEN 0 AND 1000);

-- The new form has no operator-supplied floor. Existing values remain for
-- historical settings; the fixed-seat allocator never treats them as money.
DO $$
DECLARE c RECORD;
BEGIN
    FOR c IN
        SELECT conrelid::regclass AS relation, conname
        FROM pg_constraint
        WHERE conrelid IN ('dynamic_group_policies'::regclass, 'dynamic_subscription_policies'::regclass)
          AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%floor_limit_usd%'
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', c.relation, c.conname);
    END LOOP;
END $$;
ALTER TABLE dynamic_group_policies ALTER COLUMN floor_limit_usd DROP NOT NULL;
ALTER TABLE dynamic_group_policies ADD CONSTRAINT dynamic_group_optional_floor
    CHECK (floor_limit_usd IS NULL OR (floor_limit_usd > 0 AND floor_limit_usd <= max_limit_usd));
ALTER TABLE dynamic_subscription_policies ADD CONSTRAINT dynamic_subscription_optional_floor
    CHECK (floor_limit_usd IS NULL OR (floor_limit_usd > 0 AND floor_limit_usd <= max_limit_usd));

-- Every seat exists before its subscriber joins. Ownership is immutable within
-- a source cycle, including revoked/deleted subscriptions and debug conversion.
-- Foreign keys retain its original accounting owner; no copied billing ledger.
CREATE TABLE IF NOT EXISTS dynamic_quota_seats (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES dynamic_quota_pools(account_id),
    cycle BIGINT NOT NULL CHECK (cycle > 0),
    group_id BIGINT NOT NULL REFERENCES groups(id),
    position INTEGER NOT NULL CHECK (position BETWEEN 1 AND 1000),
    subscription_id BIGINT REFERENCES user_subscriptions(id),
    user_id BIGINT REFERENCES users(id),
    allocated_standard_usd NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (allocated_standard_usd >= 0),
    allocated BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((subscription_id IS NULL) = (user_id IS NULL)),
    UNIQUE (account_id, cycle, group_id, position),
    UNIQUE (account_id, cycle, subscription_id),
    UNIQUE (account_id, cycle, group_id, user_id)
);

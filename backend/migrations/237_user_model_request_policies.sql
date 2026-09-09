-- Additive, opt-in migration. Existing group permissions and usage are untouched.
CREATE TABLE IF NOT EXISTS user_model_request_policies (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    rules JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(rules) = 'array'),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_model_request_windows (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    model VARCHAR(200) NOT NULL,
    generation BIGINT NOT NULL DEFAULT 1,
    used BIGINT NOT NULL DEFAULT 0 CHECK (used >= 0),
    resets_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, model)
);

-- A single authority, never an intersection or per-user fallback to groups.
INSERT INTO settings (key, value, updated_at)
VALUES ('user_model_request_policy_enabled', 'false', NOW())
ON CONFLICT (key) DO NOTHING;

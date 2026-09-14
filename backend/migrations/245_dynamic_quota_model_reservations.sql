-- Keep the conservative observed maximum, but do not make a cheap model
-- inherit another model's largest historical request.
WITH maxima AS (
    SELECT account_id, jsonb_object_agg(model, max_cost) AS model_maxima
    FROM (
        SELECT account_id, BTRIM(request_context->>'model') AS model,
               MAX(standard_cost_usd) AS max_cost
        FROM dynamic_quota_requests
        WHERE status = 'settled' AND standard_cost_usd > 0
          AND LENGTH(BTRIM(request_context->>'model')) BETWEEN 1 AND 128
        GROUP BY account_id, BTRIM(request_context->>'model')
    ) requests
    GROUP BY account_id
)
UPDATE dynamic_quota_pools pools
SET state = jsonb_set(state, '{model_max_request_usd}', maxima.model_maxima, true)
FROM maxima
WHERE maxima.account_id = pools.account_id;

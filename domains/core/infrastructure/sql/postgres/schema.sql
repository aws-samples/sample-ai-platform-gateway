-- PostgreSQL Schema for AIPlat Core (Single-Tenant)
-- This schema supports a single organization per deployment

-- Enable required extensions
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_cron";

-- ============================================================================
-- Configs Table
-- ============================================================================

CREATE TABLE IF NOT EXISTS configs (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL CHECK (type IN ('global', 'app', 'team')),
    name TEXT,
    allowed_models JSONB,
    fallback_chain JSONB,
    rate_limit JSONB,
    budget JSONB,
    cache_settings JSONB,
    metadata JSONB,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_configs_type ON configs(type);

COMMENT ON TABLE configs IS 'Application and global configuration settings';
COMMENT ON COLUMN configs.type IS 'Configuration scope: global, app, or team';
COMMENT ON COLUMN configs.rate_limit IS 'JSON: {requests_per_minute, tokens_per_minute}';
COMMENT ON COLUMN configs.budget IS 'JSON: {monthly_limit, action}';

-- ============================================================================
-- API Keys Table
-- ============================================================================

CREATE TABLE IF NOT EXISTS api_keys (
    key_hash TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    app TEXT NOT NULL,
    team TEXT,
    org TEXT,
    status TEXT DEFAULT 'active' CHECK (status IN ('active', 'revoked', 'expired')),
    revoked BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT NOW(),
    expires_at TIMESTAMP,
    last_used TIMESTAMP
);

CREATE INDEX idx_api_keys_app ON api_keys(app);
CREATE INDEX idx_api_keys_revoked ON api_keys(revoked) WHERE NOT revoked;
CREATE INDEX idx_api_keys_status ON api_keys(status) WHERE status = 'active';

COMMENT ON TABLE api_keys IS 'API keys for authentication (SHA256 hashed)';
COMMENT ON COLUMN api_keys.key_hash IS 'SHA256 hash of the actual API key';
COMMENT ON COLUMN api_keys.status IS 'Key status: active, revoked, or expired';

-- ============================================================================
-- Usage Records Table (Partitioned by Month)
-- ============================================================================

CREATE TABLE IF NOT EXISTS usage_records (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    timestamp TIMESTAMP NOT NULL,
    app TEXT NOT NULL,
    feature TEXT,
    model TEXT NOT NULL,
    provider TEXT NOT NULL,
    prompt_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    total_tokens INTEGER NOT NULL,
    latency_ms INTEGER NOT NULL,
    cost NUMERIC(10, 6) NOT NULL,
    cache_hit BOOLEAN DEFAULT FALSE,
    request_id TEXT
) PARTITION BY RANGE (timestamp);

-- Create indexes on the parent table (inherited by partitions)
CREATE INDEX idx_usage_records_timestamp ON usage_records(timestamp);
CREATE INDEX idx_usage_records_app ON usage_records(app, timestamp);
CREATE INDEX idx_usage_records_model ON usage_records(model, timestamp);
CREATE INDEX idx_usage_records_cache_hit ON usage_records(cache_hit) WHERE cache_hit = TRUE;

COMMENT ON TABLE usage_records IS 'Time-series usage data for LLM API calls';

-- Create initial partitions (2026-08 through 2026-12)
CREATE TABLE IF NOT EXISTS usage_records_2026_08 PARTITION OF usage_records
    FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');

CREATE TABLE IF NOT EXISTS usage_records_2026_09 PARTITION OF usage_records
    FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');

CREATE TABLE IF NOT EXISTS usage_records_2026_10 PARTITION OF usage_records
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');

CREATE TABLE IF NOT EXISTS usage_records_2026_11 PARTITION OF usage_records
    FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');

CREATE TABLE IF NOT EXISTS usage_records_2026_12 PARTITION OF usage_records
    FOR VALUES FROM ('2026-12-01') TO ('2027-01-01');

-- ============================================================================
-- Savings Records Table
-- ============================================================================

CREATE TABLE IF NOT EXISTS savings_records (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    period TEXT NOT NULL,  -- "2026-08"
    app TEXT NOT NULL,
    category TEXT NOT NULL CHECK (category IN ('cache', 'cheapest', 'fallback', 'arbitrage')),
    amount NUMERIC(10, 2) NOT NULL,
    baseline NUMERIC(10, 2) NOT NULL,
    description TEXT,
    confidence NUMERIC(3, 2) CHECK (confidence >= 0 AND confidence <= 1),
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_savings_records_period ON savings_records(period);
CREATE INDEX idx_savings_records_app ON savings_records(app, period);
CREATE INDEX idx_savings_records_category ON savings_records(category, period);

COMMENT ON TABLE savings_records IS 'Cost savings achieved through optimization strategies';
COMMENT ON COLUMN savings_records.period IS 'Month in YYYY-MM format';
COMMENT ON COLUMN savings_records.category IS 'Type of savings: cache, cheapest, fallback, or arbitrage';

-- ============================================================================
-- Audit Events Table (Partitioned by Month)
-- ============================================================================

CREATE TABLE IF NOT EXISTS audit_events (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    timestamp TIMESTAMP DEFAULT NOW(),
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    resource TEXT NOT NULL,
    details JSONB,
    ip_address INET,
    user_agent TEXT
) PARTITION BY RANGE (timestamp);

CREATE INDEX idx_audit_events_timestamp ON audit_events(timestamp);
CREATE INDEX idx_audit_events_actor ON audit_events(actor, timestamp);
CREATE INDEX idx_audit_events_action ON audit_events(action, timestamp);
CREATE INDEX idx_audit_events_resource ON audit_events(resource);

COMMENT ON TABLE audit_events IS 'Audit log for security and compliance';
COMMENT ON COLUMN audit_events.actor IS 'User email or "system"';
COMMENT ON COLUMN audit_events.action IS 'Action performed (e.g., key.create, config.update)';

-- Create initial partitions
CREATE TABLE IF NOT EXISTS audit_events_2026_08 PARTITION OF audit_events
    FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');

CREATE TABLE IF NOT EXISTS audit_events_2026_09 PARTITION OF audit_events
    FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');

CREATE TABLE IF NOT EXISTS audit_events_2026_10 PARTITION OF audit_events
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');

CREATE TABLE IF NOT EXISTS audit_events_2026_11 PARTITION OF audit_events
    FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');

CREATE TABLE IF NOT EXISTS audit_events_2026_12 PARTITION OF audit_events
    FOR VALUES FROM ('2026-12-01') TO ('2027-01-01');

-- ============================================================================
-- Cache Entries Table
-- ============================================================================

CREATE TABLE IF NOT EXISTS cache_entries (
    key TEXT PRIMARY KEY,
    value BYTEA NOT NULL,
    expires_at TIMESTAMP NOT NULL
);

CREATE INDEX idx_cache_expires_at ON cache_entries(expires_at);

COMMENT ON TABLE cache_entries IS 'LLM response cache (consider Redis for production)';
COMMENT ON COLUMN cache_entries.expires_at IS 'TTL for automatic expiration';

-- ============================================================================
-- Materialized Views for Analytics
-- ============================================================================

-- Daily usage aggregated by app and model
CREATE MATERIALIZED VIEW IF NOT EXISTS usage_daily_by_app AS
SELECT
    date_trunc('day', timestamp) AS day,
    app,
    model,
    provider,
    SUM(total_tokens) AS total_tokens,
    SUM(prompt_tokens) AS total_prompt_tokens,
    SUM(output_tokens) AS total_output_tokens,
    SUM(cost) AS total_cost,
    COUNT(*) AS request_count,
    AVG(latency_ms) AS avg_latency_ms,
    COUNT(*) FILTER (WHERE cache_hit = TRUE) AS cache_hits,
    ROUND(100.0 * COUNT(*) FILTER (WHERE cache_hit = TRUE) / COUNT(*), 2) AS cache_hit_rate
FROM usage_records
GROUP BY 1, 2, 3, 4;

CREATE UNIQUE INDEX ON usage_daily_by_app (day, app, model, provider);

COMMENT ON MATERIALIZED VIEW usage_daily_by_app IS 'Pre-aggregated daily usage stats';

-- Hourly usage aggregated by app (for real-time monitoring)
CREATE MATERIALIZED VIEW IF NOT EXISTS usage_hourly_by_app AS
SELECT
    date_trunc('hour', timestamp) AS hour,
    app,
    model,
    SUM(total_tokens) AS total_tokens,
    SUM(cost) AS total_cost,
    COUNT(*) AS request_count,
    AVG(latency_ms) AS avg_latency_ms
FROM usage_records
WHERE timestamp >= NOW() - INTERVAL '7 days'
GROUP BY 1, 2, 3;

CREATE UNIQUE INDEX ON usage_hourly_by_app (hour, app, model);

-- ============================================================================
-- Functions and Triggers
-- ============================================================================

-- Trigger to update 'updated_at' timestamp on configs
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ language 'plpgsql';

CREATE TRIGGER update_configs_updated_at BEFORE UPDATE ON configs
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Function to clean up expired cache entries
CREATE OR REPLACE FUNCTION cleanup_expired_cache()
RETURNS void AS $$
BEGIN
    DELETE FROM cache_entries WHERE expires_at < NOW();
END;
$$ LANGUAGE plpgsql;

-- Schedule cache cleanup every hour (requires pg_cron extension)
-- SELECT cron.schedule('cache-cleanup', '0 * * * *', 'SELECT cleanup_expired_cache();');

-- Function to refresh materialized views
CREATE OR REPLACE FUNCTION refresh_usage_views()
RETURNS void AS $$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY usage_daily_by_app;
    REFRESH MATERIALIZED VIEW CONCURRENTLY usage_hourly_by_app;
END;
$$ LANGUAGE plpgsql;

-- Schedule view refresh every hour
-- SELECT cron.schedule('refresh-usage-views', '5 * * * *', 'SELECT refresh_usage_views();');

-- ============================================================================
-- Partition Management Functions
-- ============================================================================

-- Function to create next month's partition
CREATE OR REPLACE FUNCTION create_next_partition(table_name TEXT, next_month DATE)
RETURNS void AS $$
DECLARE
    partition_name TEXT;
    start_date TEXT;
    end_date TEXT;
BEGIN
    partition_name := table_name || '_' || to_char(next_month, 'YYYY_MM');
    start_date := to_char(next_month, 'YYYY-MM-DD');
    end_date := to_char(next_month + INTERVAL '1 month', 'YYYY-MM-DD');

    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
        partition_name,
        table_name,
        start_date,
        end_date
    );
END;
$$ LANGUAGE plpgsql;

-- Automated partition creation (run monthly)
-- SELECT cron.schedule('create-partitions', '0 0 1 * *',
--     'SELECT create_next_partition(''usage_records'', date_trunc(''month'', NOW() + INTERVAL ''2 months''));
--      SELECT create_next_partition(''audit_events'', date_trunc(''month'', NOW() + INTERVAL ''2 months''));'
-- );

-- ============================================================================
-- Sample Data (for testing)
-- ============================================================================

-- Insert global config
INSERT INTO configs (id, type, name, allowed_models, rate_limit, created_at, updated_at)
VALUES (
    'global',
    'global',
    'Global Configuration',
    '["gpt-4", "claude-3-opus", "claude-3-sonnet", "claude-3-haiku"]'::jsonb,
    '{"requests_per_minute": 100, "tokens_per_minute": 100000}'::jsonb,
    NOW(),
    NOW()
) ON CONFLICT (id) DO NOTHING;

-- Grant permissions (adjust as needed)
-- GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO aiplat_app;
-- GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO aiplat_app;

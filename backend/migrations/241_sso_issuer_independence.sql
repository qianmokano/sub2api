-- Preserve legacy rows and stable oidc identities. Runtime settings complete
-- missing values once, under a row lock on this migration marker.
INSERT INTO settings (key, value, updated_at)
SELECT 'sso_issuer_url', value, NOW() FROM settings
WHERE key = 'oidc_connect_issuer_url'
ON CONFLICT (key) DO NOTHING;

INSERT INTO settings (key, value, updated_at)
VALUES ('migration_sso_auth_v1', 'pending', NOW())
ON CONFLICT (key) DO NOTHING;

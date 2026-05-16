DROP TABLE IF EXISTS install_token;

ALTER TABLE daemon_token
    DROP COLUMN IF EXISTS install_source,
    DROP COLUMN IF EXISTS created_by_user_id,
    DROP COLUMN IF EXISTS revoked_at;
